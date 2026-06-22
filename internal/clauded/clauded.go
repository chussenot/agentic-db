// Package clauded reads Claude Code's first-party per-session status files,
// ~/.claude/sessions/<pid>.json. Each file is written by the Claude Code process
// itself and carries a `status` field — busy / idle / waiting — that updates on
// Claude's own cadence rather than via our hooks. This is a stronger, language-
// independent signal for the "is Claude blocked waiting on the user?" question
// than substring-matching Notification text: `waiting` means Claude is blocked
// on the user (a permission prompt or interactive question), `busy` means it is
// taking a turn, and `idle` means a finished turn at rest (observed sitting at
// `idle` for many minutes, so `idle` — not `waiting` — is the steady done state).
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
	// Waiting: Claude is blocked waiting on the user (permission/interactive
	// prompt) — the genuine "needs you" signal.
	Waiting Status = "waiting"
)

// Known reports whether s is one of the recognized status values.
func (s Status) Known() bool {
	switch s {
	case Busy, Idle, Waiting:
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
	}, true
}
