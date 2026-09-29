package session

import (
	"strings"
	"testing"
	"time"

	"github.com/runyanjake/tobee/internal/llm"
)

func exchange(at time.Time, user, reply string) Exchange {
	return Exchange{At: at, Connector: "discord", UserName: "jake", Messages: []llm.Message{
		{Role: llm.RoleUser, Content: user},
		{Role: llm.RoleAssistant, Content: reply},
	}}
}

func TestHistoryAndIdleArchive(t *testing.T) {
	var archived []*Session
	s, err := Open(t.TempDir(), 10*time.Minute, func(sess *Session) error {
		archived = append(archived, sess)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	s.Record("jake", exchange(t0, "hi", "hey"))
	s.Record("jake", exchange(t0.Add(time.Minute), "list files", "a.md"))

	if got := s.History("jake", t0.Add(5*time.Minute)); len(got) != 4 || got[2].Content != "list files" {
		t.Fatalf("History = %+v", got)
	}
	if got := s.History("someone-else", t0); got != nil {
		t.Fatalf("another person saw history: %+v", got)
	}
	// Idle past the timeout: archived and gone, so a new conversation starts clean.
	if got := s.History("jake", t0.Add(12*time.Minute)); got != nil {
		t.Fatalf("stale history returned: %+v", got)
	}
	if len(archived) != 1 || len(archived[0].Exchanges) != 2 {
		t.Fatalf("archived = %+v", archived)
	}
}

// A restart keeps live sessions; the sweep archives them once idle.
func TestSessionsSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, time.Hour, nil)
	s.Record("jake", exchange(time.Now(), "hi", "hey"))

	s2, err := Open(dir, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.History("jake", time.Now()); len(got) != 2 {
		t.Fatalf("history after reopen = %+v", got)
	}
}

func TestHistoryKeepsNewestWithinBudget(t *testing.T) {
	s, _ := Open(t.TempDir(), time.Hour, nil)
	now := time.Now()
	big := strings.Repeat("x", maxHistoryBytes/2)
	s.Record("jake", exchange(now, "first "+big, "ok"))
	s.Record("jake", exchange(now, "second "+big, "ok"))
	s.Record("jake", exchange(now, "third", "ok"))
	got := s.History("jake", now)
	if strings.HasPrefix(got[0].Content, "first") || got[len(got)-2].Content != "third" {
		t.Fatalf("history did not keep the newest exchanges: %d messages, first %.10q", len(got), got[0].Content)
	}
}

func TestMarkdownShowsWhatHappened(t *testing.T) {
	sess := &Session{Person: "jake", Started: time.Now(), LastActive: time.Now(), Exchanges: []Exchange{{
		Connector: "discord", UserName: "jake", At: time.Now(),
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "delete a.md"},
			{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{Function: llm.FunctionCall{Name: "memory_delete", Arguments: `{"uris":["memory://user/a.md"]}`}}}},
			{Role: llm.RoleTool, Content: "deleted memory://user/a.md"},
			{Role: llm.RoleAssistant, Content: "Deleted."},
		},
	}}}
	md := sess.Markdown()
	for _, want := range []string{"**jake** (discord", "delete a.md", "called `memory_delete`", "result: deleted memory://user/a.md", "**tobee**: Deleted."} {
		if !strings.Contains(md, want) {
			t.Fatalf("markdown missing %q:\n%s", want, md)
		}
	}
}
