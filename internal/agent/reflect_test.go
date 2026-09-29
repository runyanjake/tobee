package agent

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runyanjake/tobee/internal/llm"
	"github.com/runyanjake/tobee/internal/session"
)

func reflector(t *testing.T, calls ...call) (*Reflector, *scriptedLLM, *[]string) {
	t.Helper()
	fake := &scriptedLLM{}
	fake.script(calls...)
	states, err := LoadStateTemplates(filepath.Join("..", "..", "prompts", "state"))
	if err != nil {
		t.Fatal(err)
	}
	var saved []string
	r := NewReflector(fake, states, func(_ string, lessons []string) error {
		saved = append(saved, lessons...)
		return nil
	})
	return r, fake, &saved
}

func failedSession() *session.Session {
	return &session.Session{
		Person: "jake",
		Exchanges: []session.Exchange{{
			At:       time.Now(),
			Messages: []llm.Message{{Role: llm.RoleUser, Content: "clear my reminders for today"}},
			Outcome: &session.Outcome{
				Status:   "replied",
				Steps:    4,
				Problems: []string{"schedule_list: called with the same arguments 3 times; closed for the turn"},
			},
		}},
	}
}

// A closed session that failed produces lessons for the next one (D-052),
// drawn only from what code recorded.
func TestReflectSavesLessonsFromFailures(t *testing.T) {
	r, fake, saved := reflector(t, call{
		name: lessonsTool,
		args: `{"lessons":["to clear a reminder, call schedule_list for the id then schedule_cancel"]}`,
	})
	r.Reflect(context.Background(), failedSession())

	if len(*saved) != 1 || !strings.Contains((*saved)[0], "schedule_cancel") {
		t.Fatalf("saved = %q", *saved)
	}
	// The model is given the recorded facts and nothing else.
	facts := fake.requests[0][0].Content
	for _, want := range []string{"<failures>", "clear my reminders for today", "schedule_list: called with the same arguments"} {
		if !strings.Contains(facts, want) {
			t.Fatalf("facts missing %q:\n%s", want, facts)
		}
	}
	if only := fake.offered[0]; len(only) != 1 || only[0] != lessonsTool {
		t.Fatalf("offered %v, want only %s", only, lessonsTool)
	}
}

// Nothing failed, so there is nothing to learn and no call to make.
func TestReflectSkipsCleanSessions(t *testing.T) {
	r, fake, saved := reflector(t)
	r.Reflect(context.Background(), &session.Session{
		Person: "jake",
		Exchanges: []session.Exchange{{
			Messages: []llm.Message{{Role: llm.RoleUser, Content: "hey"}},
			Outcome:  &session.Outcome{Status: "replied", Steps: 1},
		}},
	})
	if len(*saved) != 0 || len(fake.requests) != 0 {
		t.Fatalf("saved = %q after %d model calls, want neither", *saved, len(fake.requests))
	}
}

// An empty list is a valid answer: not every failure has a general lesson.
func TestReflectAcceptsNoLessons(t *testing.T) {
	r, _, saved := reflector(t, call{name: lessonsTool, args: `{"lessons":[]}`})
	r.Reflect(context.Background(), failedSession())
	if len(*saved) != 0 {
		t.Fatalf("saved = %q, want nothing", *saved)
	}
}

// Reflection is best-effort: unusable output is dropped, never written.
func TestReflectDropsUnusableOutput(t *testing.T) {
	r, _, saved := reflector(t, call{invalid: true})
	r.Reflect(context.Background(), failedSession())
	if len(*saved) != 0 {
		t.Fatalf("saved = %q from unreadable output", *saved)
	}
}

// A nil Reflector is how AGENT_REFLECT=false is wired, so it must be safe.
func TestReflectDisabledIsSafe(t *testing.T) {
	var r *Reflector
	r.Reflect(context.Background(), failedSession())
}
