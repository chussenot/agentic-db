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
	"github.com/mrzor/claude-status/internal/git"
	"github.com/mrzor/claude-status/internal/niri"
	"github.com/mrzor/claude-status/internal/state"
)

// notifyPatterns is the tunable list of case-insensitive substrings that mark a
// Notification as a genuine permission/approval request — the only kind of
// Notification that means Claude is BLOCKED waiting on the user. It deliberately
// EXCLUDES the idle-nudge text ("waiting for your input"): that fires ~60s after
// Stop while the session is already idle and decaying, and treating it as a
// prompt stranded the session at a steady "?" that never decayed (the bug this
// list once caused). Notification fires for plenty of other things too (e.g.
// "build finished", "Claude Code login successful"), so a message flips the
// session into Prompt only when it contains one of these AND the session is
// mid-work (see the state gate in run). Everything else records the notification
// but leaves the prior status untouched. Retune by editing this list.
var notifyPatterns = []string{
	"permission",
	"approve",
	"confirm",
}

// maxProcHops caps the /proc PPid ancestor walk so a pathological or cyclic
// chain can never spin (the real tree is zsh -> claude -> zsh -> kitty ->
// systemd, ~4 hops).
const maxProcHops = 20

// envJobDir is exported by Claude Code to BACKGROUND sessions only (claude
// --bg and kin) and inherited by their children, this hook included — so its
// presence is the background marker, readable for free with no file reads and
// no IPC. A background agent has no terminal and thus no niri window to
// resolve, ever.
const envJobDir = "CLAUDE_JOB_DIR"

// lookupEnv is the environment seam, so tests can mark the session background
// without mutating process-wide state.
var lookupEnv = os.Getenv

// listWindows is the niri seam. It exists so a test can COUNT the calls: this
// lookup spawns `niri msg -j windows`, and the hook is the hot path, so "how
// often do we shell out" is a behaviour worth asserting rather than assuming.
var listWindows = niri.ListWindows

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
	//
	// A background agent is skipped outright. It has no terminal, so the walk
	// could only ever end at the background supervisor — and because the lazy
	// branch retries whenever window_id is NULL, which for an agent is FOREVER,
	// resolution would re-run on every single hook. resolveWindow spawns
	// `niri msg -j windows`, so a busy multi-agent setup would stream pointless
	// niri IPC on the hot path. Skipping is both correct and cheap.
	if lookupEnv(envJobDir) == "" && (ev.HookEventName == "SessionStart" || !s.WindowID.Valid) {
		if winID, termPID, ok := resolveWindow(startPID, ev.Cwd); ok {
			s.WindowID = sql.NullInt64{Int64: int64(winID), Valid: true}
			s.TerminalPID = sql.NullInt64{Int64: int64(termPID), Valid: true}
		}
		// If resolution fails (remote/tmux/ssh, or niri unavailable) we leave
		// both NULL and continue — degrade gracefully.
	}

	// Repo resolution: gated exactly like window_id — resolve once (on SessionStart,
	// or lazily whenever repo_id is still NULL, which covers a session that started
	// outside a work tree and later cd'd into one), then never shell out to git
	// again for this session. Best-effort: any failure leaves repo_id NULL and the
	// recap falls back to its cwd heuristic. session_repos is the durable record
	// (survives the SessionEnd that deletes this row); repo_id is just the cache.
	if ev.HookEventName == "SessionStart" || !s.RepoID.Valid {
		if remote, root, branch, ok := git.Resolve(ev.Cwd); ok {
			if id, rerr := database.UpsertRepo(remote, root, now); rerr == nil {
				s.RepoID = sql.NullInt64{Int64: id, Valid: true}
				_ = database.LinkSessionRepo(ev.SessionID, id, nullString(branch), now)
			}
		}
	}

	// Status change. Notification is special: MapEvent always reports Prompt,
	// but only permission/idle messages actually qualify (see notifyPatterns).
	prevState := s.State
	if ev.HookEventName == "Notification" {
		// A Notification means "Claude wants the user" but covers two cases: a
		// permission/approval request (a genuine prompt) and the post-Stop idle
		// nudge. Distinguish them by BOTH the message (permission patterns) AND the
		// current state: a real permission request only happens mid-work, so a
		// matching message while the session is idle is the nudge and must NOT
		// strand it at a non-decaying "?". See notifyPatterns.
		matched := isPromptNotification(ev.Message) && prevState != string(state.Idle)
		if matched {
			s.NotifyKind = nullString("prompt")
		}
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

func nullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// clientComm is the /proc comm of a terminal-side Claude Code client process.
// Daemon-spawned session workers re-exec a versioned binary, so their comm is
// the version string (e.g. "2.1.215") — the comm filter alone separates the
// in-terminal client from daemon-side processes.
const clientComm = "claude"

// resolveWindow maps this hook invocation to a niri window, trying two
// strategies in order:
//
//  1. Ancestry: walk the /proc PPid chain upward from startPID and match each
//     ancestor (nearest first) against the niri window pids. This works when
//     the Claude process is a descendant of the terminal (pre-daemon Claude).
//  2. Client cwd: daemonized Claude (observed v2.1.215) reparents session
//     processes under the per-user `claude daemon` (itself under systemd), so
//     strategy 1 dead-ends at pid 1. The terminal-side client is still a
//     descendant of its terminal, so find `claude` client processes whose cwd
//     equals the session's cwd and walk THEIR ancestry instead. Binds only on
//     an unambiguous match — see resolveClientWindow.
//
// ok is false when neither strategy finds a window (remote/tmux/ssh, detached
// background session, ambiguity) or niri is unavailable.
func resolveWindow(startPID int, cwd string) (windowID, terminalPID int, ok bool) {
	windows, err := listWindows()
	if err != nil || len(windows) == 0 {
		return 0, 0, false
	}
	byPID := make(map[int]niri.Window, len(windows))
	for _, w := range windows {
		byPID[w.PID] = w
	}
	for _, pid := range ancestorPIDs(startPID) {
		if w, found := byPID[pid]; found {
			return w.ID, w.PID, true
		}
	}
	return resolveClientWindow(clientPIDs(clientComm, cwd), byPID)
}

// resolveClientWindow maps candidate client pids to niri windows via their
// /proc ancestry. It binds only when every candidate that reaches a window
// reaches the SAME window: two claude clients in the same cwd but different
// windows are ambiguous, and a wrong binding is worse than a NULL one (the
// pre-fallback status quo). Candidates whose ancestry reaches no window (the
// daemon itself, a client inside tmux) are skipped, not ambiguity.
// ponytail: same-cwd clients in different windows stay NULL; disambiguate by
// process start time vs session created_ts if that ever bites.
func resolveClientWindow(pids []int, byPID map[int]niri.Window) (windowID, terminalPID int, ok bool) {
	var match niri.Window
	var found bool
	for _, pid := range pids {
		for _, anc := range ancestorPIDs(pid) {
			w, isWin := byPID[anc]
			if !isWin {
				continue
			}
			if found && w.ID != match.ID {
				return 0, 0, false
			}
			match, found = w, true
			break
		}
	}
	if !found {
		return 0, 0, false
	}
	return match.ID, match.PID, true
}

// clientPIDs scans /proc for processes whose comm equals comm and whose cwd
// symlink equals cwd. Unreadable entries are skipped (processes may exit
// mid-scan). The scan reads two small files per pid and only runs while a
// session has no window_id, so it stays well inside the hook's latency budget.
func clientPIDs(comm, cwd string) []int {
	if cwd == "" {
		return nil
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		c, err := os.ReadFile("/proc/" + e.Name() + "/comm")
		if err != nil || strings.TrimSpace(string(c)) != comm {
			continue
		}
		link, err := os.Readlink("/proc/" + e.Name() + "/cwd")
		if err != nil || link != cwd {
			continue
		}
		out = append(out, pid)
	}
	return out
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
