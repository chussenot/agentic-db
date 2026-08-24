package fleet

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// Roster is the background supervisor's registry, ~/.claude/daemon/roster.json.
// It is not needed to know what is running (the first-party session files answer
// that) but it carries two things nothing else does: the supervisor's own pid, and
// how each worker was dispatched.
//
// The supervisor pid is worth having because it collapses owner resolution: the
// supervisor's environment names the session that caused the pool to exist, so one
// /proc read answers "whose fleet is this" for the whole fleet instead of one read
// per agent.
type Roster struct {
	SupervisorPID int
	UpdatedAt     time.Time
	// Workers is keyed by job id (the 8-hex short form).
	Workers map[string]Worker
}

// Worker is one entry of the roster.
//
// PID IS A TRAP: it is the pid of the worker's pty HOST process, not of the Claude
// session inside it (observed 2825277 vs 2825304 for the same job). Liveness must
// come from the session file's own pid; this field is for provenance only.
type Worker struct {
	PTYHostPID int
	SessionID  string
	Cwd        string
	StartedAt  time.Time
	// Attempt counts respawns of this job.
	Attempt int
	// Source is how the job was dispatched: "shell" (an external command),
	// "slash" (from inside a Claude session), or "spare". It is the closest thing
	// to provenance the roster carries — note it does NOT name a parent session.
	Source string
	// Agent is the agent type the worker runs as, when dispatched with one.
	Agent string
}

// rawRoster mirrors the on-disk JSON. The auth fields (ptyAuth, rvAuth, nonce) and
// the dispatch seed (which carries the job's prompt) are deliberately not declared:
// they are secrets and user prompts respectively, and what is not decoded cannot
// leak into a status database.
type rawRoster struct {
	SupervisorPID int   `json:"supervisorPid"`
	UpdatedAt     int64 `json:"updatedAt"` // unix milliseconds
	Workers       map[string]struct {
		PID       int    `json:"pid"`
		SessionID string `json:"sessionId"`
		Cwd       string `json:"cwd"`
		StartedAt int64  `json:"startedAt"`
		Attempt   int    `json:"attempt"`
		Dispatch  struct {
			Source string `json:"source"`
			Agent  string `json:"agent"`
		} `json:"dispatch"`
	} `json:"workers"`
}

// DefaultRosterPath returns $CLAUDE_CONFIG_DIR/daemon/roster.json when set, else
// ~/.claude/daemon/roster.json.
func DefaultRosterPath() string {
	if base := os.Getenv("CLAUDE_CONFIG_DIR"); base != "" {
		return filepath.Join(base, "daemon", "roster.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".claude", "daemon", "roster.json")
}

// ReadRoster parses the roster at path. ok is false when there is no readable
// roster — which is the normal state on a machine that has never run a background
// agent, and must degrade to "no roster" rather than an error.
func ReadRoster(path string) (Roster, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Roster{}, false
	}
	var raw rawRoster
	if err := json.Unmarshal(data, &raw); err != nil {
		return Roster{}, false
	}
	r := Roster{
		SupervisorPID: raw.SupervisorPID,
		Workers:       make(map[string]Worker, len(raw.Workers)),
	}
	if raw.UpdatedAt > 0 {
		r.UpdatedAt = time.UnixMilli(raw.UpdatedAt)
	}
	for short, w := range raw.Workers {
		worker := Worker{
			PTYHostPID: w.PID,
			SessionID:  w.SessionID,
			Cwd:        w.Cwd,
			Attempt:    w.Attempt,
			Source:     w.Dispatch.Source,
			Agent:      w.Dispatch.Agent,
		}
		if w.StartedAt > 0 {
			worker.StartedAt = time.UnixMilli(w.StartedAt)
		}
		r.Workers[short] = worker
	}
	return r, true
}
