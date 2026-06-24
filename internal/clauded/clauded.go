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
// The format is UNDOCUMENTED and may change between Claude versions (observed on
// v2.1.183). Every function here is therefore tolerant: unparseable or partial
// files are skipped, never fatal, so a format change degrades to "no first-party
// data" (callers fall back to hook-derived state) rather than breaking.
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
}

// IsUserPrompt reports whether this session is genuinely blocked on the user —
// status Waiting with a WaitingFor that names a user-facing prompt. It is an
// ALLOW-LIST keyed on the wait reason naming a "permission" prompt, NOT a
// block-list of internal waits: a subagent/tool wait (the `/btw` false positive
// that motivated dropping the blanket waiting->Prompt mapping) is never labeled
// "permission prompt", so it cannot match here regardless of how it is named.
// The allow-list is deliberately narrow; broaden it only against observed
// waitingFor values, never speculatively.
func (s Session) IsUserPrompt() bool {
	return s.Status == Waiting &&
		strings.Contains(strings.ToLower(s.WaitingFor), "permission")
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

// Alive reports whether the Claude process that owns this session file is still
// running. Claude Code removes <pid>.json on a clean exit, but a hard kill
// (SIGKILL/crash) leaves the file behind with a now-dead pid frozen at its last
// status; Alive distinguishes that crash-zombie from a live session. A file with
// no usable pid (pid <= 0) is treated as alive — it cannot be checked and must
// not be dropped on a guess.
func (s Session) Alive() bool {
	if s.PID <= 0 {
		return true
	}
	return procExists(s.PID)
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
	var updated time.Time
	if r.StatusUpdatedAt > 0 {
		updated = time.UnixMilli(r.StatusUpdatedAt)
	}
	return Session{
		PID:             r.PID,
		SessionID:       r.SessionID,
		Status:          Status(r.Status),
		StatusUpdatedAt: updated,
		Cwd:             r.Cwd,
		Version:         r.Version,
		WaitingFor:      r.WaitingFor,
	}, true
}
