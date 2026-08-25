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
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mrzor/claude-status/internal/clauded"
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

// fleetNotifySep splits a FLEET notification — one a session receives ABOUT one of
// its background agents — from an ordinary one. The full form is
// "<agent-name> needs your input: <ask>", e.g.
// "project-manager-3 needs your input: approve Bash: ls docs/…".
//
// These need their own path because they defeat the ordinary prompt filter twice
// over: the ask usually contains "approve"/"confirm", so the pattern match fires,
// and yet the RECEIVING session is not the blocked one — its agent is. Flipping the
// receiver to Prompt would strand a session at "?" for work it cannot do anything
// about from its own turn, while the agent that is actually waiting stays unmarked.
const fleetNotifySep = " needs your input: "

// Environment variables a Claude Code process exports to the children it spawns —
// which includes hook processes, since a hook is a child of the session that fires
// it. Verified on live children of both background and interactive sessions
// (Claude Code 2.1.231).
//
// CLAUDE_PID is the pid of the Claude process that spawned THIS hook. Note it means
// something different one level up: in a background session's own environment the
// same variable names its SPAWNER (see package fleet). Read here, it is always
// "my own Claude".
//
// CLAUDE_JOB_DIR is exported only by background sessions, so its presence is the
// background marker and its basename is the job id. CLAUDE_CODE_SESSION_KIND and
// CLAUDE_CODE_SESSION_NAME are NOT inherited by children — they exist only on the
// background process itself — so the hook cannot read them and does not try; the
// daemon picks the name up from the first-party status file instead.
//
// HERDR_PANE_ID / HERDR_WORKSPACE_ID come from a different producer: herdr, the
// multiplexer that hosts many Claude agents inside ONE terminal window. It injects
// them into every pane it runs, and a hook is a child of the pane's claude, so it
// inherits them — verified on a live tool child of a herdr-hosted claude (the
// child's /proc/<pid>/environ carries HERDR_PANE_ID, HERDR_TAB_ID and
// HERDR_WORKSPACE_ID, identical to its parent's). This is the same free ride
// CLAUDE_JOB_DIR gave us: an exact session<->pane join with no socket roundtrip, no
// file read, and no dependence on herdr's Claude integration being installed.
//
// It matters because a herdr pane's claude is an ORDINARY interactive session —
// kind "interactive", no jobId, no CLAUDE_CODE_SESSION_KIND=bg — so the whole
// background-agent machinery is blind to it, while window_id/terminal_pid are
// shared by every pane of the multiplexer and can no longer tell two agents apart.
// The pane id is what makes a 34-second colony tick attributable to the pane that
// ran it. HERDR_AGENT_NAME is deliberately NOT injected: a child cannot learn its
// own agent name from the environment, so the PANE ID is the join key, not the name.
const (
	envClaudePID        = "CLAUDE_PID"
	envJobDir           = "CLAUDE_JOB_DIR"
	envHerdrPaneID      = "HERDR_PANE_ID"
	envHerdrWorkspaceID = "HERDR_WORKSPACE_ID"
)

// lookupEnv is the environment seam, so tests can drive identity without setting
// process-wide variables.
var lookupEnv = os.Getenv

// listWindows is the niri seam. It exists so a test can COUNT the calls: this
// lookup spawns `niri msg -j windows`, and the hook is the hot path, so "how often
// do we shell out" is a behaviour worth asserting rather than assuming.
var listWindows = niri.ListWindows

// selfIdentity is what the hook can learn about its own session for free, from its
// own environment, with no file reads and no niri IPC.
type selfIdentity struct {
	// Kind is "bg", "interactive", or "" when the environment said nothing (an
	// older Claude Code, or an unexpected spawn path).
	Kind string
	// JobID is set for background sessions only.
	JobID string
	// ClaudePID is this session's Claude process pid — NOT the terminal pid the
	// window walk resolves. It is the liveness anchor for a session that has no
	// window at all.
	ClaudePID int
	// ProcStart is ClaudePID's start time, so a recycled pid cannot later make a
	// dead session look alive.
	ProcStart string
	// HerdrPaneID is the herdr pane hosting this session, e.g. "w1:p8V"; empty
	// when we are not running under herdr. Unlike Kind it says nothing about the
	// session's lifecycle — a herdr pane is interactive — it says WHERE it lives.
	HerdrPaneID string
	// HerdrWorkspaceID is that pane's herdr workspace, e.g. "w1": the level bound
	// to a git checkout, so it is the grouping key for the panes of one herd.
	HerdrWorkspaceID string
}

// isBackground reports whether the environment identified this as a background
// agent.
func (i selfIdentity) isBackground() bool { return i.Kind == "bg" }

// readSelfIdentity derives the session's identity from the hook's environment.
func readSelfIdentity() selfIdentity {
	var id selfIdentity
	if dir := strings.TrimRight(lookupEnv(envJobDir), "/"); dir != "" {
		id.JobID = filepath.Base(dir)
		id.Kind = "bg"
	}
	if v := lookupEnv(envClaudePID); v != "" {
		if pid, err := strconv.Atoi(v); err == nil && pid > 0 {
			id.ClaudePID = pid
			if start, ok := clauded.ProcStartOf(pid); ok {
				id.ProcStart = start
			}
			if id.Kind == "" {
				id.Kind = "interactive"
			}
		}
	}
	// Pane identity, when herdr is the host. Read unconditionally and kept
	// independent of Kind: a herdr pane's claude looks exactly like any other
	// interactive session, so there is no marker to gate this on but the variables
	// themselves.
	id.HerdrPaneID = lookupEnv(envHerdrPaneID)
	id.HerdrWorkspaceID = lookupEnv(envHerdrWorkspaceID)
	return id
}

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

	// Identity from our own environment: free, and the only way a windowless
	// session can be told apart from an ssh/tmux one. Resolve-once, like the
	// window and the repo, so the /proc read for ProcStart happens at most once
	// per session. Upsert merges these with COALESCE, so nothing is lost if the
	// daemon got there first.
	self := readSelfIdentity()
	if ev.HookEventName == "SessionStart" || !s.Kind.Valid || !s.ClaudePID.Valid {
		if self.Kind != "" {
			s.Kind = nullString(self.Kind)
		}
		if self.JobID != "" {
			s.JobID = nullString(self.JobID)
		}
		if self.ClaudePID > 0 {
			s.ClaudePID = sql.NullInt64{Int64: int64(self.ClaudePID), Valid: true}
		}
		if self.ProcStart != "" {
			s.ClaudeProcStart = nullString(self.ProcStart)
		}
		// Same resolve-once gate on purpose, rather than one keyed on
		// !s.HerdrPaneID.Valid: for the overwhelming majority of sessions there is no
		// pane id to find, so a gate of its own would be permanently open and would
		// drag the ProcStart /proc read back onto every single hook. A herdr pane's
		// claude exports CLAUDE_PID, so SessionStart already opens this gate for the
		// sessions that have something to record. Sessions already running when this
		// shipped pick their pane up at their next SessionStart, which for a colony
		// tick is a minute away.
		if self.HerdrPaneID != "" {
			s.HerdrPaneID = nullString(self.HerdrPaneID)
		}
		if self.HerdrWorkspaceID != "" {
			s.HerdrWorkspaceID = nullString(self.HerdrWorkspaceID)
		}
	}

	// Window resolution: only on SessionStart, or lazily when an existing row
	// never got a window_id (covers DB wipes / daemon restarts mid-session).
	//
	// A background agent is skipped outright. It has no terminal, so the walk can
	// only ever end at the background supervisor — and because the lazy branch
	// retries whenever window_id is NULL, which for an agent is FOREVER, the walk
	// was re-running on every single hook. resolveWindow spawns `niri msg -j
	// windows`, so a seven-agent colony streaming PostToolUse was generating a
	// continuous stream of pointless niri IPC. Skipping it is both correct and the
	// cheapest performance fix in this file.
	if !self.isBackground() && (ev.HookEventName == "SessionStart" || !s.WindowID.Valid) {
		if winID, termPID, ok := resolveWindow(startPID); ok {
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
		// A Notification means "Claude wants the user" but covers three cases: a
		// permission/approval request (a genuine prompt), the post-Stop idle
		// nudge, and a FLEET notification about one of this session's background
		// agents. Distinguish the first two by BOTH the message (permission
		// patterns) AND the current state: a real permission request only happens
		// mid-work, so a matching message while the session is idle is the nudge
		// and must NOT strand it at a non-decaying "?". See notifyPatterns.
		//
		// The fleet case is excluded from `matched` entirely: the ask it carries
		// would match the patterns, but the session receiving it is the WATCHER,
		// not the blocked party. It is attributed to the agent instead, below.
		agentName, ask, isFleet := parseFleetNotification(ev.Message)
		if isFleet {
			attributeFleetNotification(database, ev.SessionID, agentName, ask, now)
		}
		matched := !isFleet && isPromptNotification(ev.Message) && prevState != string(state.Idle)
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

// parseFleetNotification splits "<agent-name> needs your input: <ask>". ok is
// false for any other message, including one where either half is empty.
func parseFleetNotification(message string) (name, ask string, ok bool) {
	name, ask, ok = strings.Cut(message, fleetNotifySep)
	if !ok {
		return "", "", false
	}
	name, ask = strings.TrimSpace(name), strings.TrimSpace(ask)
	if name == "" || ask == "" {
		return "", "", false
	}
	// The name is a session name, not prose: a message that merely happens to
	// contain the separator mid-sentence is not a fleet notification.
	if strings.ContainsAny(name, " \t") {
		return "", "", false
	}
	return name, ask, true
}

// attributeFleetNotification records a fleet notification against the AGENT it is
// about, rather than against the session that received it.
//
// Two things are written, both best-effort (an attribution failure must never fail
// the hook):
//
//   - the agent's console edge, i.e. "this session was told you need input". That
//     is the desktop a human would go to in order to answer, and it is genuinely
//     not the same session that spawned the fleet: on the machine this was built
//     for, a fleet spawned from one desktop was being watched from another, whose
//     tile cheerfully reported 19h idle.
//   - an audit row keyed to the AGENT, so `events --session <agent>` finally shows
//     which agent asked and what for. Before this, the ask was recorded only
//     against the watcher with matched=false, and the agent's own log said nothing.
//
// The agent's own state is deliberately left alone: its first-party status file and
// its job's `needs` already describe what it wants, and those are durable, whereas
// a notification is a one-shot event that a later PostToolUse would clobber.
func attributeFleetNotification(database *db.DB, receiverID, agentName, ask string, now int64) {
	agent, found, err := database.GetByName(agentName)
	if err != nil || !found || agent.SessionID == receiverID {
		return
	}
	_, _ = database.SetIdentity(agent.SessionID, db.Identity{
		ConsoleSessionID: &receiverID,
		ConsoleTS:        &now,
	})
	_ = database.InsertEvent(db.Event{
		TS:        now,
		SessionID: agent.SessionID,
		Event:     "FleetNotification",
		Message:   nullString(truncate(ask, 300)),
		Matched:   sql.NullBool{Bool: true, Valid: true},
		NewState:  "unchanged",
	})
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
	windows, err := listWindows()
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
