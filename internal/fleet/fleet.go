// Package fleet reads Claude Code's first-party record of BACKGROUND AGENTS —
// sessions started with `claude --bg` (or a /-command, or a colony dispatch) that
// run in their own process, under their own session id, with their own hooks.
//
// Why this package exists: a background agent has no terminal. It therefore never
// resolves to a niri window, which is the axis the rest of this program is built
// on — so it is invisible to the workspace glyphs and the tiles, and (until the
// liveness work) immortal in the database. Worse, an ordinary session that spawns
// a fleet goes QUIET while its children work: its own ~/.claude/sessions/<pid>.json
// stops being updated because its main loop is suspended, so it reads as idle and
// decays on the bar while six of its agents sit waiting for a human. Everything
// here exists to make that state legible.
//
// Two things are called "background" in Claude Code, and only the first is this
// package's business:
//
//   - a background SESSION (this package): own process, own session id, own hooks,
//     a record under ~/.claude/jobs/<jobId>/, and an entry in
//     ~/.claude/daemon/roster.json.
//   - an in-process TASK (the Agent, Monitor and backgrounded Bash tools): no
//     process and no session id of its own. It appears here only as a job's
//     InFlight/Fan, and for an interactive parent it is not recorded in any
//     first-party file at all.
//
// The formats read here are UNDOCUMENTED (observed on Claude Code 2.1.231) and are
// treated exactly as package clauded treats the status files: every read is
// tolerant, a malformed or missing file yields "no data" rather than an error, and
// a format change degrades to the pre-fleet behaviour instead of breaking.
//
// Deliberately NOT read: a job's `intent`, its `respawnFlags` prompt text, and
// anything in roster.json's auth fields (ptyAuth / rvAuth / nonce). The first two
// are raw user prompts and the third are secrets; none of them belong in a status
// database. `detail` and `needs` ARE read — they are short human-written status
// lines, which is precisely what makes them worth surfacing.
package fleet

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// Lifecycle is a background job's coarse state, as the job itself reports it.
// Unrecognized values are preserved verbatim rather than coerced.
type Lifecycle string

const (
	// LifecycleWorking: the agent is making progress.
	LifecycleWorking Lifecycle = "working"
	// LifecycleBlocked: the agent has stopped and cannot continue on its own.
	// Whether that means "needs the human" is answered by Job.Needs, NOT by this
	// value — an agent can be blocked on another agent.
	LifecycleBlocked Lifecycle = "blocked"
	// LifecycleDone: finished. The job record can outlive the process.
	LifecycleDone Lifecycle = "done"
)

// Tempo is the job's self-reported pace. Observed values: active, idle, blocked.
type Tempo string

// InFlight counts the in-process tasks a background job is currently running.
// This is the only place Claude Code publishes such a count, and it exists for
// background sessions only — an interactive session running six agents publishes
// nothing equivalent.
type InFlight struct {
	Tasks  int
	Queued int
	// Kinds names the task types in flight. Note the vocabulary differs from
	// FanEntry.Kind for the same thing: "local_agent" here, "agent" there.
	Kinds []string
}

// FanEntry is one in-flight task inside a background job. IDs come from two
// disjoint namespaces, which is itself a classifier: an agent id is "a" followed
// by 16 hex digits; a monitor or backgrounded shell is "b" followed by 8 base36
// characters.
type FanEntry struct {
	ID        string
	Kind      string // "agent" | "monitor"
	Label     string
	StartedAt time.Time
}

// Job is the tolerant decode of one ~/.claude/jobs/<JobID>/state.json.
type Job struct {
	// JobID is the directory name, and equals the session id's first 8 hex
	// characters. It is the join key between a clauded.Session and this record.
	JobID     string
	SessionID string
	Name      string
	// Agent is the agent type the job runs as ("colony-integrator"), empty when
	// it was spawned without one. Template is the job template ("bg", or the
	// agent name for a templated dispatch).
	Agent    string
	Template string
	// Cwd is the job's PARENT CHECKOUT, which is not the same thing as the
	// session's own cwd: for a job running in a git worktree, the session reports
	// the worktree while this field reports the repository it was cut from. That
	// makes it the field to use when attributing a job to a repo.
	Cwd            string
	WorktreePath   string
	WorktreeBranch string
	// Detail is the agent's own one-line summary of what it is doing.
	Detail string
	// Needs is what the job is waiting for from a human, verbatim, e.g.
	// "approve Bash: ls docs/..." or "answer: <question> (Opt A · Opt B)". EMPTY
	// IS THE SIGNAL: a job with a non-empty Needs wants the human, whatever its
	// Lifecycle says, and a blocked job with no Needs is stuck on something
	// internal.
	Needs     string
	Lifecycle Lifecycle
	Tempo     Tempo
	InFlight  InFlight
	Fan       []FanEntry
	Tokens    int
	CreatedAt time.Time
	UpdatedAt time.Time
	// FileModTime is the mtime of state.json. It advances on every state change,
	// including a task joining or leaving Fan, which makes it a far better
	// liveness signal for a working job than the parent's frozen status file.
	// (UpdatedAt is the job's own ISO-8601 UTC stamp; do not compare the two
	// naively — one is local-clock file metadata, the other is UTC content.)
	FileModTime time.Time
}

// AwaitsHuman reports whether this job is waiting on a person. It keys on Needs
// rather than on Lifecycle, because "blocked" covers both "blocked on you" and
// "blocked on another agent", and only the first should ever pull a human's eye.
func (j Job) AwaitsHuman() bool { return j.Needs != "" }

// rawJob mirrors the on-disk JSON. Unknown fields are ignored, so new Claude Code
// fields never break the read. The prompt-bearing fields (intent, respawnFlags)
// are intentionally absent from this struct: what is not decoded cannot leak.
type rawJob struct {
	SessionID      string `json:"sessionId"`
	Name           string `json:"name"`
	Agent          string `json:"agent"`
	Template       string `json:"template"`
	Cwd            string `json:"cwd"`
	WorktreePath   string `json:"worktreePath"`
	WorktreeBranch string `json:"worktreeBranch"`
	Detail         string `json:"detail"`
	Needs          string `json:"needs"`
	State          string `json:"state"`
	Tempo          string `json:"tempo"`
	Tokens         int    `json:"tokens"`
	CreatedAt      string `json:"createdAt"` // ISO-8601 UTC
	UpdatedAt      string `json:"updatedAt"` // ISO-8601 UTC
	InFlight       struct {
		Tasks  int      `json:"tasks"`
		Queued int      `json:"queued"`
		Kinds  []string `json:"kinds"`
	} `json:"inFlight"`
	Fan []struct {
		ID        string `json:"id"`
		Kind      string `json:"kind"`
		Label     string `json:"label"`
		StartedAt int64  `json:"startedAt"` // unix milliseconds
	} `json:"fan"`
}

// DefaultJobsDir returns Claude Code's background-jobs directory:
// $CLAUDE_CONFIG_DIR/jobs when set, else ~/.claude/jobs. Mirrors clauded.DefaultDir.
func DefaultJobsDir() string {
	if base := os.Getenv("CLAUDE_CONFIG_DIR"); base != "" {
		return filepath.Join(base, "jobs")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".claude", "jobs")
}

// Reader reads job records, re-parsing a state.json only when its mtime or size
// changed. The daemon calls this on its 1s tick, and a colony's jobs rewrite their
// state every few seconds at most, so the cache turns a per-tick parse of every
// job into a per-tick stat of every job.
type Reader struct {
	dir   string
	cache map[string]cachedJob
}

type cachedJob struct {
	mod  time.Time
	size int64
	job  Job
}

// NewReader returns a Reader over dir (see DefaultJobsDir).
func NewReader(dir string) *Reader {
	return &Reader{dir: dir, cache: map[string]cachedJob{}}
}

// Dir reports the directory being read.
func (r *Reader) Dir() string { return r.dir }

// ReadJobs returns the job records for jobIDs, keyed by job id, skipping any that
// cannot be read.
//
// The caller passes the job ids of LIVE sessions rather than letting this walk the
// directory, and that is deliberate: job directories are not reliably cleaned up
// (dead jobs from previous months are still on disk here, some with a state.json
// and some without), so directory contents say nothing about what is running.
// Liveness comes from the first-party session files; this function only enriches
// what those already prove alive. Entries whose job has been reaped from the cache
// are dropped so a long-lived Reader cannot grow without bound.
func (r *Reader) ReadJobs(jobIDs []string) map[string]Job {
	out := make(map[string]Job, len(jobIDs))
	seen := make(map[string]bool, len(jobIDs))
	for _, id := range jobIDs {
		if id == "" {
			continue
		}
		seen[id] = true
		job, ok := r.read(id)
		if !ok {
			continue
		}
		out[id] = job
	}
	for id := range r.cache {
		if !seen[id] {
			delete(r.cache, id)
		}
	}
	return out
}

// read returns one job, from cache when the file has not changed. ok is false when
// the job has no readable state.json — which is normal, not exceptional: a job
// directory can exist with only a timeline, and a job that has just been dispatched
// may not have written its state yet.
func (r *Reader) read(jobID string) (Job, bool) {
	path := filepath.Join(r.dir, jobID, "state.json")
	fi, err := os.Stat(path)
	if err != nil {
		delete(r.cache, jobID)
		return Job{}, false
	}
	if c, ok := r.cache[jobID]; ok && c.mod.Equal(fi.ModTime()) && c.size == fi.Size() {
		return c.job, true
	}
	job, ok := parseJob(path, jobID, fi.ModTime())
	if !ok {
		delete(r.cache, jobID)
		return Job{}, false
	}
	r.cache[jobID] = cachedJob{mod: fi.ModTime(), size: fi.Size(), job: job}
	return job, true
}

// parseJob decodes one state.json. A malformed file is skipped rather than
// reported: the daemon must keep working through a format change.
func parseJob(path, jobID string, mod time.Time) (Job, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Job{}, false
	}
	var raw rawJob
	if err := json.Unmarshal(data, &raw); err != nil {
		return Job{}, false
	}
	job := Job{
		JobID:          jobID,
		SessionID:      raw.SessionID,
		Name:           raw.Name,
		Agent:          raw.Agent,
		Template:       raw.Template,
		Cwd:            raw.Cwd,
		WorktreePath:   raw.WorktreePath,
		WorktreeBranch: raw.WorktreeBranch,
		Detail:         raw.Detail,
		Needs:          raw.Needs,
		Lifecycle:      Lifecycle(raw.State),
		Tempo:          Tempo(raw.Tempo),
		Tokens:         raw.Tokens,
		CreatedAt:      parseISO(raw.CreatedAt),
		UpdatedAt:      parseISO(raw.UpdatedAt),
		FileModTime:    mod,
		InFlight: InFlight{
			Tasks:  raw.InFlight.Tasks,
			Queued: raw.InFlight.Queued,
			Kinds:  raw.InFlight.Kinds,
		},
	}
	for _, f := range raw.Fan {
		entry := FanEntry{ID: f.ID, Kind: f.Kind, Label: f.Label}
		if f.StartedAt > 0 {
			entry.StartedAt = time.UnixMilli(f.StartedAt)
		}
		job.Fan = append(job.Fan, entry)
	}
	return job, true
}

// parseISO decodes an ISO-8601/RFC-3339 stamp, yielding the zero time on anything
// unparseable (the field is informational; a bad value must not drop the job).
func parseISO(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
