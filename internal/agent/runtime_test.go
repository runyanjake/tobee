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
	strategy := NewPlanExecute(NewPlanner(fake, states), NewExecutor(fake, host, states, 4, 12),
		NewSynthesizer(fake, states), out)
	q, err := taskqueue.Open(t.TempDir(), 10)
	if err != nil {
		t.Fatal(err)
	}
	rt := NewRuntime(q, &ContextBuilder{Host: host}, out, strategy, Config{TurnBudget: 10 * time.Second})
	return &harness{llm: fake, chat: ch, queue: q, rt: rt}
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

	h.llm.script(
		call{name: "plan_commit", args: `{"goal":"Rename a file.","steps":[{"intent":"Find out which file the user means."}]}`},
		call{name: "user_ask", args: `{"question":"Which file?"}`},
	)
	h.runNext(t, chatEvent("e1", "rename the file", ""))

	if len(h.chat.sent) != 1 || h.chat.sent[0] != "Which file?" {
		t.Fatalf("sent = %q, want only the question", h.chat.sent)
	}
	if _, parked := h.queue.Stats(); parked != 1 {
		t.Fatalf("parked = %d, want 1", parked)
	}

	h.llm.script(call{name: "plan_commit", args: `{"goal":"Confirm.","steps":[],"direct_reply":"Renamed notes.md."}`})
	task := h.runNext(t, chatEvent("e2", "notes.md", "m1"))

	if task.Resume == nil {
		t.Fatal("answer did not resume the parked task")
	}
	if last := h.chat.sent[len(h.chat.sent)-1]; last != "Renamed notes.md." {
		t.Fatalf("reply = %q", last)
	}

	// The resumed planner saw request → question → answer, untagged.
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

// Verbatim tool output reaches the user even when the model says otherwise (D-030).
func TestVerbatimToolOutputIsDelivered(t *testing.T) {
	status := mcpserver.New("status", "")
	status.Add(mcpserver.Tool{Name: "summary", Verbatim: true, Handler: func(context.Context, json.RawMessage) (string, error) {
		return "Everything quiet.", nil
	}})
	h := newHarness(t, status)

	h.llm.script(
		call{name: "plan_commit", args: `{"goal":"Report status.","steps":[{"intent":"Report tobee's status."}]}`},
		call{name: "status_summary", args: `{}`},
		call{name: "step_finish", args: `{"result":"Status reported.","finished":true}`},
		call{name: "reply_commit", args: `{"spoken":"Here you go."}`},
	)
	h.runNext(t, chatEvent("e1", "how are things?", ""))

	if got := h.chat.sent[len(h.chat.sent)-1]; got != "Here you go.\n\nEverything quiet." {
		t.Fatalf("reply = %q", got)
	}
	// The chat channel cannot edit, so no plan checklist was announced.
	if len(h.chat.sent) != 1 {
		t.Fatalf("sent %d messages, want 1: %q", len(h.chat.sent), h.chat.sent)
	}
}

// The reasoning chain is reconstructable, in order, from task-tagged logs alone (D-040).
func TestTurnLogsTheReasoningChain(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(telemetry.NewHandler(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))))
	defer slog.SetDefault(prev)

	status := mcpserver.New("status", "")
	status.Add(mcpserver.Tool{Name: "summary", Verbatim: true, Handler: func(context.Context, json.RawMessage) (string, error) {
		return "Everything quiet.", nil
	}})
	h := newHarness(t, status)
	h.llm.script(
		call{name: "plan_commit", args: `{"goal":"Report status.","steps":[{"intent":"Report tobee's status."}]}`,
			reasoning: "The user wants status; one step."},
		call{name: "status_summary", args: `{}`},
		call{name: "step_finish", args: `{"result":"Status reported.","finished":true}`},
		call{name: "reply_commit", args: `{"spoken":"Here you go."}`},
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
		"thinking agent: step begin",
		"action agent: tool call",
		"action agent: tool result",
		"thinking agent: step result",
		"output agent: output",
	}
	if strings.Join(chain, "\n") != strings.Join(want, "\n") {
		t.Fatalf("chain =\n%s\nwant\n%s", strings.Join(chain, "\n"), strings.Join(want, "\n"))
	}
}

// Unreadable output is retried once with a nudge and never kept in the
// transcript, so later phases can't continue it (D-041).
func TestInvalidOutputIsNotKept(t *testing.T) {
	h := newHarness(t)
	h.llm.script(
		call{invalid: true},
		call{name: "plan_commit", args: `{"goal":"Greet.","steps":[],"direct_reply":"Hey."}`},
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
	if last := retry[len(retry)-1]; last.Content != plannerNudge {
		t.Fatalf("retry did not end with the nudge: %+v", last)
	}
}
