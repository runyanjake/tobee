// Package user is the built-in "user" MCP server; ask sends a clarifying question
// and parks the task until the user answers (D-036).
package user

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/runyanjake/tobee/internal/delivery"
	"github.com/runyanjake/tobee/internal/event"
	"github.com/runyanjake/tobee/internal/mcpserver"
	"github.com/runyanjake/tobee/internal/scope"
)

func New(instructions string, out *delivery.Router) *mcpserver.Server {
	srv := mcpserver.New("user", instructions)
	srv.Add(mcpserver.Tool{
		Name: "ask",
		Description: "Ask the user a clarifying question when the request is ambiguous and a wrong guess " +
			"would waste work or do harm. Sends the question to the user now and ends this turn; the task " +
			"resumes when they answer. Ask one short, specific question.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"question": {"type": "string", "description": "The question, as the user will read it."}
			},
			"required": ["question"]
		}`),
		Handler: askHandler(out),
	})
	return srv
}

func askHandler(out *delivery.Router) mcpserver.Handler {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var in struct {
			Question string `json:"question"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
		question := strings.TrimSpace(in.Question)
		if question == "" {
			return "", fmt.Errorf("question is required")
		}
		s, ok := scope.From(ctx)
		if !ok || !s.HasUser() || s.Connector == "" {
			return "", fmt.Errorf("user_ask unavailable: no user is attached to this turn")
		}

		addr := s.Address()
		msgID, err := out.Send(ctx, addr, question)
		if err != nil {
			return "", err
		}

		keys := []string{event.ActorKey(addr, s.User)}
		if msgID != "" {
			keys = append([]string{event.ReplyKey(addr, msgID)}, keys...)
		}
		mcpserver.SetResultMeta(ctx, mcpserver.MetaAwait, mcpserver.Await{Keys: keys, Question: question})
		return "Question sent. This turn ends now and resumes when the user answers.", nil
	}
}
