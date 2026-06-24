package daemon

import (
	"testing"
	"time"

	"github.com/mrzor/claude-status/internal/clauded"
	"github.com/mrzor/claude-status/internal/db"
	"github.com/mrzor/claude-status/internal/state"
)

func TestFirstPartyStateMapping(t *testing.T) {
	cases := []struct {
		name       string
		in         clauded.Status
		waitingFor string
		fpMS       int64 // first-party StatusUpdatedAt (unix ms); 0 => zero/absent
		hookSeen   int64 // hook LastSeenTS (unix ms)
		want       state.Status
		ok         bool
	}{
		// busy with no timestamp -> legacy busy->working (overlay still fixes a stale hook).
		{"busy-no-ts", clauded.Busy, "", 0, 0, state.Working, true},
		// fresh busy (>= last hook) -> working preserved (genuinely working session).
		{"busy-fresh", clauded.Busy, "", 5000, 4000, state.Working, true},
		{"busy-equal", clauded.Busy, "", 5000, 5000, state.Working, true},
		// stale busy (a hook fired AFTER the busy was written) -> defer to hook (clickhouse case).
		{"busy-stale", clauded.Busy, "", 4000, 5000, "", false},
		// waiting alone is overloaded: deferred to hooks (the /btw false-"?" case).
		{"waiting-bare", clauded.Waiting, "", 0, 0, "", false},
		{"waiting-internal", clauded.Waiting, "subagent", 0, 0, "", false},
		// waiting + a permission prompt is the genuine "needs you" -> Prompt. NOT
		// freshness-gated: a real prompt persists even when the file is stale (ezmm).
		{"waiting-permission", clauded.Waiting, "permission prompt", 1000, 9000, state.Prompt, true},
		{"waiting-permission-case", clauded.Waiting, "Permission Prompt", 0, 0, state.Prompt, true},
		// idle is not gated either (rest persists).
		{"idle-stale", clauded.Idle, "", 1000, 9000, state.Idle, true},
		{"shell", clauded.Shell, "", 0, 0, state.Shell, true},
		{"unknown", clauded.Status("compacting"), "", 0, 0, "", false},
		{"empty", clauded.Status(""), "", 0, 0, "", false},
	}
	for _, c := range cases {
		s := clauded.Session{Status: c.in, WaitingFor: c.waitingFor}
		if c.fpMS != 0 {
			s.StatusUpdatedAt = time.UnixMilli(c.fpMS)
		}
		got, ok := firstPartyState(s, c.hookSeen)
		if ok != c.ok || got != c.want {
			t.Errorf("%s: firstPartyState(%q,wf=%q,fp=%d,hook=%d) = (%q,%v), want (%q,%v)", c.name, c.in, c.waitingFor, c.fpMS, c.hookSeen, got, ok, c.want, c.ok)
		}
	}
}

func TestOverlayPrefersFirstPartyState(t *testing.T) {
	sessions := []db.Session{
		{SessionID: "a", State: "idle"},    // first-party says busy -> working
		{SessionID: "b", State: "working"}, // first-party says waiting -> DEFERRED, hook "working" kept
		{SessionID: "c", State: "prompt"},  // first-party says idle -> idle (clears a stuck "?")
	}
	fp := map[string]clauded.Session{
		"a": {SessionID: "a", Status: clauded.Busy},
		"b": {SessionID: "b", Status: clauded.Waiting},
		"c": {SessionID: "c", Status: clauded.Idle},
	}
	overlayFirstParty(sessions, fp)

	// "b": waiting no longer overrides — the hook-derived "working" is preserved,
	// because waiting is overloaded (subagent/tool wait, not necessarily "?").
	want := map[string]string{"a": "working", "b": "working", "c": "idle"}
	for _, s := range sessions {
		if s.State != want[s.SessionID] {
			t.Errorf("session %s state = %q, want %q", s.SessionID, s.State, want[s.SessionID])
		}
	}
}

func TestOverlayWaitingDoesNotForcePrompt(t *testing.T) {
	// Regression for the /btw false "?" (memory
	// first-party-status-waiting-is-not-a-reliable): a session whose turn ended
	// (hook -> idle) but whose first-party sat at `waiting` (a subagent/tool wait,
	// not a question) must NOT be flipped to prompt. It stays on the hook state.
	sessions := []db.Session{
		{SessionID: "btw", State: "idle"},    // Stop fired; hook says idle
		{SessionID: "mid", State: "working"}, // still mid-turn per hooks
	}
	fp := map[string]clauded.Session{
		"btw": {SessionID: "btw", Status: clauded.Waiting},
		"mid": {SessionID: "mid", Status: clauded.Waiting},
	}
	overlayFirstParty(sessions, fp)
	if sessions[0].State != "idle" {
		t.Errorf("btw state = %q, want idle (waiting must not force prompt)", sessions[0].State)
	}
	if sessions[1].State != "working" {
		t.Errorf("mid state = %q, want working preserved", sessions[1].State)
	}
}

func TestOverlayStaleBusyDefersToHookIdle(t *testing.T) {
	// The clickhouse-server case: first-party froze at `busy`, but the hook fired
	// Stop->idle AFTER the busy was written (LastSeenTS newer than the busy's
	// statusUpdatedAt). The stale busy must NOT override the fresher idle.
	sessions := []db.Session{{SessionID: "ch", State: "idle", LastSeenTS: 5000}}
	fp := map[string]clauded.Session{
		"ch": {SessionID: "ch", Status: clauded.Busy, StatusUpdatedAt: time.UnixMilli(4000)},
	}
	overlayFirstParty(sessions, fp)
	if sessions[0].State != "idle" {
		t.Errorf("state = %q, want idle (stale busy must not override fresher hook)", sessions[0].State)
	}
}

func TestOverlayFreshBusyOverridesToWorking(t *testing.T) {
	// A genuinely-working session: first-party busy is at least as fresh as the
	// last hook, so it still overrides a stale hook idle (the overlay's original
	// missed-Stop fix must keep working).
	sessions := []db.Session{{SessionID: "w", State: "idle", LastSeenTS: 4000}}
	fp := map[string]clauded.Session{
		"w": {SessionID: "w", Status: clauded.Busy, StatusUpdatedAt: time.UnixMilli(5000)},
	}
	overlayFirstParty(sessions, fp)
	if sessions[0].State != "working" {
		t.Errorf("state = %q, want working (fresh busy overrides)", sessions[0].State)
	}
}

func TestOverlayPromotesPermissionPromptToPrompt(t *testing.T) {
	// The ezmm 4h-stuck case: hook state is stale "working" (a permission
	// Notification got clobbered by a later PostToolUse), but first-party durably
	// reports waiting + "permission prompt". The overlay must flip it to prompt.
	sessions := []db.Session{{SessionID: "ezmm", State: "working"}}
	fp := map[string]clauded.Session{
		"ezmm": {SessionID: "ezmm", Status: clauded.Waiting, WaitingFor: "permission prompt"},
	}
	overlayFirstParty(sessions, fp)
	if sessions[0].State != "prompt" {
		t.Errorf("state = %q, want prompt (waiting+permission must promote)", sessions[0].State)
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
