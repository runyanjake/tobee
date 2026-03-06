package core

import (
	"context"
	"log"
	"tobee/internal/ai"
	"tobee/internal/state"
)

// Loop reads Messages from the queue and processes them one at a time.
// Each integration registers a "reply:<name>" action to handle outbound responses.
type Loop struct {
	queue    *Queue
	state    *state.State
	ai       *ai.Client
	chatTmpl *ai.ChatTemplate
}

func NewLoop(q *Queue, s *state.State, aiClient *ai.Client, chatTmpl *ai.ChatTemplate) *Loop {
	return &Loop{queue: q, state: s, ai: aiClient, chatTmpl: chatTmpl}
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
	log.Printf("core: processing message integration=%s session=%s", msg.Integration, msg.SessionID)
	ctx = WithMessage(ctx, msg)

	rendered, err := l.chatTmpl.Render(ai.ChatData{
		UserRequest: msg.Content,
		Tools:       l.toolDefs(),
	})
	if err != nil {
		log.Printf("core: rendering chat template: %v", err)
		return
	}

	conv := ai.NewConversation()
	if sys := l.state.BuildSystemPrompt(); sys != "" {
		conv.System(sys)
	}
	conv.User(rendered)

	raw, err := l.ai.ChatStream(ctx, conv.Messages(), nil)
	if err != nil {
		log.Printf("core: AI stream failed: %v", err)
		return
	}

	resp, parseErr := ai.ParseResponse(raw)
	if parseErr != nil {
		log.Printf("core: AI response not valid JSON, using raw: %v", parseErr)
		l.reply(ctx, msg, raw)
		return
	}

	if resp.Response != "" {
		l.reply(ctx, msg, resp.Response)
	}

	for _, tc := range resp.ToolCalls {
		fn, ok := l.state.GetAction(tc.Name)
		if !ok {
			log.Printf("core: tool call to unknown action %q", tc.Name)
			continue
		}
		result, err := fn(ctx, l.state, tc.Args)
		if err != nil {
			log.Printf("core: tool call %q error: %v", tc.Name, err)
		} else {
			log.Printf("core: tool call %q result: %s", tc.Name, result)
		}
	}
}

// reply dispatches the response text to the integration that sent the original message.
// Each integration registers its handler under "reply:<integration name>".
func (l *Loop) reply(ctx context.Context, msg Message, text string) {
	fn, ok := l.state.GetAction("reply:" + msg.Integration)
	if !ok {
		log.Printf("core: no reply handler for integration %q", msg.Integration)
		return
	}
	if _, err := fn(ctx, l.state, map[string]string{"message": text}); err != nil {
		log.Printf("core: reply error: %v", err)
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
