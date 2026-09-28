package status

import (
	"encoding/json"
	"testing"
	"time"
)

func TestParseWindow(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"empty defaults to 1h", "", defaultWindow, false},
		{"whitespace defaults to 1h", "  ", defaultWindow, false},
		{"minutes", "30m", 30 * time.Minute, false},
		{"hours", "24h", 24 * time.Hour, false},
		{"days suffix", "7d", 7 * 24 * time.Hour, false},
		{"fractional days", "0.5d", 12 * time.Hour, false},
		{"compound", "1h30m", 90 * time.Minute, false},
		{"at the maximum", "30d", maxWindow, false},
		{"beyond the maximum", "31d", 0, true},
		{"zero rejected", "0h", 0, true},
		{"negative rejected", "-1h", 0, true},
		{"absolute timestamp rejected", "2023-10-05T18:00:00Z", 0, true},
		{"prose rejected", "last hour", 0, true},
		{"bare number rejected", "24", 0, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseWindow(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseWindow(%q) = %v, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseWindow(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("parseWindow(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseSince(t *testing.T) {
	now := time.Now()

	cases := []struct {
		name string
		args string
		want time.Time
	}{
		{"omitted", `{}`, now.Add(-defaultWindow)},
		{"explicit window", `{"window":"24h"}`, now.Add(-24 * time.Hour)},
		{"days", `{"window":"7d"}`, now.Add(-7 * 24 * time.Hour)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseSince(json.RawMessage(tc.args))
			if err != nil {
				t.Fatalf("parseSince(%s): %v", tc.args, err)
			}
			// parseSince reads the clock internally; allow drift.
			if diff := got.Sub(tc.want); diff > time.Second || diff < -time.Second {
				t.Fatalf("parseSince(%s) = %v, want ~%v", tc.args, got, tc.want)
			}
		})
	}

	if _, err := parseSince(json.RawMessage(`{"window":"nonsense"}`)); err == nil {
		t.Fatal("parseSince with a bad window: want error")
	}
}
