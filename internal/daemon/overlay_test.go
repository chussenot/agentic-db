package daemon

import (
	"testing"

	"github.com/mrzor/claude-status/internal/clauded"
	"github.com/mrzor/claude-status/internal/db"
	"github.com/mrzor/claude-status/internal/state"
)

func TestFirstPartyStateMapping(t *testing.T) {
	cases := []struct {
		in   clauded.Status
		want state.Status
		ok   bool
	}{
		{clauded.Busy, state.Working, true},
		{clauded.Waiting, state.Prompt, true},
		{clauded.Idle, state.Idle, true},
		{clauded.Status("compacting"), "", false},
		{clauded.Status(""), "", false},
	}
	for _, c := range cases {
		got, ok := firstPartyState(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("firstPartyState(%q) = (%q,%v), want (%q,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestOverlayPrefersFirstPartyState(t *testing.T) {
	sessions := []db.Session{
		{SessionID: "a", State: "idle"},    // first-party says busy -> working
		{SessionID: "b", State: "working"}, // first-party says waiting -> prompt
		{SessionID: "c", State: "prompt"},  // first-party says idle -> idle (clears a stuck "?")
	}
	fp := map[string]clauded.Session{
		"a": {SessionID: "a", Status: clauded.Busy},
		"b": {SessionID: "b", Status: clauded.Waiting},
		"c": {SessionID: "c", Status: clauded.Idle},
	}
	overlayFirstParty(sessions, fp)

	want := map[string]string{"a": "working", "b": "prompt", "c": "idle"}
	for _, s := range sessions {
		if s.State != want[s.SessionID] {
			t.Errorf("session %s state = %q, want %q", s.SessionID, s.State, want[s.SessionID])
		}
	}
}

func TestOverlayKeepsHookStateWhenNoFirstParty(t *testing.T) {
	// A session absent from the first-party map (e.g. remote/ssh) keeps its
	// hook-derived state — the notification path remains the fallback.
	sessions := []db.Session{{SessionID: "remote", State: "prompt"}}
	overlayFirstParty(sessions, map[string]clauded.Session{})
	if sessions[0].State != "prompt" {
		t.Errorf("state = %q, want prompt preserved", sessions[0].State)
	}
}

func TestOverlayIgnoresUnknownFirstPartyStatus(t *testing.T) {
	// An unrecognized first-party status must not clobber the hook state.
	sessions := []db.Session{{SessionID: "a", State: "working"}}
	fp := map[string]clauded.Session{"a": {SessionID: "a", Status: clauded.Status("compacting")}}
	overlayFirstParty(sessions, fp)
	if sessions[0].State != "working" {
		t.Errorf("state = %q, want working preserved (unknown status ignored)", sessions[0].State)
	}
}

func TestOverlayOnlyTouchesState(t *testing.T) {
	// window_id / last_talk_ts (decay timing) must survive the overlay untouched.
	s := workingSession(42)
	s.SessionID = "a"
	s.LastTalkTS.Int64, s.LastTalkTS.Valid = 1234, true
	sessions := []db.Session{s}
	overlayFirstParty(sessions, map[string]clauded.Session{"a": {SessionID: "a", Status: clauded.Idle}})

	if sessions[0].State != "idle" {
		t.Errorf("state = %q, want idle", sessions[0].State)
	}
	if !sessions[0].WindowID.Valid || sessions[0].WindowID.Int64 != 42 {
		t.Errorf("window_id clobbered: %+v", sessions[0].WindowID)
	}
	if !sessions[0].LastTalkTS.Valid || sessions[0].LastTalkTS.Int64 != 1234 {
		t.Errorf("last_talk_ts clobbered: %+v", sessions[0].LastTalkTS)
	}
}
