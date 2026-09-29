package scheduler

import (
	"strings"
	"testing"
	"time"
)

// A reminder's confirmation is read by a person, so the fire time must render
// in the local zone and on the local calendar day (D-049).
func TestFormatWhen(t *testing.T) {
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skipf("zoneinfo unavailable: %v", err)
	}
	old := time.Local
	time.Local = la
	defer func() { time.Local = old }()

	now := time.Now().In(la)
	// 6pm local falls on the next UTC day; truncating on UTC midnight would
	// report it as "tomorrow".
	evening := time.Date(now.Year(), now.Month(), now.Day(), 18, 0, 0, 0, la)
	zone := evening.Format("MST") // PDT or PST, depending on the date

	tests := []struct {
		name string
		at   time.Time
		want string
	}{
		{"evening today", evening, "6:00pm " + zone + " today"},
		{"tomorrow", evening.AddDate(0, 0, 1), "6:00pm " + zone + " tomorrow"},
		{"a UTC instant is converted", evening.UTC(), "6:00pm " + zone + " today"},
		{"unset", time.Time{}, "unknown"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := FormatWhen(tc.at); got != tc.want {
				t.Fatalf("FormatWhen() = %q, want %q", got, tc.want)
			}
		})
	}
}

// Beyond tomorrow the day is named, so "Thursday" isn't a guess.
func TestFormatWhenDistantDateNamesTheDay(t *testing.T) {
	got := FormatWhen(time.Now().Add(72 * time.Hour))
	for _, unwanted := range []string{"today", "tomorrow"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("FormatWhen(+72h) = %q, should not say %q", got, unwanted)
		}
	}
	if len(strings.Fields(got)) < 4 {
		t.Fatalf("FormatWhen(+72h) = %q, want weekday, date, time and zone", got)
	}
}
