package fleet

import (
	"testing"
	"time"
)

func TestIsConsoleTitleMatchesOnlyTheAggregateForm(t *testing.T) {
	for _, title := range []string{
		"6 awaiting input · claude agents",
		"⠙ 0 awaiting input · claude agents", // a spinner frame must not change the meaning
		"claude agents",
	} {
		if !IsConsoleTitle(title) {
			t.Errorf("IsConsoleTitle(%q) = false, want true", title)
		}
	}
	for _, title := range []string{
		"", "✳ project-manager-5", "~/taf/project-manager",
	} {
		if IsConsoleTitle(title) {
			t.Errorf("IsConsoleTitle(%q) = true, want false", title)
		}
	}
}

// The fossil case, measured 2026-08-25: two windows still titled "claude agents"
// whose host sessions had been silent 188h and 187h — with their claude processes
// STILL RUNNING, which is why process liveness cannot be the gate. A genuine console
// goes quiet while its view is open (22h observed live) and must survive.
func TestConsoleHostLiveSeparatesAWatchedConsoleFromAnEightDayFossil(t *testing.T) {
	now := time.UnixMilli(1_787_660_000_000)
	cases := []struct {
		name     string
		lastTalk time.Time
		want     bool
	}{
		{"just typed into", now.Add(-90 * time.Second), true},
		{"the 22h resting console this mechanism exists for", now.Add(-22 * time.Hour), true},
		{"never talked yet (no evidence of death)", time.Time{}, true},
		{"window 34, silent 187h", now.Add(-187 * time.Hour), false},
		{"window 12, silent 188h", now.Add(-188 * time.Hour), false},
	}
	for _, c := range cases {
		if got := ConsoleHostLive(c.lastTalk, now); got != c.want {
			t.Errorf("%s: ConsoleHostLive = %v, want %v", c.name, got, c.want)
		}
	}
}
