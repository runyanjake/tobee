package actions

import (
	"context"
	"fmt"
	"strings"
	"tobee/internal/state"
)

// Register wires all built-in actions onto the given State.
func Register(s *state.State) {
	s.RegisterAction("echo", echo)
	s.RegisterAction("help", help(s))
}

// echo returns the "message" arg as-is — useful for testing triggers.
func echo(_ context.Context, _ *state.State, args map[string]string) (string, error) {
	msg := args["message"]
	if msg == "" {
		msg = args["msg"]
	}
	return msg, nil
}

// help lists all registered action names.
func help(s *state.State) state.ActionFunc {
	return func(_ context.Context, _ *state.State, _ map[string]string) (string, error) {
		names := s.ListActions()
		var b strings.Builder
		b.WriteString("Available actions:\n")
		for _, name := range names {
			fmt.Fprintf(&b, "  !%s\n", name)
		}
		return b.String(), nil
	}
}

