// Package hook implements the `claude-status hook` subcommand — the hot path
// invoked by every Claude Code hook. It reads a hook JSON event from stdin,
// derives the new session state (via internal/state), resolves the niri window
// on SessionStart (via internal/niri), and upserts one row (via internal/db).
//
// The hook is the HOT PATH: it must be fast (<50ms) and must NEVER block or
// fail Claude. Run swallows every error (logging it to a ring-buffered file
// next to the DB) and always returns nil, so the process exits 0 in all cases.
package hook

import (
	"database/sql"
	"encoding/json"
	"flag"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/mrzor/claude-status/internal/db"
	"github.com/mrzor/claude-status/internal/niri"
	"github.com/mrzor/claude-status/internal/state"
)

// notifyPatterns is the tunable list of case-insensitive substrings that mark a
// Notification as a genuine "Claude needs the user" prompt (permission request
// or idle nudge). Notification fires for plenty of other things (e.g. "build
// finished"), so only messages containing one of these flip the session into
// the Prompt state. Everything else records the notification but leaves the
// prior status untouched. Retune by editing this list.
var notifyPatterns = []string{
	"permission",
	"waiting for your input",
	"needs your",
	"approve",
	"confirm",
}

// maxProcHops caps the /proc PPid ancestor walk so a pathological or cyclic
// chain can never spin (the real tree is zsh -> claude -> zsh -> kitty ->
// systemd, ~4 hops).
const maxProcHops = 20

// hookEvent is the tolerant decode of the hook JSON delivered on stdin. Unknown
// fields are ignored so future Claude Code additions don't break the hook.
type hookEvent struct {
	SessionID     string `json:"session_id"`
	Cwd           string `json:"cwd"`
	HookEventName string `json:"hook_event_name"`
	Message       string `json:"message"`
}

// Run executes the hook subcommand. args is os.Args[2:]. It wires stdin and the
// real process pid into the core, and ALWAYS returns nil: any error is logged to
// the ring buffer and swallowed so the hook never blocks or fails Claude.
func Run(args []string) error {
	dbPath := db.DefaultDBPath()
	fs := flag.NewFlagSet("hook", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&dbPath, "db", dbPath, "path to the claude-status sqlite database")
	// Parse errors are non-fatal: fall back to defaults and carry on, because
	// the hook must never fail Claude over a bad flag.
	_ = fs.Parse(args)

	if err := run(os.Stdin, dbPath, os.Getppid()); err != nil {
		logError(dbPath, err)
	}
	return nil
}

// run is the testable core: it reads the hook JSON from r, applies the event to
// the row for that session in the database at dbPath, and resolves the niri
// window starting the /proc walk from startPID. It returns an error for the
// caller to log; it never writes to the log itself (so tests can assert errors).
func run(r io.Reader, dbPath string, startPID int) error {
	raw, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	var ev hookEvent
	// Tolerant parse: ignore unmarshal errors on a best-effort basis only if we
	// got nothing usable. A malformed body with no session_id is a no-op.
	if err := json.Unmarshal(raw, &ev); err != nil {
		return err
	}
	if ev.SessionID == "" {
		// Nothing to key on; treat as a no-op (still exit 0).
		return nil
	}

	database, err := db.Open(dbPath)
	if err != nil {
		return err
	}
	defer database.Close()

	now := db.Now().UnixMilli()
	t := state.MapEvent(ev.HookEventName)

	// Audit scaffold: one bounded-log row per invocation, filled in per path.
	// Best-effort — an audit write must never fail the hook.
	evt := db.Event{TS: now, SessionID: ev.SessionID, Event: ev.HookEventName}

	// SessionEnd: clean teardown, delete the row and stop.
	if t.Delete {
		evt.NewState = "deleted"
		_ = database.InsertEvent(evt)
		return database.Delete(ev.SessionID)
	}

	existing, found, err := database.Get(ev.SessionID)
	if err != nil {
		return err
	}
	if found {
		evt.WindowID = existing.WindowID
	}

	// Unknown event: still bump last_seen for an existing row (liveness
	// heartbeat), otherwise no-op. Never create a row for an unknown event.
	if !t.Known {
		if found {
			existing.LastSeenTS = now
			evt.NewState = "unchanged"
			_ = database.InsertEvent(evt)
			return database.Upsert(existing)
		}
		evt.NewState = "ignored"
		_ = database.InsertEvent(evt)
		return nil
	}

	// Build the next row, preserving cached fields from the existing one.
	var s db.Session
	if found {
		s = existing
	} else {
		s.SessionID = ev.SessionID
		s.CreatedTS = now
		// A brand-new session must have a NOT NULL state even before any status
		// change is applied below.
		s.State = string(state.Idle)
	}

	s.Cwd = nullString(ev.Cwd)
	s.LastSeenTS = now
	s.LastEventTS = sql.NullInt64{Int64: now, Valid: true}
	if t.BumpTalk {
		s.LastTalkTS = sql.NullInt64{Int64: now, Valid: true}
	}

	// Window resolution: only on SessionStart, or lazily when an existing row
	// never got a window_id (covers DB wipes / daemon restarts mid-session).
	if ev.HookEventName == "SessionStart" || !s.WindowID.Valid {
		if winID, termPID, ok := resolveWindow(startPID); ok {
			s.WindowID = sql.NullInt64{Int64: int64(winID), Valid: true}
			s.TerminalPID = sql.NullInt64{Int64: int64(termPID), Valid: true}
		}
		// If resolution fails (remote/tmux/ssh, or niri unavailable) we leave
		// both NULL and continue — degrade gracefully.
	}

	// Status change. Notification is special: MapEvent always reports Prompt,
	// but only permission/idle messages actually qualify (see notifyPatterns).
	prevState := s.State
	if ev.HookEventName == "Notification" {
		matched := isPromptNotification(ev.Message)
		s.NotifyKind = nullString(classifyNotify(ev.Message))
		evt.Message = nullString(truncate(ev.Message, 300))
		evt.Matched = sql.NullBool{Bool: matched, Valid: true}
		if matched {
			s.State = string(state.Prompt)
		}
		// Non-qualifying notifications leave the prior state untouched.
	} else if t.ChangeStatus {
		s.State = string(t.NewStatus)
	}

	// Record the audit row. For a brand-new session record the established state
	// (prevState is only the construction default, so a first Stop would falsely
	// read as "unchanged"); for an existing row, "unchanged" when the event
	// didn't move it. evt.WindowID reflects the (possibly just-resolved) window.
	evt.WindowID = s.WindowID
	switch {
	case !found:
		evt.NewState = s.State
	case s.State == prevState:
		evt.NewState = "unchanged"
	default:
		evt.NewState = s.State
	}
	_ = database.InsertEvent(evt)

	return database.Upsert(s)
}

// truncate caps a string at n bytes (Notification messages can be long; the
// audit log only needs enough to recognize what fired).
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// isPromptNotification reports whether a Notification message matches one of the
// permission/idle patterns that mean Claude is genuinely waiting for the user.
func isPromptNotification(message string) bool {
	m := strings.ToLower(message)
	for _, p := range notifyPatterns {
		if strings.Contains(m, p) {
			return true
		}
	}
	return false
}

// classifyNotify returns a short notify_kind classification for the message, or
// "" for a non-qualifying notification.
func classifyNotify(message string) string {
	if isPromptNotification(message) {
		return "prompt"
	}
	return ""
}

func nullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// resolveWindow walks the /proc PPid chain upward from startPID, collecting
// ancestor pids, then matches them (nearest ancestor first) against the niri
// window list. The first ancestor pid that is a window's PID yields that
// window's id (window_id) and pid (terminal_pid). ok is false when no ancestor
// maps to a niri window (remote/tmux/ssh) or niri is unavailable.
func resolveWindow(startPID int) (windowID, terminalPID int, ok bool) {
	ancestors := ancestorPIDs(startPID)
	if len(ancestors) == 0 {
		return 0, 0, false
	}
	windows, err := niri.ListWindows()
	if err != nil || len(windows) == 0 {
		return 0, 0, false
	}
	byPID := make(map[int]niri.Window, len(windows))
	for _, w := range windows {
		byPID[w.PID] = w
	}
	for _, pid := range ancestors {
		if w, found := byPID[pid]; found {
			return w.ID, w.PID, true
		}
	}
	return 0, 0, false
}

// ancestorPIDs returns startPID followed by its /proc PPid ancestors, nearest
// first, stopping at pid 1, at a missing/unreadable /proc entry, or after
// maxProcHops. It is pure (reads only /proc) so it can be unit-tested against
// /proc/self without niri.
func ancestorPIDs(startPID int) []int {
	var out []int
	pid := startPID
	for hop := 0; hop < maxProcHops; hop++ {
		if pid <= 1 {
			out = append(out, pid)
			break
		}
		out = append(out, pid)
		ppid, ok := parentPID(pid)
		if !ok {
			break
		}
		pid = ppid
	}
	return out
}

// parentPID reads /proc/<pid>/status and returns the PPid field. ok is false if
// the file is missing/unreadable or has no PPid line.
func parentPID(pid int) (int, bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		rest, found := strings.CutPrefix(line, "PPid:")
		if !found {
			continue
		}
		ppid, perr := strconv.Atoi(strings.TrimSpace(rest))
		if perr != nil {
			return 0, false
		}
		return ppid, true
	}
	return 0, false
}
