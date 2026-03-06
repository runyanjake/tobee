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
	s.RegisterAction("set_context", setContext)
	s.RegisterAction("get_context", getContext)
	s.RegisterAction("clear_context", clearContext)
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

// setContext stores a key/value pair in AdditionalContext.
// Required args: key, value
func setContext(_ context.Context, s *state.State, args map[string]string) (string, error) {
	key := args["key"]
	if key == "" {
		return "", fmt.Errorf("set_context requires 'key' arg")
	}
	value := args["value"]
	s.SetAdditionalContext(key, value)
	return fmt.Sprintf("Set context: %s = %s", key, value), nil
}

// getContext retrieves a value from AdditionalContext.
// Required args: key
func getContext(_ context.Context, s *state.State, args map[string]string) (string, error) {
	key := args["key"]
	if key == "" {
		return "", fmt.Errorf("get_context requires 'key' arg")
	}
	val, ok := s.GetAdditionalContext(key)
	if !ok {
		return fmt.Sprintf("No context found for key: %s", key), nil
	}
	return fmt.Sprintf("%s = %s", key, val), nil
}

// clearContext removes a key from AdditionalContext.
// Required args: key
func clearContext(_ context.Context, s *state.State, args map[string]string) (string, error) {
	key := args["key"]
	if key == "" {
		return "", fmt.Errorf("clear_context requires 'key' arg")
	}
	s.DeleteAdditionalContext(key)
	return fmt.Sprintf("Cleared context key: %s", key), nil
}
