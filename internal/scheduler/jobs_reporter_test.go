package scheduler

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The summary is activity, not inventory. Saying "I'm holding 2 reminders for
// you" here put a count of what was merely waiting into a sentence people read
// as an answer, against D-053 — and it made status_summary read like the
// reminder-listing tool, so the model began choosing it over schedule_list and
// answered "List all reminders" with a status line (D-062). What is waiting
// belongs in the report, which already lists every job.
func TestSummaryIsSilentAboutPendingReminders(t *testing.T) {
	store, err := NewJobStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := NewJobManager(store)
	for _, p := range []string{"Text Darren about basketball", "Water the plants"} {
		if _, err := m.Create(Job{
			Prompt: p, At: time.Now().Add(time.Hour),
			Connector: "discord", Channel: "c1", User: "u1",
		}); err != nil {
			t.Fatal(err)
		}
	}

	full, summary := m.Reporter().Render(context.Background(), time.Time{})
	if summary != "" {
		t.Fatalf("summary = %q, want empty: nothing has happened, two things are merely waiting", summary)
	}
	// The report is where inventory lives, and it still has it.
	if !strings.Contains(full, "Waiting:") {
		t.Fatalf("report lost its Waiting section:\n%s", full)
	}
	for _, want := range []string{"Text Darren about basketball", "Water the plants"} {
		if !strings.Contains(full, want) {
			t.Fatalf("report missing %q:\n%s", want, full)
		}
	}
}

func TestSummaryReportsRemindersThatFired(t *testing.T) {
	store, err := NewJobStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := NewJobManager(store)
	// Stand in for fires: the ring is what Render reads.
	m.recent[0] = firedJobEvent{ID: "j-1", Name: "basketball", At: time.Now(), OneShot: true}
	m.head = 1

	_, summary := m.Reporter().Render(context.Background(), time.Time{})
	if !strings.Contains(summary, "1 reminder of yours went off") {
		t.Fatalf("summary = %q, want the fired reminder reported", summary)
	}
}
