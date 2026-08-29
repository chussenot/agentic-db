// Package clauded reads Claude Code's first-party per-session status files,
// ~/.claude/sessions/<pid>.json. Each file is written by the Claude Code process
// itself and carries a `status` field — busy / idle / waiting — that updates on
// Claude's own cadence rather than via our hooks. `busy` means it is taking a
// turn, `idle` means a finished turn at rest (observed sitting at `idle` for many
// minutes, so `idle` — not `waiting` — is the steady done state).
//
// `waiting` is OVERLOADED: the main loop reports it whenever it is suspended on
// ANYTHING — a real user-facing prompt as much as an internal subagent/tool wait
// (a `/btw` turn was observed sitting at `waiting` for minutes mid-turn with no
// question). The companion `waitingFor` field names the reason for the wait
// ("permission prompt" for a genuine user prompt), which is what lets callers
// distinguish a real "needs you" from internal blocking — see WaitingFor.
//
// The file also carries the session's IDENTITY, which is how this package knows a
// background agent when it sees one: `kind` is "interactive" or "bg", `name` is the
// session's display name, and a bg session additionally carries `jobId` (the first
// 8 hex of its session id), which keys its richer job record under
// ~/.claude/jobs/<jobId>/. A background agent has no terminal,
// so it never resolves to a niri window; the first-party file is the only place it
// announces itself.
//
// The format is UNDOCUMENTED and may change between Claude versions (identity
// fields observed on v2.1.231, status fields on v2.1.183). Every function here is
// therefore tolerant: unparseable or partial files are skipped, never fatal, so a
// format change degrades to "no first-party data" (callers fall back to
// hook-derived state) rather than breaking.
package clauded

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Status is the first-party status string from a session file. Values other than
// the three constants are preserved verbatim (so an unrecognized future value is
// observable rather than silently coerced).
type Status string

const (
	// Busy: Claude is actively taking a turn.
	Busy Status = "busy"
	// Idle: a finished turn at rest (the steady "done" state; decays on our bar).
	Idle Status = "idle"
	// Waiting: the main loop is suspended. Overloaded — could be a genuine user
	// prompt or an internal subagent/tool wait. Disambiguate via WaitingFor.
	Waiting Status = "waiting"
	// Shell: Claude is passively monitoring a background shell/command.
	Shell Status = "shell"
)

// Known reports whether s is one of the recognized status values.
func (s Status) Known() bool {
	switch s {
	case Busy, Idle, Waiting, Shell:
		return true
	default:
		return false
	}
}

// Kind is the first-party session kind. Values other than the two constants are
// preserved verbatim, so an unrecognized future kind is observable.
type Kind string

const (
	// KindInteractive: a session a human drives through a terminal.
	KindInteractive Kind = "interactive"
	// KindBackground: a background agent (`claude --bg`, a /-command, a colony
	// dispatch). Its own process, its own session id, its own hooks — but no
	// terminal, so it never resolves to a niri window.
	KindBackground Kind = "bg"
)

// Session is the tolerant decode of one ~/.claude/sessions/<pid>.json file.
type Session struct {
	PID             int
	SessionID       string
	Status          Status
	StatusUpdatedAt time.Time // zero if the file omitted it
	Cwd             string
	Version         string
	// WaitingFor names the reason the main loop is suspended when Status is
	// Waiting (e.g. "permission prompt"); empty when absent or not waiting.
	WaitingFor string

	// Identity.
	Kind Kind
	// Name is the session's display name ("project-manager-3"), derived by Claude
	// or set with --name; NameSource says which ("derived" / "user").
	Name       string
	NameSource string
	// JobID keys a background agent's job record at ~/.claude/jobs/<JobID>/ and is
	// the session id's first 8 hex. Empty for interactive sessions.
	JobID string
	// Agent is the agent type a background session was spawned with
	// (`--agent colony-integrator`); empty when spawned without one.
	Agent string
	// ParkedJobID appears on INTERACTIVE sessions only and names the one bg job
	// this session parked. It is a single value, not an inventory of children —
	// do not mistake it for a parent->child link.
	ParkedJobID string
	// StartedAt is when the session began; UpdatedAt is the file's own last write
	// (StatusUpdatedAt is the last *status* change, which can be older).
	StartedAt time.Time
	UpdatedAt time.Time
	// Entrypoint is how Claude was launched ("cli").
	Entrypoint string
	// ProcStart is the owning process's start time as the kernel reports it
	// (/proc/<pid>/stat field 22), copied verbatim as a string. Comparing it
	// against the live /proc value is what distinguishes "same process" from
	// "some other process that reused the pid" — see Alive.
	ProcStart string
}

// IsBackground reports whether this session is itself a background agent.
func (s Session) IsBackground() bool {
	return s.Kind == KindBackground || s.JobID != ""
}

// promptWaitReasons are the observed waitingFor values that name a genuine
// user-facing prompt. ALLOW-LIST, deliberately narrow — extend it only against
// values actually seen in the wild:
//   - "permission prompt": a tool-permission request.
//   - "input needed":      an AskUserQuestion-style prompt. Observed 2026-08-14 on
//     a colony helpdesk background agent blocked on a question.
var promptWaitReasons = []string{"permission", "input needed"}

// IsUserPrompt reports whether this session is genuinely blocked on the user —
// status Waiting with a WaitingFor that names a user-facing prompt. It is an
// ALLOW-LIST keyed on the wait reason, NOT a block-list of internal waits: a
// subagent/tool wait (the `/btw` false positive that motivated dropping the
// blanket waiting->Prompt mapping) is never labeled with one of these reasons, so
// it cannot match here regardless of how it is named.
func (s Session) IsUserPrompt() bool {
	if s.Status != Waiting {
		return false
	}
	reason := strings.ToLower(s.WaitingFor)
	for _, want := range promptWaitReasons {
		if strings.Contains(reason, want) {
			return true
		}
	}
	return false
}

// rawSession mirrors the on-disk JSON. Unknown fields are ignored by the decoder
// so new Claude Code fields never break the read.
type rawSession struct {
	PID             int    `json:"pid"`
	SessionID       string `json:"sessionId"`
	Status          string `json:"status"`
	StatusUpdatedAt int64  `json:"statusUpdatedAt"` // unix milliseconds
	Cwd             string `json:"cwd"`
	Version         string `json:"version"`
	WaitingFor      string `json:"waitingFor"`

	Kind        string `json:"kind"`
	Name        string `json:"name"`
	NameSource  string `json:"nameSource"`
	JobID       string `json:"jobId"`
	Agent       string `json:"agent"`
	ParkedJobID string `json:"parkedJobId"`
	StartedAt   int64  `json:"startedAt"` // unix milliseconds
	UpdatedAt   int64  `json:"updatedAt"` // unix milliseconds
	Entrypoint  string `json:"entrypoint"`
	ProcStart   string `json:"procStart"` // string in the JSON, not a number
}

// DefaultDir returns Claude Code's sessions directory: $CLAUDE_CONFIG_DIR/sessions
// when CLAUDE_CONFIG_DIR is set, else ~/.claude/sessions.
func DefaultDir() string {
	if base := os.Getenv("CLAUDE_CONFIG_DIR"); base != "" {
		return filepath.Join(base, "sessions")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".claude", "sessions")
}

// Read parses every <pid>.json in dir and returns the sessions keyed by
// SessionID. It is tolerant: a missing directory yields an empty map (not an
// error), and individual files that fail to parse or lack a session_id are
// skipped. When two files report the same SessionID (pid reuse / a stale leftover
// file), the one with the newer StatusUpdatedAt wins.
func Read(dir string) (map[string]Session, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]Session{}, nil
		}
		return nil, err
	}
	out := make(map[string]Session, len(entries))
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		s, ok := readFile(filepath.Join(dir, e.Name()))
		if !ok {
			continue
		}
		if prev, dup := out[s.SessionID]; dup && prev.StatusUpdatedAt.After(s.StatusUpdatedAt) {
			continue // keep the fresher of two files for the same session
		}
		out[s.SessionID] = s
	}
	return out, nil
}

// procExists reports whether /proc/<pid> exists (the owning process is alive).
// Package var so tests can stub it; mirrors the daemon's identical seam.
var procExists = func(pid int) bool {
	_, err := os.Stat("/proc/" + strconv.Itoa(pid))
	return err == nil
}

// procStarted returns the process start time the kernel reports for pid
// (/proc/<pid>/stat field 22), in the same units the session file records. ok is
// false when it cannot be read or parsed. Package var so tests can stub it.
var procStarted = func(pid int) (string, bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", false
	}
	return statStartTime(string(data))
}

// ProcStartOf returns the process start time the kernel reports for pid, in the
// same units and format the session files record. Exported so callers that learn a
// pid from somewhere else — the hook, from its own environment — can capture the
// same pid-reuse guard without duplicating the /proc/stat parsing.
func ProcStartOf(pid int) (string, bool) {
	if pid <= 0 {
		return "", false
	}
	return procStarted(pid)
}

// statStartTime pulls field 22 (starttime) out of a /proc/<pid>/stat line. Field 2
// is the executable name in parentheses and may itself contain spaces and
// parentheses, so the fields are counted from after the LAST ')' — the standard
// way to parse this file. Fields 1 and 2 precede it, so starttime is index 19 of
// the remainder.
func statStartTime(stat string) (string, bool) {
	close := strings.LastIndexByte(stat, ')')
	if close < 0 {
		return "", false
	}
	fields := strings.Fields(stat[close+1:])
	const startTimeIdx = 19
	if len(fields) <= startTimeIdx {
		return "", false
	}
	return fields[startTimeIdx], true
}

// Alive reports whether the Claude process that owns this session file is still
// running. Claude Code removes <pid>.json on a clean exit, but a hard kill
// (SIGKILL/crash) leaves the file behind with a now-dead pid frozen at its last
// status; Alive distinguishes that crash-zombie from a live session. A file with
// no usable pid (pid <= 0) is treated as alive — it cannot be checked and must
// not be dropped on a guess.
//
// When the file records a ProcStart, it is compared against the live process's
// start time: a matching pid whose start time differs is a DIFFERENT process that
// merely reused the pid, so the session is dead. Pid reuse is not hypothetical
// here — background agents are claimed from a pre-forked spare pool, so pids churn
// fast. An unreadable live start time is not treated as evidence of death.
func (s Session) Alive() bool {
	if s.PID <= 0 {
		return true
	}
	if !procExists(s.PID) {
		return false
	}
	if s.ProcStart == "" {
		return true
	}
	live, ok := procStarted(s.PID)
	if !ok {
		return true
	}
	return live == s.ProcStart
}

// ReadLive is Read filtered to sessions whose owning process is still alive (see
// Alive). The daemon reads through ReadLive so "present in the first-party set"
// means the file exists AND its Claude process is running: a crash-zombie file
// (present, pid dead) is treated as absent, letting the daemon's
// first-party-absence reap clear it even while its terminal lingers. doctor uses
// raw Read so the zombie stays visible for debugging.
func ReadLive(dir string) (map[string]Session, error) {
	all, err := Read(dir)
	if err != nil {
		return nil, err
	}
	for id, s := range all {
		if !s.Alive() {
			delete(all, id)
		}
	}
	return all, nil
}

// readFile parses one session file. ok is false (skip it) on a read/parse error
// or a missing session_id (nothing to key on).
func readFile(path string) (Session, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Session{}, false
	}
	var r rawSession
	if err := json.Unmarshal(data, &r); err != nil {
		return Session{}, false
	}
	if r.SessionID == "" {
		return Session{}, false
	}
	return Session{
		PID:             r.PID,
		SessionID:       r.SessionID,
		Status:          Status(r.Status),
		StatusUpdatedAt: msTime(r.StatusUpdatedAt),
		Cwd:             r.Cwd,
		Version:         r.Version,
		WaitingFor:      r.WaitingFor,

		Kind:        Kind(r.Kind),
		Name:        r.Name,
		NameSource:  r.NameSource,
		JobID:       r.JobID,
		Agent:       r.Agent,
		ParkedJobID: r.ParkedJobID,
		StartedAt:   msTime(r.StartedAt),
		UpdatedAt:   msTime(r.UpdatedAt),
		Entrypoint:  r.Entrypoint,
		ProcStart:   r.ProcStart,
	}, true
}

// msTime converts unix milliseconds to a time, mapping absent/zero to the zero
// time so callers can test with IsZero.
func msTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}
