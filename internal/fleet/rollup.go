package fleet

// Attribution and roll-up: joining the three sources (our DB rows, the first-party
// status files, the job records) into "whose fleet is this, and does it want
// something".
//
// This lives in package fleet rather than in the daemon so that the daemon and
// doctor share ONE implementation. The precedent to avoid is doctor.effectiveState,
// which hand-mirrors daemon.firstPartyState and has to be kept in step by hand.
//
// Everything here is a pure function of its inputs plus an explicit `now`: no I/O,
// no clock, no globals. The I/O happens in the readers above.

import (
	"sort"
	"time"

	"github.com/mrzor/claude-status/internal/clauded"
	"github.com/mrzor/claude-status/internal/db"
)

// OwnerSource records which rung of the attribution ladder answered, so a wrong
// attribution can be diagnosed instead of guessed at.
type OwnerSource string

const (
	// OwnerFromSpawn: the background process's own environment named a spawner
	// (CLAUDE_PID) that resolves to a live session. Strongest available signal.
	OwnerFromSpawn OwnerSource = "spawn"
	// OwnerFromCwd: no live spawner, but the job's parent checkout matches exactly
	// one live non-background session's cwd. This is the RE-HOMING rung, and it
	// matters because background agents outlive their spawner: without it, a fleet
	// whose launching session has exited becomes unattributable.
	OwnerFromCwd OwnerSource = "cwd"
	// OwnerNone: nothing resolved. Tracked, still reapable, just unattributed —
	// preferable to guessing, since two clones of one repo are indistinguishable
	// by repo identity alone.
	OwnerNone OwnerSource = "none"
)

// Demand is how loudly a background agent is asking for attention. The order is
// the precedence used when collapsing a fleet into one signal, worst first.
type Demand int

const (
	// DemandHuman: this agent is waiting for a person. It is deliberately the top
	// rank regardless of lifecycle — see Job.AwaitsHuman.
	DemandHuman Demand = iota
	// DemandWorking: making progress; interesting, not urgent.
	DemandWorking
	// DemandBlocked: stopped, but on something internal (another agent, a
	// monitor), not on a person.
	DemandBlocked
	// DemandIdle: alive and at rest.
	DemandIdle
	// DemandDone: finished. The process may still be around.
	DemandDone
)

// String renders a Demand for display.
func (d Demand) String() string {
	switch d {
	case DemandHuman:
		return "awaits-human"
	case DemandWorking:
		return "working"
	case DemandBlocked:
		return "blocked"
	case DemandIdle:
		return "idle"
	case DemandDone:
		return "done"
	}
	return "unknown"
}

// Child is one background agent, joined across all three sources.
type Child struct {
	SessionID string
	Name      string
	JobID     string
	// PID is the Claude process pid (from the first-party file), not a terminal.
	PID int
	// Job is the job record; the zero value when the agent has no readable
	// state.json (freshly dispatched, or its state file is gone).
	Job Job
	// HasJob distinguishes a zero Job from a job that genuinely reads as zero.
	HasJob bool
	// Demand is what this agent wants, and Needs is the verbatim ask when it
	// wants a person.
	Demand Demand
	Needs  string
	// Detail is the agent's own one-line summary of what it is doing.
	Detail string
	// Activity is the most recent evidence this agent did anything — see
	// childActivity for what counts and why the status file alone does not.
	Activity time.Time

	OwnerSessionID   string
	OwnerSource      OwnerSource
	ConsoleSessionID string
}

// Rollup is a fleet summarized for one attributed session.
type Rollup struct {
	// SessionID is the session this fleet is attributed to.
	SessionID string
	// Children are its background agents, worst-demand first.
	Children []Child
	// Live counts children that have not finished; Total counts all of them.
	Live  int
	Total int
	// AwaitingHuman, Working, Blocked, Idle, Done are the per-demand counts.
	AwaitingHuman int
	Working       int
	Blocked       int
	Idle          int
	Done          int
	// Worst is the highest-precedence demand across the fleet (DemandDone when
	// every child has finished).
	Worst Demand
	// Neediest is the child that set Worst, so a caller can name it.
	Neediest Child
	// NewestActivity is the most recent activity of any child.
	NewestActivity time.Time
}

// Attribute joins the sources into one Child per live background session.
//
// Inputs: rows as the DB has them, live as clauded.ReadLive returned it (so every
// entry is a session whose process is alive), jobs keyed by job id, and spawn as a
// resolver over /proc. rows is what makes the console edge visible, since that edge
// is persisted rather than derived.
//
// A background session with no live job record still yields a Child: it is running,
// it is ours to track, and its demand can still be read from its status file.
func Attribute(rows []db.Session, live map[string]clauded.Session, jobs map[string]Job, spawn *OwnerResolver, now time.Time) map[string]Child {
	byPID := make(map[int]clauded.Session, len(live))
	for _, s := range live {
		if s.PID > 0 {
			byPID[s.PID] = s
		}
	}
	rowByID := make(map[string]db.Session, len(rows))
	for _, r := range rows {
		rowByID[r.SessionID] = r
	}

	out := map[string]Child{}
	for id, s := range live {
		if !s.IsBackground() {
			continue
		}
		job, hasJob := jobs[s.JobID]
		child := Child{
			SessionID: id,
			Name:      s.Name,
			JobID:     s.JobID,
			PID:       s.PID,
			Job:       job,
			HasJob:    hasJob,
			Needs:     job.Needs,
			Detail:    job.Detail,
			Demand:    demandOf(s, job, hasJob),
			Activity:  childActivity(s, job, hasJob, rowByID[id]),
		}
		if row, ok := rowByID[id]; ok && row.ConsoleSessionID.Valid {
			child.ConsoleSessionID = row.ConsoleSessionID.String
		}
		child.OwnerSessionID, child.OwnerSource = resolveOwner(s, job, hasJob, live, byPID, spawn)
		out[id] = child
	}
	return out
}

// demandOf decides what a background agent wants. Needs-presence wins over
// everything: the live colony shows two agents whose state.json names a question
// for the human ("confirm: …", "A or B") while their first-party waitingFor is
// EMPTY — so keying on the status file alone silently loses those asks.
func demandOf(s clauded.Session, job Job, hasJob bool) Demand {
	if hasJob && job.AwaitsHuman() {
		return DemandHuman
	}
	if s.IsUserPrompt() {
		return DemandHuman
	}
	if hasJob {
		switch job.Lifecycle {
		case LifecycleDone:
			return DemandDone
		case LifecycleBlocked:
			return DemandBlocked
		case LifecycleWorking:
			// The job says it is working, but the process may be resting between
			// beats; trust the live status when it says so.
			if s.Status == clauded.Idle {
				return DemandIdle
			}
			return DemandWorking
		}
	}
	switch s.Status {
	case clauded.Busy, clauded.Shell:
		return DemandWorking
	case clauded.Idle:
		return DemandIdle
	}
	return DemandIdle
}

// childActivity is the most recent evidence this agent did anything.
//
// It takes the LATEST of several signals because each one individually goes stale:
// a job's state.json mtime advances on every beat (including a task joining or
// leaving its fan), the status file's UpdatedAt advances when the session changes
// state, and our own last_talk_ts advances on the agent's hook traffic. A working
// agent moves at least one of them.
func childActivity(s clauded.Session, job Job, hasJob bool, row db.Session) time.Time {
	var newest time.Time
	bump := func(t time.Time) {
		if t.After(newest) {
			newest = t
		}
	}
	bump(s.StatusUpdatedAt)
	bump(s.UpdatedAt)
	if hasJob {
		bump(job.FileModTime)
		bump(job.UpdatedAt)
	}
	if row.LastTalkTS.Valid {
		bump(time.UnixMilli(row.LastTalkTS.Int64))
	}
	if row.LastSeenTS > 0 {
		bump(time.UnixMilli(row.LastSeenTS))
	}
	return newest
}

// maxOwnerHops bounds the walk from a background agent up to a non-background
// owner. In practice the spare pool means the first hop already lands on the fleet
// root, so this is a guard against a cycle, not a real traversal depth.
const maxOwnerHops = 4

// resolveOwner runs the attribution ladder: spawn edge, then exact-cwd re-homing,
// then unowned.
func resolveOwner(s clauded.Session, job Job, hasJob bool, live map[string]clauded.Session, byPID map[int]clauded.Session, spawn *OwnerResolver) (string, OwnerSource) {
	if spawn != nil {
		cur := s
		for range maxOwnerHops {
			origin, ok := spawn.Spawn(cur.PID, cur.ProcStart)
			if !ok || origin.SpawnerPID == 0 {
				break
			}
			next, known := byPID[origin.SpawnerPID]
			if !known || next.SessionID == cur.SessionID {
				break
			}
			if !next.IsBackground() {
				return next.SessionID, OwnerFromSpawn
			}
			cur = next // a background agent spawned by another one; keep climbing
		}
	}
	// Re-homing. The job record's Cwd is the PARENT CHECKOUT, while the session's
	// own cwd is its worktree — so this matches the launching repository, not the
	// chamber. Exact path only, and only when it is unambiguous: two clones of one
	// remote are indistinguishable by repo identity, so a near-miss is left
	// unowned rather than guessed.
	root := job.Cwd
	if !hasJob || root == "" {
		root = job.WorktreePath // nothing better available
	}
	if root != "" {
		var match string
		n := 0
		for id, cand := range live {
			if cand.IsBackground() || cand.Cwd != root {
				continue
			}
			match, n = id, n+1
		}
		if n == 1 {
			return match, OwnerFromCwd
		}
	}
	return "", OwnerNone
}

// Rollups groups children by attributed session. Each child is counted under BOTH
// its owner (who started the fleet) and its console (who was last told it needs
// input), because both sessions are legitimately "not idle" while it works — and on
// a real machine they are different sessions on different desktops.
func Rollups(children map[string]Child) map[string]Rollup {
	byOwner := map[string][]Child{}
	for _, c := range children {
		if c.OwnerSessionID != "" {
			byOwner[c.OwnerSessionID] = append(byOwner[c.OwnerSessionID], c)
		}
		if c.ConsoleSessionID != "" && c.ConsoleSessionID != c.OwnerSessionID {
			byOwner[c.ConsoleSessionID] = append(byOwner[c.ConsoleSessionID], c)
		}
	}
	out := make(map[string]Rollup, len(byOwner))
	for owner, kids := range byOwner {
		out[owner] = summarize(owner, kids)
	}
	return out
}

// summarize collapses one session's children into a Rollup.
func summarize(sessionID string, kids []Child) Rollup {
	// Worst demand first, then newest activity, then session id — a total order,
	// so the same fleet always summarizes identically (the tiles learned this
	// lesson the hard way: a non-deterministic pick blinks).
	sort.Slice(kids, func(i, j int) bool {
		if kids[i].Demand != kids[j].Demand {
			return kids[i].Demand < kids[j].Demand
		}
		if !kids[i].Activity.Equal(kids[j].Activity) {
			return kids[i].Activity.After(kids[j].Activity)
		}
		return kids[i].SessionID < kids[j].SessionID
	})

	r := Rollup{SessionID: sessionID, Children: kids, Total: len(kids), Worst: DemandDone}
	for _, c := range kids {
		switch c.Demand {
		case DemandHuman:
			r.AwaitingHuman++
		case DemandWorking:
			r.Working++
		case DemandBlocked:
			r.Blocked++
		case DemandIdle:
			r.Idle++
		case DemandDone:
			r.Done++
		}
		if c.Demand != DemandDone {
			r.Live++
		}
		if c.Activity.After(r.NewestActivity) {
			r.NewestActivity = c.Activity
		}
	}
	if len(kids) > 0 {
		r.Worst = kids[0].Demand
		r.Neediest = kids[0]
	}
	return r
}

// EffectiveActivity is when this session last showed ANY sign of life, its fleet
// included.
//
// This is the number the "19h idle" bug is about. A session's own last_talk_ts is
// honest but incomplete: while its background agents work, the session itself says
// nothing and its first-party status file stops being written (its main loop is
// suspended on its children), so it decays on the bar as though abandoned — even
// with six agents mid-flight, three of them waiting on a human.
//
// Callers should treat this as the freshness input, and keep last_talk_ts for "when
// did the HUMAN last see output", which is a different question.
func EffectiveActivity(row db.Session, fp clauded.Session, rollup Rollup) time.Time {
	var newest time.Time
	bump := func(t time.Time) {
		if t.After(newest) {
			newest = t
		}
	}
	if row.LastTalkTS.Valid {
		bump(time.UnixMilli(row.LastTalkTS.Int64))
	}
	bump(fp.StatusUpdatedAt)
	bump(fp.UpdatedAt)
	// Only a live child counts: a finished agent's last beat is history, and
	// letting it count would pin a session bright forever.
	for _, c := range rollup.Children {
		if c.Demand != DemandDone {
			bump(c.Activity)
		}
	}
	return newest
}
