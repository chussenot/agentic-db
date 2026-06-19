package daemon

import (
	"os"
	"strconv"
	"time"

	"github.com/mrzor/claude-status/internal/db"
)

// staleThreshold is how long a session may go without any hook heartbeat
// (last_seen_ts) before the GC reaps it. It is the kill -9 safety net: a Claude
// that dies without firing SessionEnd, on a window niri may still report, is
// cleared once it goes quiet this long. The design doc specifies ~10 minutes.
const staleThreshold = 10 * time.Minute

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

// deadPredicate builds the ReapDead predicate from the live window model and the
// current time. A session is dead if ANY of:
//
//   - it has a cached window_id that is absent from the niri model (the window
//     closed), OR
//   - it has a terminal_pid whose /proc/<pid> no longer exists (kitty died), OR
//   - its last_seen_ts is older than staleThreshold (kill -9 / wedged hook).
//
// Sessions with neither a window_id nor a terminal_pid (remote/unresolved) are
// only reaped by the staleness arm — their dots never appear, and the heartbeat
// timeout eventually clears the row.
func deadPredicate(model windowPresence, now time.Time) func(db.Session) bool {
	cutoff := now.Add(-staleThreshold).UnixMilli()
	return func(s db.Session) bool {
		if s.WindowID.Valid && !model.HasWindow(int(s.WindowID.Int64)) {
			return true
		}
		if s.TerminalPID.Valid && !procExists(int(s.TerminalPID.Int64)) {
			return true
		}
		if s.LastSeenTS < cutoff {
			return true
		}
		return false
	}
}
