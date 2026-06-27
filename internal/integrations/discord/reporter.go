package discord

import (
	"context"
	"encoding/json"
	"time"

	"tobee/internal/abilities"
)

// Reporter exposes Discord state via the abilities.Reporter contract.
//
// Doing — connected/listening state, last RX timestamp.
// Done  — count of inbound messages within the window.
func (b *Bot) Reporter() abilities.Reporter { return discReporter{b: b} }

type discReporter struct{ b *Bot }

func (r discReporter) Name() string { return "discord" }

func (r discReporter) Report(_ context.Context, since time.Time) (abilities.ReportData, error) {
	var data abilities.ReportData

	r.b.statsMu.RLock()
	connect := r.b.connect
	lastRx := r.b.lastRx
	rxLog := r.b.rxLog
	rxHead := r.b.rxHead
	rxFill := r.b.rxFill
	r.b.statsMu.RUnlock()

	type doingOut struct {
		Connected     bool      `json:"connected"`
		ConnectedAt   time.Time `json:"connectedAt,omitempty"`
		ChannelFilter string    `json:"channelFilter,omitempty"`
		LastRx        time.Time `json:"lastRx,omitempty"`
	}
	d := doingOut{
		Connected:     !connect.IsZero(),
		ConnectedAt:   connect,
		ChannelFilter: r.b.channelID,
		LastRx:        lastRx,
	}
	if raw, err := json.Marshal(d); err == nil {
		data.Doing = raw
	}

	count := 0
	channels := map[string]int{}
	n := rxHead
	if rxFill {
		n = len(rxLog)
	}
	walk := func(ev rxEvent) {
		if ev.At.IsZero() {
			return
		}
		if !since.IsZero() && ev.At.Before(since) {
			return
		}
		count++
		channels[ev.Ch]++
	}
	if rxFill {
		for i := 0; i < len(rxLog); i++ {
			walk(rxLog[(rxHead+i)%len(rxLog)])
		}
	} else {
		for i := 0; i < n; i++ {
			walk(rxLog[i])
		}
	}
	if count > 0 {
		out := struct {
			Inbound  int            `json:"inbound"`
			Channels map[string]int `json:"channels,omitempty"`
		}{Inbound: count, Channels: channels}
		if raw, err := json.Marshal(out); err == nil {
			data.Done = raw
		}
	}

	return data, nil
}
