package mcphost

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/runyanjake/tobee/internal/event"
)

// External servers are configured with MCP_SERVER_<NAME>_* variables (see .env.example);
// the lowercased <NAME> is the server name and tool namespace.
const envPrefix = "MCP_SERVER_"

type ServerConfig struct {
	Name        string
	Command     []string
	URL         string
	BearerToken string
	Env         []string // KEY=VALUE, passed to a stdio server
	Trusted     bool
	Timeout     time.Duration
	Subscribe   []string
	ReplyTo     event.Address
}

var suffixes = []string{"_COMMAND", "_URL", "_BEARER_TOKEN", "_TRUSTED", "_TIMEOUT", "_SUBSCRIBE", "_REPLY_TO"}

// LoadServers returns the valid servers even when others fail; failures are joined into err.
func LoadServers(environ []string) ([]ServerConfig, error) {
	vals := map[string]map[string]string{} // NAME → suffix → value
	envs := map[string][]string{}          // NAME → KEY=VALUE
	for _, kv := range environ {
		key, val, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(key, envPrefix) {
			continue
		}
		rest := strings.TrimPrefix(key, envPrefix)
		if name, v, ok := strings.Cut(rest, "_ENV_"); ok && name != "" && v != "" {
			envs[name] = append(envs[name], v+"="+val)
			continue
		}
		for _, sfx := range suffixes {
			if name, ok := strings.CutSuffix(rest, sfx); ok && name != "" {
				if vals[name] == nil {
					vals[name] = map[string]string{}
				}
				vals[name][sfx] = val
				break
			}
		}
	}

	names := make([]string, 0, len(vals))
	for n := range vals {
		names = append(names, n)
	}
	sort.Strings(names)

	var out []ServerConfig
	var errs []error
	for _, raw := range names {
		c, err := parseServer(raw, vals[raw], envs[raw])
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, c)
	}
	return out, errors.Join(errs...)
}

func parseServer(raw string, v map[string]string, env []string) (ServerConfig, error) {
	c := ServerConfig{
		Name:        strings.ToLower(raw),
		Command:     strings.Fields(v["_COMMAND"]),
		URL:         strings.TrimSpace(v["_URL"]),
		BearerToken: v["_BEARER_TOKEN"],
		Env:         env,
	}
	if sanitize(c.Name) != c.Name {
		return c, fmt.Errorf("%s%s: name must be [A-Z0-9_-]", envPrefix, raw)
	}
	if (len(c.Command) == 0) == (c.URL == "") {
		return c, fmt.Errorf("%s%s: set exactly one of _COMMAND or _URL", envPrefix, raw)
	}
	if t := v["_TRUSTED"]; t != "" {
		b, err := strconv.ParseBool(t)
		if err != nil {
			return c, fmt.Errorf("%s%s_TRUSTED: %w", envPrefix, raw, err)
		}
		c.Trusted = b
	}
	if t := v["_TIMEOUT"]; t != "" {
		d, err := time.ParseDuration(t)
		if err != nil {
			return c, fmt.Errorf("%s%s_TIMEOUT: %w", envPrefix, raw, err)
		}
		c.Timeout = d
	}
	for _, u := range strings.Split(v["_SUBSCRIBE"], ",") {
		if u = strings.TrimSpace(u); u != "" {
			c.Subscribe = append(c.Subscribe, u)
		}
	}
	if len(c.Subscribe) > 0 {
		// A notification becomes a task with every tool available; only a trusted server may start one.
		if !c.Trusted {
			return c, fmt.Errorf("%s%s_SUBSCRIBE requires _TRUSTED=true", envPrefix, raw)
		}
		conn, ch, ok := strings.Cut(v["_REPLY_TO"], ":")
		if !ok || conn == "" || ch == "" {
			return c, fmt.Errorf("%s%s_SUBSCRIBE requires _REPLY_TO=<connector>:<channel>", envPrefix, raw)
		}
		c.ReplyTo = event.Address{Connector: conn, Channel: ch}
	}
	return c, nil
}

func (c ServerConfig) Transport() mcp.Transport {
	if c.URL != "" {
		t := &mcp.StreamableClientTransport{Endpoint: c.URL}
		if c.BearerToken != "" {
			t.HTTPClient = &http.Client{Transport: bearer{token: c.BearerToken, next: http.DefaultTransport}}
		}
		return t
	}
	cmd := exec.Command(c.Command[0], c.Command[1:]...)
	// Never pass tobee's own environment to a third-party process: it holds tobee's secrets.
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}, c.Env...)
	return &mcp.CommandTransport{Command: cmd}
}

func (c ServerConfig) Options() Options {
	return Options{Trusted: c.Trusted, Timeout: c.Timeout}
}

type bearer struct {
	token string
	next  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.next.RoundTrip(r)
}
