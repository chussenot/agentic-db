package hook

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mrzor/claude-status/internal/db"
	"github.com/mrzor/claude-status/internal/state"
)

// fixedClock installs a deterministic db.Now for the duration of a test and
// restores the previous value on cleanup. Returned ts is the unix-ms the clock
// reports.
func fixedClock(t *testing.T, when time.Time) int64 {
	t.Helper()
	prev := db.Now
	db.Now = func() time.Time { return when }
	t.Cleanup(func() { db.Now = prev })
	return when.UnixMilli()
}

// openTemp returns a DB at a fresh temp path plus that path.
func tempDBPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "claude.sqlite")
}

// drive feeds a JSON body through the testable core against dbPath, using a
// startPID of 1 so window resolution never matches a niri window (keeps tests
// hermetic). It fails the test on a core error.
func drive(t *testing.T, dbPath, body string) {
	t.Helper()
	if err := run(strings.NewReader(body), dbPath, 1); err != nil {
		t.Fatalf("run(%q): %v", body, err)
	}
}

func getRow(t *testing.T, dbPath, sessionID string) (db.Session, bool) {
	t.Helper()
	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer database.Close()
	s, found, err := database.Get(sessionID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	return s, found
}

func TestEventStateMapping(t *testing.T) {
	tests := []struct {
		name        string
		event       string
		wantState   string
		wantBumpTLK bool // last_talk_ts should be set
	}{
		{"SessionStart", "SessionStart", string(state.Idle), true},
		{"UserPromptSubmit", "UserPromptSubmit", string(state.Working), false},
		{"PostToolUse", "PostToolUse", string(state.Working), false},
		{"Stop", "Stop", string(state.Idle), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dbPath := tempDBPath(t)
			ts := fixedClock(t, time.UnixMilli(1_700_000_000_000))
			drive(t, dbPath, `{"session_id":"s1","cwd":"/tmp","hook_event_name":"`+tc.event+`"}`)

			s, found := getRow(t, dbPath, "s1")
			if !found {
				t.Fatalf("row not found after %s", tc.event)
			}
			if s.State != tc.wantState {
				t.Errorf("state = %q, want %q", s.State, tc.wantState)
			}
			if s.LastSeenTS != ts {
				t.Errorf("last_seen_ts = %d, want %d", s.LastSeenTS, ts)
			}
			if !s.LastEventTS.Valid || s.LastEventTS.Int64 != ts {
				t.Errorf("last_event_ts = %v, want %d", s.LastEventTS, ts)
			}
			if tc.wantBumpTLK {
				if !s.LastTalkTS.Valid || s.LastTalkTS.Int64 != ts {
					t.Errorf("last_talk_ts = %v, want %d", s.LastTalkTS, ts)
				}
			} else if s.LastTalkTS.Valid {
				t.Errorf("last_talk_ts = %v, want NULL", s.LastTalkTS)
			}
			if !s.Cwd.Valid || s.Cwd.String != "/tmp" {
				t.Errorf("cwd = %v, want /tmp", s.Cwd)
			}
		})
	}
}

func TestSubagentStopKeepsStatusBumpsTalk(t *testing.T) {
	dbPath := tempDBPath(t)
	fixedClock(t, time.UnixMilli(1_700_000_000_000))
	// Start working, then SubagentStop: status should stay working, talk bumps.
	drive(t, dbPath, `{"session_id":"s1","hook_event_name":"UserPromptSubmit"}`)
	ts2 := fixedClock(t, time.UnixMilli(1_700_000_005_000))
	drive(t, dbPath, `{"session_id":"s1","hook_event_name":"SubagentStop"}`)

	s, found := getRow(t, dbPath, "s1")
	if !found {
		t.Fatal("row missing")
	}
	if s.State != string(state.Working) {
		t.Errorf("state = %q, want working (unchanged)", s.State)
	}
	if !s.LastTalkTS.Valid || s.LastTalkTS.Int64 != ts2 {
		t.Errorf("last_talk_ts = %v, want %d", s.LastTalkTS, ts2)
	}
}

func TestSessionEndDeletes(t *testing.T) {
	dbPath := tempDBPath(t)
	fixedClock(t, time.UnixMilli(1_700_000_000_000))
	drive(t, dbPath, `{"session_id":"s1","hook_event_name":"SessionStart"}`)
	if _, found := getRow(t, dbPath, "s1"); !found {
		t.Fatal("row should exist before SessionEnd")
	}
	drive(t, dbPath, `{"session_id":"s1","hook_event_name":"SessionEnd"}`)
	if _, found := getRow(t, dbPath, "s1"); found {
		t.Error("row should be deleted after SessionEnd")
	}
}

func TestUnknownEventBumpsExistingOnly(t *testing.T) {
	dbPath := tempDBPath(t)
	fixedClock(t, time.UnixMilli(1_700_000_000_000))

	// Unknown event with no existing row: no-op (no row created).
	drive(t, dbPath, `{"session_id":"ghost","hook_event_name":"Bogus"}`)
	if _, found := getRow(t, dbPath, "ghost"); found {
		t.Error("unknown event should not create a row")
	}

	// Now create a row, then unknown event should bump last_seen, keep state.
	drive(t, dbPath, `{"session_id":"s1","hook_event_name":"UserPromptSubmit"}`)
	ts2 := fixedClock(t, time.UnixMilli(1_700_000_009_000))
	drive(t, dbPath, `{"session_id":"s1","hook_event_name":"Bogus"}`)

	s, found := getRow(t, dbPath, "s1")
	if !found {
		t.Fatal("row missing")
	}
	if s.State != string(state.Working) {
		t.Errorf("state = %q, want working (unchanged by unknown event)", s.State)
	}
	if s.LastSeenTS != ts2 {
		t.Errorf("last_seen_ts = %d, want %d (bumped by unknown event)", s.LastSeenTS, ts2)
	}
	// last_event_ts must NOT be bumped by an unknown event.
	if !s.LastEventTS.Valid || s.LastEventTS.Int64 == ts2 {
		t.Errorf("last_event_ts = %v, should not be bumped to %d", s.LastEventTS, ts2)
	}
}

func TestNotificationFiltering(t *testing.T) {
	tests := []struct {
		name       string
		message    string
		wantState  string
		wantNotify bool // notify_kind == "prompt"
	}{
		{"permission", "Claude needs your permission to run a command", string(state.Prompt), true},
		{"waiting", "Claude is waiting for your input", string(state.Prompt), true},
		{"approve", "Approve this action?", string(state.Prompt), true},
		{"confirm", "Please confirm", string(state.Prompt), true},
		{"build finished", "build finished successfully", string(state.Working), false},
		{"empty", "", string(state.Working), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dbPath := tempDBPath(t)
			fixedClock(t, time.UnixMilli(1_700_000_000_000))
			// Prior state is working so we can see whether Notification changes it.
			drive(t, dbPath, `{"session_id":"s1","hook_event_name":"UserPromptSubmit"}`)
			fixedClock(t, time.UnixMilli(1_700_000_001_000))
			body := `{"session_id":"s1","hook_event_name":"Notification","message":` +
				strconv.Quote(tc.message) + `}`
			drive(t, dbPath, body)

			s, found := getRow(t, dbPath, "s1")
			if !found {
				t.Fatal("row missing")
			}
			if s.State != tc.wantState {
				t.Errorf("state = %q, want %q", s.State, tc.wantState)
			}
			if tc.wantNotify {
				if !s.NotifyKind.Valid || s.NotifyKind.String != "prompt" {
					t.Errorf("notify_kind = %v, want prompt", s.NotifyKind)
				}
			} else if s.NotifyKind.Valid && s.NotifyKind.String != "" {
				t.Errorf("notify_kind = %v, want empty/NULL", s.NotifyKind)
			}
		})
	}
}

func TestNotificationCaseInsensitive(t *testing.T) {
	dbPath := tempDBPath(t)
	fixedClock(t, time.UnixMilli(1_700_000_000_000))
	drive(t, dbPath, `{"session_id":"s1","hook_event_name":"UserPromptSubmit"}`)
	drive(t, dbPath, `{"session_id":"s1","hook_event_name":"Notification","message":"PERMISSION REQUIRED"}`)
	s, _ := getRow(t, dbPath, "s1")
	if s.State != string(state.Prompt) {
		t.Errorf("state = %q, want prompt (case-insensitive match)", s.State)
	}
}

func TestEmptySessionIDIsNoOp(t *testing.T) {
	dbPath := tempDBPath(t)
	fixedClock(t, time.UnixMilli(1_700_000_000_000))
	// Should not error and should not create any row.
	drive(t, dbPath, `{"hook_event_name":"SessionStart"}`)
	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer database.Close()
	rows, err := database.LoadLive()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected no rows, got %d", len(rows))
	}
}

func TestMalformedJSONReturnsError(t *testing.T) {
	dbPath := tempDBPath(t)
	if err := run(strings.NewReader(`{not json`), dbPath, 1); err == nil {
		t.Error("expected error on malformed JSON (Run will swallow it)")
	}
}

func TestUnknownFieldsIgnored(t *testing.T) {
	dbPath := tempDBPath(t)
	fixedClock(t, time.UnixMilli(1_700_000_000_000))
	drive(t, dbPath, `{"session_id":"s1","hook_event_name":"Stop","transcript_path":"/x","extra":42}`)
	s, found := getRow(t, dbPath, "s1")
	if !found || s.State != string(state.Idle) {
		t.Errorf("unknown fields should be ignored; got found=%v state=%q", found, s.State)
	}
}

func TestWindowUnresolvedStaysNull(t *testing.T) {
	dbPath := tempDBPath(t)
	fixedClock(t, time.UnixMilli(1_700_000_000_000))
	// startPID=1 means the only ancestor is pid 1, which won't match a niri
	// window (and niri may be absent in CI) -> window_id/terminal_pid NULL.
	drive(t, dbPath, `{"session_id":"s1","hook_event_name":"SessionStart"}`)
	s, _ := getRow(t, dbPath, "s1")
	if s.WindowID.Valid {
		t.Errorf("window_id = %v, want NULL when unresolved", s.WindowID)
	}
	if s.TerminalPID.Valid {
		t.Errorf("terminal_pid = %v, want NULL when unresolved", s.TerminalPID)
	}
}

func TestCreatedTSPreservedAcrossEvents(t *testing.T) {
	dbPath := tempDBPath(t)
	created := fixedClock(t, time.UnixMilli(1_700_000_000_000))
	drive(t, dbPath, `{"session_id":"s1","hook_event_name":"SessionStart"}`)
	fixedClock(t, time.UnixMilli(1_700_000_050_000))
	drive(t, dbPath, `{"session_id":"s1","hook_event_name":"Stop"}`)
	s, _ := getRow(t, dbPath, "s1")
	if s.CreatedTS != created {
		t.Errorf("created_ts = %d, want preserved %d", s.CreatedTS, created)
	}
}

// --- /proc walker tests (no niri required) ---------------------------------

func TestAncestorPIDsSelfChain(t *testing.T) {
	// Walking from our own pid must include us and terminate at pid 1.
	self := os.Getpid()
	chain := ancestorPIDs(self)
	if len(chain) == 0 {
		t.Fatal("ancestor chain empty")
	}
	if chain[0] != self {
		t.Errorf("chain[0] = %d, want self %d", chain[0], self)
	}
	if last := chain[len(chain)-1]; last != 1 {
		t.Errorf("chain does not terminate at pid 1; last = %d", last)
	}
	if len(chain) > maxProcHops {
		t.Errorf("chain length %d exceeds cap %d", len(chain), maxProcHops)
	}
}

func TestAncestorPIDsParentMatchesGetppid(t *testing.T) {
	chain := ancestorPIDs(os.Getpid())
	if len(chain) < 2 {
		t.Skip("no parent in chain (unusual environment)")
	}
	if chain[1] != os.Getppid() {
		t.Errorf("chain[1] = %d, want getppid %d", chain[1], os.Getppid())
	}
}

func TestParentPIDMissingProc(t *testing.T) {
	// pid 0 has no /proc entry -> not ok.
	if _, ok := parentPID(0); ok {
		t.Error("parentPID(0) should fail (no /proc/0)")
	}
}

func TestResolveWindowDegradesWhenNoMatch(t *testing.T) {
	// startPID=1: even if niri is present, pid 1 (systemd) is not a window.
	if _, _, ok := resolveWindow(1); ok {
		t.Error("resolveWindow(1) should not match a niri window")
	}
}

// --- log ring buffer -------------------------------------------------------

func TestLogErrorWritesAndRotates(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "claude.sqlite")
	logPath := filepath.Join(dir, logFileName)

	fixedClock(t, time.UnixMilli(1_700_000_000_000))
	logError(dbPath, errString("boom"))

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(data), "boom") {
		t.Errorf("log missing message: %q", string(data))
	}

	// Force rotation: pad the log past the cap, then log again.
	if err := os.WriteFile(logPath, make([]byte, maxLogBytes+1), 0o644); err != nil {
		t.Fatalf("pad: %v", err)
	}
	logError(dbPath, errString("after-rotate"))

	if _, err := os.Stat(logPath + ".1"); err != nil {
		t.Errorf("expected rotated file %s.1: %v", logPath, err)
	}
	cur, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read rotated log: %v", err)
	}
	if !strings.Contains(string(cur), "after-rotate") {
		t.Errorf("current log should hold latest entry, got %q", string(cur))
	}
}

func TestLogErrorNilIsNoop(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "claude.sqlite")
	logError(dbPath, nil)
	if _, err := os.Stat(filepath.Join(dir, logFileName)); err == nil {
		t.Error("nil error should not create a log file")
	}
}

// errString is a tiny error type for the log tests.
type errString string

func (e errString) Error() string { return string(e) }
