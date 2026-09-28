package mcphost

import (
	"strings"
	"testing"
)

func TestLoadServers(t *testing.T) {
	cfgs, err := LoadServers([]string{
		"MCP_SERVER_WEATHER_URL=https://example.com/mcp",
		"MCP_SERVER_WEATHER_BEARER_TOKEN=secret",
		"MCP_SERVER_FILES_COMMAND=npx -y @modelcontextprotocol/server-filesystem /tmp",
		"MCP_SERVER_FILES_ENV_NODE_ENV=production",
		"MCP_SERVER_FILES_TRUSTED=true",
		"MCP_SERVER_FILES_TIMEOUT=1m",
		"MCP_SERVER_FILES_SUBSCRIBE=file:///tmp/a, file:///tmp/b",
		"MCP_SERVER_FILES_REPLY_TO=discord:123",
		"UNRELATED=1",
	})
	if err != nil {
		t.Fatalf("LoadServers: %v", err)
	}
	if len(cfgs) != 2 {
		t.Fatalf("got %d configs, want 2", len(cfgs))
	}
	files, weather := cfgs[0], cfgs[1]
	if files.Name != "files" || len(files.Command) != 4 || !files.Trusted || files.Timeout.Minutes() != 1 {
		t.Fatalf("files = %+v", files)
	}
	if len(files.Env) != 1 || files.Env[0] != "NODE_ENV=production" {
		t.Fatalf("files.Env = %v", files.Env)
	}
	if len(files.Subscribe) != 2 || files.ReplyTo.Connector != "discord" || files.ReplyTo.Channel != "123" {
		t.Fatalf("files subscription = %+v", files)
	}
	if weather.Name != "weather" || weather.URL == "" || weather.Trusted || weather.BearerToken != "secret" {
		t.Fatalf("weather = %+v", weather)
	}
}

// Notifications become tasks with every tool available, so only a
// trusted server may start one.
func TestLoadServersRejectsUntrustedSubscription(t *testing.T) {
	cfgs, err := LoadServers([]string{
		"MCP_SERVER_FEED_URL=https://example.com/mcp",
		"MCP_SERVER_FEED_SUBSCRIBE=feed://latest",
		"MCP_SERVER_FEED_REPLY_TO=discord:1",
	})
	if err == nil || !strings.Contains(err.Error(), "_TRUSTED=true") {
		t.Fatalf("err = %v, want trust requirement", err)
	}
	if len(cfgs) != 0 {
		t.Fatalf("invalid server was returned: %+v", cfgs)
	}
}

func TestLoadServersNeedsOneTransport(t *testing.T) {
	_, err := LoadServers([]string{
		"MCP_SERVER_BOTH_URL=https://example.com/mcp",
		"MCP_SERVER_BOTH_COMMAND=server",
	})
	if err == nil {
		t.Fatal("expected an error for both _COMMAND and _URL")
	}
}
