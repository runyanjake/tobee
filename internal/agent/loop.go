package agent

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"
	"tobee/internal/ai"
)

const (
	maxRetries       = 3
	maxProcessingAge = 10 * time.Minute
)

// retryPrompt is appended to the conversation when the model returns a
// response that cannot be parsed, to steer it back to the required format.
const retryPrompt = `SYSTEM: Your previous response could not be parsed as valid JSON. ` +
	`You MUST respond with only a JSON object containing exactly these two fields: ` +
	`{"response": "<your reply or empty string>", "tool_calls": []}. ` +
	`No text before or after the JSON. No markdown.`

// MemorySearcher retrieves relevant memories as formatted strings for a given query.
// Implemented by integrations/memory.Store via its SearchStrings method.
type MemorySearcher interface {
	SearchStrings(ctx context.Context, query string, limit int) ([]string, error)
}

// phase represents a step in the agent processing loop.
type phase int

const (
	phasePrepareContext phase = iota // assemble conversation from history, memory, and user input
	phaseExecute                     // stream AI response into buffer
	phaseCheck                       // validate result; dispatch reply and tool calls
)

func (p phase) String() string {
	return [...]string{"PrepareContext", "Execute", "Check"}[p]
}

// processState carries all mutable data through the agent loop phases.
type processState struct {
	phase     phase
	conv      *ai.Conversation // built during PrepareContext
	raw       string           // filled during Execute
	resp      *ai.AIResponse   // filled during Check
	retries   int
	startedAt time.Time
}

// Loop reads Messages from the queue and processes them one at a time through
// the PrepareContext → Execute → Check state machine.
type Loop struct {
	queue    *Queue
	state    *State
	ai       *ai.Client
	chatTmpl *ai.ChatTemplate
	memory   MemorySearcher // optional; nil disables memory recall

	historyMu sync.Mutex
	history   map[string]*ai.Conversation // keyed by "integration:sessionID"
}

func NewLoop(q *Queue, s *State, aiClient *ai.Client, chatTmpl *ai.ChatTemplate, mem MemorySearcher) *Loop {
	return &Loop{
		queue:    q,
		state:    s,
		ai:       aiClient,
		chatTmpl: chatTmpl,
		memory:   mem,
		history:  make(map[string]*ai.Conversation),
	}
}

// Start drains the queue in a background goroutine, processing one message at a time.
func (l *Loop) Start(ctx context.Context) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case msg := <-l.queue.C():
				l.process(ctx, msg)
			}
		}
	}()
}

func (l *Loop) process(ctx context.Context, msg Message) {
	log.Printf("agent[%s]: message integration=%s session=%s", phasePrepareContext, msg.Integration, msg.SessionID)
	ctx = WithMessage(ctx, msg)
	ps := &processState{phase: phasePrepareContext, startedAt: time.Now()}

	for {
		var err error
		switch ps.phase {
		case phasePrepareContext:
			err = l.doPrepareContext(ctx, msg, ps)
		case phaseExecute:
			err = l.doExecute(ctx, ps)
		case phaseCheck:
			done, checkErr := l.doCheck(ctx, msg, ps)
			if checkErr != nil {
				log.Printf("agent[Check]: %v", checkErr)
			}
			if done {
				return
			}
			continue
		}
		if err != nil {
			log.Printf("agent[%s]: %v", ps.phase, err)
			return
		}
	}
}

// doPrepareContext assembles the conversation:
//  1. System prompt on the first turn of a session
//  2. Prior session history (plain user/assistant pairs — no template noise)
//  3. Relevant memories injected into the current user message
//  4. Rendered user message with available tools
func (l *Loop) doPrepareContext(ctx context.Context, msg Message, ps *processState) error {
	sessionKey := msg.Integration + ":" + msg.SessionID

	l.historyMu.Lock()
	hist, exists := l.history[sessionKey]
	if !exists {
		hist = ai.NewConversation()
		l.history[sessionKey] = hist
	}
	l.historyMu.Unlock()

	conv := ai.NewConversation()
	if !exists {
		if sys := l.state.BuildSystemPrompt(); sys != "" {
			conv.System(sys)
		}
	}

	for _, m := range hist.Messages() {
		switch m.Role {
		case "user":
			conv.User(m.Content)
		case "assistant":
			conv.Assistant(m.Content)
		}
	}

	var memories []string
	if l.memory != nil {
		results, err := l.memory.SearchStrings(ctx, msg.Content, 5)
		if err != nil {
			log.Printf("agent[PrepareContext]: memory search failed: %v", err)
		} else {
			memories = results
		}
	}

	rendered, err := l.chatTmpl.Render(ai.ChatData{
		UserRequest: msg.Content,
		Tools:       l.toolDefs(),
		Memories:    memories,
	})
	if err != nil {
		return fmt.Errorf("rendering chat template: %w", err)
	}

	conv.User(rendered)
	ps.conv = conv
	ps.phase = phaseExecute
	return nil
}

// doExecute streams the AI response into ps.raw.
func (l *Loop) doExecute(ctx context.Context, ps *processState) error {
	log.Printf("agent[Execute]: sending %d messages to AI", len(ps.conv.Messages()))
	raw, err := l.ai.ChatStream(ctx, ps.conv.Messages(), nil)
	if err != nil {
		return fmt.Errorf("AI stream: %w", err)
	}
	ps.raw = raw
	ps.phase = phaseCheck
	return nil
}

// doCheck validates the response and, if valid, persists history and dispatches.
// On a bad response it retries up to maxRetries times or maxProcessingAge,
// injecting a correction prompt so the model knows what went wrong.
// Returns done=true when the message has been fully handled.
func (l *Loop) doCheck(ctx context.Context, msg Message, ps *processState) (done bool, err error) {
	resp, parseErr := ai.ParseResponse(ps.raw)
	invalid := parseErr != nil || (resp != nil && resp.Response == "" && len(resp.ToolCalls) == 0)

	if invalid {
		if ps.retries < maxRetries && time.Since(ps.startedAt) < maxProcessingAge {
			ps.retries++
			log.Printf("agent[Check]: invalid response (attempt %d/%d): %v", ps.retries, maxRetries, parseErr)
			ps.conv.Assistant(ps.raw)
			ps.conv.User(retryPrompt)
			ps.phase = phaseExecute
			return false, nil
		}
		log.Printf("agent[Check]: giving up after %d retries, sending raw response", ps.retries)
		if ps.raw != "" {
			l.reply(ctx, msg, ps.raw)
			l.appendHistory(msg, msg.Content, ps.raw)
		}
		return true, fmt.Errorf("invalid response after %d retries", ps.retries)
	}

	ps.resp = resp
	l.appendHistory(msg, msg.Content, resp.Response)

	if resp.Response != "" {
		l.reply(ctx, msg, resp.Response)
	}

	for _, tc := range resp.ToolCalls {
		fn, ok := l.state.GetAction(tc.Name)
		if !ok {
			log.Printf("agent[Check]: unknown tool %q", tc.Name)
			continue
		}
		result, callErr := fn(ctx, l.state, tc.Args)
		if callErr != nil {
			log.Printf("agent[Check]: tool %q error: %v", tc.Name, callErr)
		} else {
			log.Printf("agent[Check]: tool %q result: %s", tc.Name, result)
		}
	}

	return true, nil
}

func (l *Loop) appendHistory(msg Message, userContent, assistantContent string) {
	sessionKey := msg.Integration + ":" + msg.SessionID
	l.historyMu.Lock()
	defer l.historyMu.Unlock()
	hist := l.history[sessionKey]
	hist.User(userContent)
	hist.Assistant(assistantContent)
}

func (l *Loop) reply(ctx context.Context, msg Message, text string) {
	fn, ok := l.state.GetAction("reply:" + msg.Integration)
	if !ok {
		log.Printf("agent: no reply handler for integration %q", msg.Integration)
		return
	}
	if _, err := fn(ctx, l.state, map[string]string{"message": text}); err != nil {
		log.Printf("agent: reply error: %v", err)
	}
}

func (l *Loop) toolDefs() []ai.ToolDef {
	names := l.state.ListActions()
	defs := make([]ai.ToolDef, 0, len(names))
	for _, name := range names {
		defs = append(defs, ai.ToolDef{Name: name})
	}
	return defs
}
