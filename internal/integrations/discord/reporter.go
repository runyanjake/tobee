package discord

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/runyanjake/tobee/internal/abilities"
)

// Reporter exposes Discord state via the abilities.Reporter contract.
func (b *Bot) Reporter() abilities.Reporter { return discReporter{b: b} }

type discReporter struct{ b *Bot }

func (r discReporter) Name() string { return "discord" }

func (r discReporter) Render(_ context.Context, since time.Time) (string, string) {
	r.b.statsMu.RLock()
	connect := r.b.connect
	lastRx := r.b.lastRx
	rxLog := append([]rxEvent(nil), r.b.rxLog...)
	rxHead := r.b.rxHead
	rxFill := r.b.rxFill
	r.b.statsMu.RUnlock()

	connected := !connect.IsZero()

	inbound := 0
	channels := map[string]int{}
	count := func(ev rxEvent) {
		if ev.At.IsZero() {
			return
		}
		if !since.IsZero() && ev.At.Before(since) {
			return
		}
		inbound++
		channels[ev.Ch]++
	}
	if rxFill {
		for i := 0; i < len(rxLog); i++ {
			count(rxLog[(rxHead+i)%len(rxLog)])
		}
	} else {
		for i := 0; i < rxHead; i++ {
			count(rxLog[i])
		}
	}

	var full strings.Builder
	fmt.Fprintf(&full, "Doing: %s\n", discordDoingLine(connected, connect, r.b.channelID, lastRx))
	if inbound == 0 {
		full.WriteString("Done: no inbound messages in window\n")
	} else {
		fmt.Fprintf(&full, "Done: %d inbound — %s\n", inbound, discordChannelCounts(channels))
	}
	full.WriteString("Waiting: —\n")

	var summary string
	switch {
	case !connected:
		summary = "Discord is offline"
	case inbound == 0:
		summary = "Discord is connected with no recent messages"
	default:
		summary = fmt.Sprintf("Discord is connected and saw %d inbound message%s in the window",
			inbound, discordPlural(inbound))
	}
	return full.String(), summary
}

func discordDoingLine(connected bool, connect time.Time, channel string, lastRx time.Time) string {
	if !connected {
		return "offline"
	}
	parts := []string{fmt.Sprintf("connected since %s", connect.UTC().Format(time.RFC3339))}
	if channel != "" {
		parts = append(parts, fmt.Sprintf("channel=%s", channel))
	}
	if !lastRx.IsZero() {
		parts = append(parts, fmt.Sprintf("last RX %s", lastRx.UTC().Format(time.RFC3339)))
	}
	return strings.Join(parts, ", ")
}

func discordChannelCounts(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, fmt.Sprintf("%s=%d", k, m[k]))
	}
	return strings.Join(out, ", ")
}

func discordPlural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
