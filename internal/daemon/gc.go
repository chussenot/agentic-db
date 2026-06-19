package daemon

import (
	"os"
	"strconv"

	"github.com/mrzor/claude-status/internal/db"
)

// windowPresence answers whether the niri model currently knows a window id.
// The live *niri.Model satisfies it; tests inject a fake.
type windowPresence interface {
	HasWindow(windowID int) bool
}

// procExists reports whether /proc/<pid> exists (the process is alive). It is a
// package var so tests can stub it without spawning real processes.
var procExists = func(pid int) bool {
	_, err := os.Stat("/proc/" + strconv.Itoa(pid))
	return err == nil
}

// deadPredicate builds the ReapDead predicate from the live window model. Every
// session we track is LOCAL — the hook always resolves a niri window + terminal
// (kitty) pid — so liveness is ground truth: is the terminal still there? A
// session is dead if EITHER:
//
//   - its cached window_id is absent from the niri model (the terminal window
//     closed), OR
//   - its terminal_pid no longer has a /proc/<pid> (the kitty process died).
//
// Window presence is the authoritative signal (niri owns the window list); the
// /proc check is cheap backup for the brief interval where niri might still
// list a just-closed window. Closing the terminal trips both.
//
// There is deliberately NO heartbeat/last_seen staleness reap: an idle Claude
// emits no hooks, so a timeout would delete it mid-decay. Idle sessions instead
// persist (fading to dim ░░ and resting there) until their terminal closes, or
// until SessionEnd deletes the row on a clean exit. last_seen_ts remains in the
// schema purely as a heartbeat/debug field, not a reap trigger.
func deadPredicate(model windowPresence) func(db.Session) bool {
	return func(s db.Session) bool {
		if s.WindowID.Valid && !model.HasWindow(int(s.WindowID.Int64)) {
			return true
		}
		if s.TerminalPID.Valid && !procExists(int(s.TerminalPID.Int64)) {
			return true
		}
		return false
	}
}
