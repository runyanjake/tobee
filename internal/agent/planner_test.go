package agent

import "testing"

func stepsArg(intents ...string) commitArgs {
	var args commitArgs
	for _, in := range intents {
		args.Steps = append(args.Steps, struct {
			Intent string `json:"intent"`
		}{Intent: in})
	}
	return args
}

func TestPlanFromCommitArgsFastPath(t *testing.T) {
	args := commitArgs{Goal: "Answer the greeting.", DirectReply: "  Hey. What's up?  "}

	plan, err := planFromCommitArgs(args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := plan.DirectReply, "Hey. What's up?"; got != want {
		t.Errorf("DirectReply = %q, want %q", got, want)
	}
	if len(plan.Steps) != 0 {
		t.Errorf("Steps = %d, want 0", len(plan.Steps))
	}
	// A zero-step plan must render nothing: the loop keys the
	// announcement off this, so a non-empty string here would put a
	// stray plan message back on every trivial turn.
	if got := plan.RenderAnnouncement(); got != "" {
		t.Errorf("RenderAnnouncement = %q, want empty", got)
	}
}

func TestPlanFromCommitArgsStepsDropDirectReply(t *testing.T) {
	args := stepsArg("Report tobee's current subsystem status.")
	args.Goal = "Report status."
	args.DirectReply = "Everything is fine."

	plan, err := planFromCommitArgs(args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan.DirectReply != "" {
		t.Errorf("DirectReply = %q, want empty — steps must win", plan.DirectReply)
	}
	if len(plan.Steps) != 1 {
		t.Fatalf("Steps = %d, want 1", len(plan.Steps))
	}
	if plan.Steps[0].ID != "s1" || plan.Steps[0].Status != StepPending {
		t.Errorf("step = %+v, want id s1 and pending", plan.Steps[0])
	}
}

func TestPlanFromCommitArgsRejectsEmpty(t *testing.T) {
	cases := []struct {
		name string
		args commitArgs
	}{
		{"no steps and no direct_reply", commitArgs{Goal: "Do a thing."}},
		{"blank steps and blank direct_reply", func() commitArgs {
			a := stepsArg("", "   ")
			a.DirectReply = "   "
			return a
		}()},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := planFromCommitArgs(tc.args); err == nil {
				t.Fatal("expected an error, got nil")
			}
		})
	}
}
