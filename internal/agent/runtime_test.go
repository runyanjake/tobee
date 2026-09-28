package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/runyanjake/tobee/internal/delivery"
	"github.com/runyanjake/tobee/internal/event"
	"github.com/runyanjake/tobee/internal/llm"
	"github.com/runyanjake/tobee/internal/mcphost"
	"github.com/runyanjake/tobee/internal/mcpserver"
	userserver "github.com/runyanjake/tobee/internal/servers/user"
	"github.com/runyanjake/tobee/internal/taskqueue"
	"github.com/runyanjake/tobee/internal/telemetry"
)

// scriptedLLM answers each Decide with the next scripted call and records what it was sent.
type scriptedLLM struct {
	mu       sync.Mutex
	calls    []call
	requests [][]llm.Message
	offered  [][]string // tool names offered per call
}

// call.invalid simulates output that could not be read as a tool call.
type call struct {
	name, args, reasoning string
	invalid               bool
}

func (s *scriptedLLM) Decide(_ context.Context, msgs []llm.Message, tools []llm.ToolSpec) (*llm.Decision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, append([]llm.Message(nil), msgs...))
	names := make([]string, len(tools))
	for i, t := range tools {
		names[i] = t.Name
	}
	s.offered = append(s.offered, names)
	if len(s.calls) == 0 {
		return nil, fmt.Errorf("script exhausted")
	}
	c := s.calls[0]
	s.calls = s.calls[1:]
	if c.invalid {
		return &llm.Decision{Raw: "some prose"}, fmt.Errorf("%w: prose", llm.ErrInvalidDecision)
	}
	offered := false
	for _, t := range tools {
		offered = offered || t.Name == c.name
	}
	if !offered {
		return nil, fmt.Errorf("scripted %s was not offered", c.name)
	}
	return &llm.Decision{
		Call: llm.ToolCall{
			ID: fmt.Sprintf("call-%d", len(s.requests)), Type: "function",
			Function: llm.FunctionCall{Name: c.name, Arguments: c.args},
		},
		Reasoning: c.reasoning,
	}, nil
}

func (s *scriptedLLM) script(calls ...call) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, calls...)
}

type chat struct {
	mu   sync.Mutex
	sent []string
}

func (c *chat) Send(_ context.Context, _ event.Address, text string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, text)
	return fmt.Sprintf("m%d", len(c.sent)), nil
}

type harness struct {
	out   *delivery.Router
	loop  *Loop
	llm   *scriptedLLM
	chat  *chat
	queue *taskqueue.Queue
	rt    *Runtime
}

func newHarness(t *testing.T, extra ...*mcpserver.Server) *harness {
	t.Helper()
	fake := &scriptedLLM{}

	out := delivery.NewRouter()
	ch := &chat{}
	out.Register("chat", ch)

	host := mcphost.New()
	t.Cleanup(host.Close)
	for _, s := range append([]*mcpserver.Server{userserver.New("", out)}, extra...) {
		if err := host.ConnectInProcess(context.Background(), s); err != nil {
			t.Fatal(err)
		}
	}

	states, err := LoadStateTemplates(filepath.Join("..", "..", "prompts", "state"))
	if err != nil {
		t.Fatal(err)
	}
	loop := NewLoop(fake, host, states, out, 12)
	q, err := taskqueue.Open(t.TempDir(), 10)
	if err != nil {
		t.Fatal(err)
	}
	rt := NewRuntime(q, &ContextBuilder{Host: host}, out, loop, Config{TurnBudget: 10 * time.Second})
	return &harness{llm: fake, chat: ch, queue: q, rt: rt, out: out, loop: loop}
}

func (h *harness) runNext(t *testing.T, ev event.Event) *taskqueue.Task {
	t.Helper()
	if err := h.queue.Enqueue(ev); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	task, err := h.queue.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	h.rt.run(context.Background(), task)
	return task
}

func chatEvent(id, content, inReplyTo string) event.Event {
	return event.Event{
		ID: id, Source: "chat", Kind: event.KindMessage,
		Actor:     event.Actor{ID: "u1", Name: "jake"},
		Origin:    event.Address{Connector: "chat", Channel: "c1"},
		InReplyTo: inReplyTo, Content: content,
	}
}

// A clarifying question parks the task; the answer resumes it with the
// original request, the question, and the answer as ordinary chat (D-036).
func TestAskParksAndAnswerResumes(t *testing.T) {
	h := newHarness(t)

	h.llm.script(call{name: "user_ask", args: `{"question":"Which file?"}`})
	h.runNext(t, chatEvent("e1", "rename the file", ""))

	if len(h.chat.sent) != 1 || h.chat.sent[0] != "Which file?" {
		t.Fatalf("sent = %q, want only the question", h.chat.sent)
	}
	if _, parked := h.queue.Stats(); parked != 1 {
		t.Fatalf("parked = %d, want 1", parked)
	}

	h.llm.script(call{name: "reply", args: `{"spoken":"Renamed notes.md."}`})
	task := h.runNext(t, chatEvent("e2", "notes.md", "m1"))

	if task.Resume == nil {
		t.Fatal("answer did not resume the parked task")
	}
	if last := h.chat.sent[len(h.chat.sent)-1]; last != "Renamed notes.md." {
		t.Fatalf("reply = %q", last)
	}

	msgs := h.llm.requests[len(h.llm.requests)-1]
	var convo []string
	for _, m := range msgs[1:] { // skip system
		if strings.HasPrefix(m.Content, "<phase") {
			break
		}
		convo = append(convo, string(m.Role)+":"+m.Content)
	}
	want := []string{"user:rename the file", "assistant:Which file?", "user:notes.md"}
	if strings.Join(convo, "|") != strings.Join(want, "|") {
		t.Fatalf("resumed conversation = %q, want %q", convo, want)
	}
}

// A greeting is one model call and one message: no plan, no second reply.
func TestGreetingIsOneCallOneMessage(t *testing.T) {
	h := newHarness(t)
	h.llm.script(call{name: "reply", args: `{"spoken":"Hey!"}`})
	h.runNext(t, chatEvent("e1", "Hey @TOBEE", ""))

	if len(h.llm.requests) != 1 {
		t.Fatalf("model calls = %d, want 1", len(h.llm.requests))
	}
	if len(h.chat.sent) != 1 || h.chat.sent[0] != "Hey!" {
		t.Fatalf("sent = %q", h.chat.sent)
	}
}

// Verbatim tool output reaches the user even when the model's own words
// say something else (D-030); the lookup's reply needs no synthesis phase.
func TestVerbatimToolOutputIsDelivered(t *testing.T) {
	h := newHarness(t, statusServer())
	h.llm.script(
		call{name: "status_summary", args: `{}`},
		call{name: "reply", args: `{"spoken":"Here you go."}`},
	)
	h.runNext(t, chatEvent("e1", "how are things?", ""))

	if got := h.chat.sent; len(got) != 1 || got[0] != "Here you go.\n\nEverything quiet." {
		t.Fatalf("sent = %q", got)
	}
	// The model is told the output is already shown, so it doesn't restate it.
	result := h.llm.requests[1][len(h.llm.requests[1])-1]
	if result.Role != llm.RoleTool || !strings.Contains(result.Content, "Don't repeat it") {
		t.Fatalf("verbatim result = %+v", result)
	}
}

// A plan is shown only for several steps and only where it can be edited.
func TestPlanShownOnlyWhenMultiStepAndEditable(t *testing.T) {
	h := newHarness(t)
	ed := &editableChat{}
	h.out.Register("chat", ed)
	h.llm.script(
		call{name: "plan", args: `{"goal":"One thing.","steps":[{"title":"Do it","status":"active"}]}`},
		call{name: "plan", args: `{"goal":"Two things.","steps":[{"title":"A","status":"active"},{"title":"B","status":"pending"}]}`},
		call{name: "plan", args: `{"goal":"Two things.","steps":[{"title":"A","status":"done"},{"title":"B","status":"done"}]}`},
		call{name: "reply", args: `{"spoken":"Both done."}`},
	)
	h.runNext(t, chatEvent("e1", "do A then B", ""))

	if len(ed.sent) != 2 || !strings.Contains(ed.sent[0], "⏳ 2. B") || ed.sent[1] != "Both done." {
		t.Fatalf("sent = %q", ed.sent)
	}
	if len(ed.edits) != 1 || !strings.Contains(ed.edits[0], "✅ 2. B") {
		t.Fatalf("edits = %q", ed.edits)
	}
}

// Out of steps, the loop forces one reply-only call instead of failing silently.
func TestBudgetForcesReply(t *testing.T) {
	h := newHarness(t, statusServer())
	h.loop.maxSteps = 2
	h.llm.script(
		call{name: "status_summary", args: `{}`},
		call{name: "status_summary", args: `{}`},
		call{name: "reply", args: `{"spoken":"Partly done."}`},
	)
	h.runNext(t, chatEvent("e1", "loop forever", ""))

	last := h.llm.offered[len(h.llm.offered)-1]
	if len(last) != 1 || last[0] != "reply" {
		t.Fatalf("final call offered %v, want only reply", last)
	}
	if got := h.chat.sent; len(got) != 1 || !strings.HasPrefix(got[0], "Partly done.") {
		t.Fatalf("sent = %q", got)
	}
}

// Unreadable output is retried once with a nudge and never kept in the
// transcript, so later calls can't continue it (D-041).
func TestInvalidOutputIsNotKept(t *testing.T) {
	h := newHarness(t)
	h.llm.script(
		call{invalid: true},
		call{name: "reply", args: `{"spoken":"Hey."}`},
	)
	h.runNext(t, chatEvent("e1", "hey", ""))

	if got := h.chat.sent; len(got) != 1 || got[0] != "Hey." {
		t.Fatalf("sent = %q", got)
	}
	retry := h.llm.requests[1]
	for _, m := range retry {
		if strings.Contains(m.Content, "some prose") {
			t.Fatal("invalid output was kept in the conversation")
		}
	}
	if last := retry[len(retry)-1]; last.Content != invalidNudge {
		t.Fatalf("retry did not end with the nudge: %+v", last)
	}
}

// The reasoning chain is reconstructable from the log alone, all tagged
// with the task (D-040).
func TestTurnLogsTheReasoningChain(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(telemetry.NewHandler(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))))
	defer slog.SetDefault(prev)

	h := newHarness(t, statusServer())
	h.llm.script(
		call{name: "plan", args: `{"goal":"Report status.","steps":[{"title":"Check","status":"active"},{"title":"Report","status":"pending"}]}`,
			reasoning: "Two steps."},
		call{name: "status_summary", args: `{}`},
		call{name: "reply", args: `{"spoken":"Here you go."}`},
	)
	task := h.runNext(t, chatEvent("e1", "how are things?", ""))

	var chain []string
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("bad log line %q: %v", line, err)
		}
		cat, _ := rec["cat"].(string)
		if cat == "" {
			t.Fatalf("record without cat: %v", rec)
		}
		if cat == "system" || cat == "llm" {
			continue
		}
		if rec["task"] != task.ID {
			t.Fatalf("chain record without task id: %v", rec)
		}
		chain = append(chain, cat+" "+rec["msg"].(string))
	}
	want := []string{
		"input agent: input",
		"thinking agent: reasoning",
		"thinking agent: plan",
		"action agent: tool call",
		"action agent: tool result",
		"output agent: output",
	}
	if strings.Join(chain, "\n") != strings.Join(want, "\n") {
		t.Fatalf("chain =\n%s\nwant\n%s", strings.Join(chain, "\n"), strings.Join(want, "\n"))
	}
}

func statusServer() *mcpserver.Server {
	s := mcpserver.New("status", "")
	s.Add(mcpserver.Tool{Name: "summary", Verbatim: true, ReadOnly: true, Handler: func(context.Context, json.RawMessage) (string, error) {
		return "Everything quiet.", nil
	}})
	return s
}

// editableChat is a chat channel that supports edits, like Discord.
type editableChat struct {
	chat
	edits []string
}

func (c *editableChat) Edit(_ context.Context, _ event.Address, _, text string) error {
	c.edits = append(c.edits, text)
	return nil
}
