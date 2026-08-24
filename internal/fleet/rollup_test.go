package fleet

import (
	"database/sql"
	"testing"
	"time"
)

import "github.com/mrzor/claude-status/internal/clauded"
import "github.com/mrzor/claude-status/internal/db"

var base = time.UnixMilli(1786714000000)

func bgSession(id, name, jobID string, pid int, status clauded.Status, waitingFor string) clauded.Session {
	return clauded.Session{
		SessionID: id, Name: name, JobID: jobID, PID: pid, ProcStart: "1",
		Kind: clauded.KindBackground, Status: status, WaitingFor: waitingFor,
		StatusUpdatedAt: base, UpdatedAt: base,
	}
}

func interactiveSession(id, name string, pid int, cwd string) clauded.Session {
	return clauded.Session{
		SessionID: id, Name: name, PID: pid, ProcStart: "1", Cwd: cwd,
		Kind: clauded.KindInteractive, Status: clauded.Idle, StatusUpdatedAt: base, UpdatedAt: base,
	}
}

func TestDemandNeedsBeatsLifecycle(t *testing.T) {
	cases := []struct {
		name   string
		fp     clauded.Session
		job    Job
		hasJob bool
		want   Demand
	}{
		{
			// The live case that motivated needs-presence: the job names a question
			// for the human while the status file's waitingFor is EMPTY.
			name:   "blocked job with needs and no waitingFor",
			fp:     bgSession("s", "keeper", "j", 1, clauded.Shell, ""),
			job:    Job{Lifecycle: LifecycleBlocked, Needs: "A (leave as landed) or B (revert)"},
			hasJob: true,
			want:   DemandHuman,
		},
		{
			name:   "working job that still needs approval",
			fp:     bgSession("s", "pm-3", "j", 1, clauded.Waiting, "permission prompt"),
			job:    Job{Lifecycle: LifecycleWorking, Needs: "approve Bash: rm ..."},
			hasJob: true,
			want:   DemandHuman,
		},
		{
			name:   "blocked on another agent, not on a person",
			fp:     bgSession("s", "dispatcher", "j", 1, clauded.Shell, ""),
			job:    Job{Lifecycle: LifecycleBlocked},
			hasJob: true,
			want:   DemandBlocked,
		},
		{
			name:   "working",
			fp:     bgSession("s", "pm-5", "j", 1, clauded.Busy, ""),
			job:    Job{Lifecycle: LifecycleWorking},
			hasJob: true,
			want:   DemandWorking,
		},
		{
			name:   "job says working but the process is resting",
			fp:     bgSession("s", "pm-5", "j", 1, clauded.Idle, ""),
			job:    Job{Lifecycle: LifecycleWorking},
			hasJob: true,
			want:   DemandIdle,
		},
		{
			name:   "done",
			fp:     bgSession("s", "helpdesk", "j", 1, clauded.Idle, ""),
			job:    Job{Lifecycle: LifecycleDone},
			hasJob: true,
			want:   DemandDone,
		},
		{
			// No job record yet (freshly dispatched): the status file still speaks.
			name: "no job record, busy",
			fp:   bgSession("s", "fresh", "j", 1, clauded.Busy, ""),
			want: DemandWorking,
		},
		{
			name: "no job record, waiting on permission",
			fp:   bgSession("s", "fresh", "j", 1, clauded.Waiting, "permission prompt"),
			want: DemandHuman,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := demandOf(c.fp, c.job, c.hasJob); got != c.want {
				t.Errorf("demandOf = %v, want %v", got, c.want)
			}
		})
	}
}

func TestAttributeUsesSpawnEdge(t *testing.T) {
	// pid 200 (the agent) was spawned by pid 100 (an interactive session).
	stubEnviron(t, map[int][]string{
		200: {"CLAUDE_CODE_SESSION_KIND=bg", "CLAUDE_PID=100"},
	})
	live := map[string]clauded.Session{
		"owner": interactiveSession("owner", "project-manager-f4", 100, "/repo"),
		"bg1":   bgSession("bg1", "project-manager-3", "job1", 200, clauded.Busy, ""),
	}
	jobs := map[string]Job{"job1": {JobID: "job1", Lifecycle: LifecycleWorking, Cwd: "/repo"}}

	got := Attribute(nil, live, jobs, NewOwnerResolver(), base)
	if len(got) != 1 {
		t.Fatalf("attributed %d children, want 1 (interactive sessions are not children)", len(got))
	}
	c := got["bg1"]
	if c.OwnerSessionID != "owner" || c.OwnerSource != OwnerFromSpawn {
		t.Errorf("owner = %q via %q, want owner via spawn", c.OwnerSessionID, c.OwnerSource)
	}
}

// A background agent spawned by another background agent must be attributed to the
// non-background root, not to its immediate parent.
func TestAttributeClimbsToNonBackgroundRoot(t *testing.T) {
	stubEnviron(t, map[int][]string{
		300: {"CLAUDE_CODE_SESSION_KIND=bg", "CLAUDE_PID=200"}, // worker <- dispatcher
		200: {"CLAUDE_CODE_SESSION_KIND=bg", "CLAUDE_PID=100"}, // dispatcher <- root
	})
	live := map[string]clauded.Session{
		"root":       interactiveSession("root", "pm-f4", 100, "/repo"),
		"dispatcher": bgSession("dispatcher", "pm-dispatcher", "j2", 200, clauded.Shell, ""),
		"worker":     bgSession("worker", "pm-3", "j3", 300, clauded.Busy, ""),
	}
	got := Attribute(nil, live, map[string]Job{}, NewOwnerResolver(), base)
	if got["worker"].OwnerSessionID != "root" {
		t.Errorf("worker owner = %q, want root", got["worker"].OwnerSessionID)
	}
	if got["dispatcher"].OwnerSessionID != "root" {
		t.Errorf("dispatcher owner = %q, want root", got["dispatcher"].OwnerSessionID)
	}
}

// Background agents outlive their spawner, so a fleet whose launching session has
// exited must re-home rather than become unattributable.
func TestAttributeReHomesOnJobCwdWhenSpawnerIsGone(t *testing.T) {
	stubEnviron(t, map[int][]string{
		200: {"CLAUDE_CODE_SESSION_KIND=bg", "CLAUDE_PID=999"}, // 999 is gone
	})
	live := map[string]clauded.Session{
		// The job's Cwd is the parent checkout; this session sits in it.
		"heir": interactiveSession("heir", "pm-f4", 101, "/repo"),
		"bg1":  bgSession("bg1", "pm-3", "job1", 200, clauded.Busy, ""),
	}
	jobs := map[string]Job{"job1": {
		JobID: "job1", Lifecycle: LifecycleWorking,
		Cwd:          "/repo",
		WorktreePath: "/repo/.claude/worktrees/pm-3",
	}}
	c := Attribute(nil, live, jobs, NewOwnerResolver(), base)["bg1"]
	if c.OwnerSessionID != "heir" || c.OwnerSource != OwnerFromCwd {
		t.Errorf("owner = %q via %q, want heir via cwd", c.OwnerSessionID, c.OwnerSource)
	}
}

// Two clones of one repository are indistinguishable, so an ambiguous re-homing is
// left unowned rather than guessed.
func TestAttributeLeavesAmbiguousReHomingUnowned(t *testing.T) {
	stubEnviron(t, map[int][]string{200: {"CLAUDE_CODE_SESSION_KIND=bg", "CLAUDE_PID=999"}})
	live := map[string]clauded.Session{
		"a":   interactiveSession("a", "clone-a", 101, "/repo"),
		"b":   interactiveSession("b", "clone-b", 102, "/repo"),
		"bg1": bgSession("bg1", "pm-3", "job1", 200, clauded.Busy, ""),
	}
	jobs := map[string]Job{"job1": {JobID: "job1", Cwd: "/repo"}}
	c := Attribute(nil, live, jobs, NewOwnerResolver(), base)["bg1"]
	if c.OwnerSessionID != "" || c.OwnerSource != OwnerNone {
		t.Errorf("owner = %q via %q, want unowned", c.OwnerSessionID, c.OwnerSource)
	}
}

func TestAttributeCarriesConsoleEdgeFromRows(t *testing.T) {
	stubEnviron(t, map[int][]string{200: {"CLAUDE_CODE_SESSION_KIND=bg", "CLAUDE_PID=100"}})
	live := map[string]clauded.Session{
		"owner": interactiveSession("owner", "pm-f4", 100, "/repo"),
		"bg1":   bgSession("bg1", "pm-3", "job1", 200, clauded.Waiting, "permission prompt"),
	}
	rows := []db.Session{{
		SessionID:        "bg1",
		ConsoleSessionID: sql.NullString{String: "watcher", Valid: true},
	}}
	c := Attribute(rows, live, map[string]Job{}, NewOwnerResolver(), base)["bg1"]
	if c.ConsoleSessionID != "watcher" {
		t.Errorf("ConsoleSessionID = %q, want watcher", c.ConsoleSessionID)
	}
	if c.OwnerSessionID != "owner" {
		t.Errorf("owner = %q, want owner (the console edge does not replace it)", c.OwnerSessionID)
	}
}

func TestRollupsCountAndRankFleet(t *testing.T) {
	mk := func(id string, d Demand, needs string, ageSec int) Child {
		return Child{
			SessionID: id, Name: id, OwnerSessionID: "owner", Demand: d, Needs: needs,
			Activity: base.Add(-time.Duration(ageSec) * time.Second),
		}
	}
	children := map[string]Child{
		"w1": mk("w1", DemandWorking, "", 30),
		"h1": mk("h1", DemandHuman, "approve Bash: rm ...", 90),
		"b1": mk("b1", DemandBlocked, "", 10),
		"d1": mk("d1", DemandDone, "", 5),
		"i1": mk("i1", DemandIdle, "", 60),
	}
	got := Rollups(children)
	r, ok := got["owner"]
	if !ok {
		t.Fatalf("no rollup for owner: %v", got)
	}
	if r.Total != 5 || r.Live != 4 {
		t.Errorf("Total=%d Live=%d, want 5/4 (done is not live)", r.Total, r.Live)
	}
	if r.AwaitingHuman != 1 || r.Working != 1 || r.Blocked != 1 || r.Idle != 1 || r.Done != 1 {
		t.Errorf("counts wrong: %+v", r)
	}
	if r.Worst != DemandHuman {
		t.Errorf("Worst = %v, want awaits-human", r.Worst)
	}
	if r.Neediest.SessionID != "h1" || r.Neediest.Needs != "approve Bash: rm ..." {
		t.Errorf("Neediest = %+v, want h1 with its ask", r.Neediest)
	}
	// Newest activity is the freshest child's, regardless of demand.
	if !r.NewestActivity.Equal(base.Add(-5 * time.Second)) {
		t.Errorf("NewestActivity = %v", r.NewestActivity)
	}
	if r.Children[0].SessionID != "h1" {
		t.Errorf("children not sorted worst-first: %+v", r.Children[0])
	}
}

// The two edges are tracked separately, so a child counts for the session that
// spawned it AND for the session watching it.
func TestRollupsCountChildUnderOwnerAndConsole(t *testing.T) {
	children := map[string]Child{
		"bg1": {SessionID: "bg1", OwnerSessionID: "spawner", ConsoleSessionID: "watcher", Demand: DemandHuman},
	}
	got := Rollups(children)
	if len(got) != 2 {
		t.Fatalf("got %d rollups, want 2 (owner and console)", len(got))
	}
	for _, id := range []string{"spawner", "watcher"} {
		if got[id].AwaitingHuman != 1 || got[id].Worst != DemandHuman {
			t.Errorf("rollup[%s] = %+v, want one awaiting child", id, got[id])
		}
	}
	// And no double counting when the two edges are the same session.
	same := Rollups(map[string]Child{
		"bg1": {SessionID: "bg1", OwnerSessionID: "s", ConsoleSessionID: "s", Demand: DemandWorking},
	})
	if len(same) != 1 || same["s"].Total != 1 {
		t.Errorf("same-session edges double counted: %+v", same)
	}
}

func TestRollupsSkipUnattributedChildren(t *testing.T) {
	got := Rollups(map[string]Child{
		"orphan": {SessionID: "orphan", Demand: DemandWorking},
	})
	if len(got) != 0 {
		t.Errorf("an unowned child must not produce a rollup: %v", got)
	}
}

// The point of the whole exercise: a session that has said nothing for 19 hours but
// whose fleet is mid-flight is NOT stale.
func TestEffectiveActivityCountsLiveChildren(t *testing.T) {
	nineteenHoursAgo := base.Add(-19 * time.Hour)
	row := db.Session{
		SessionID:  "owner",
		LastTalkTS: sql.NullInt64{Int64: nineteenHoursAgo.UnixMilli(), Valid: true},
		LastSeenTS: nineteenHoursAgo.UnixMilli(),
	}
	// The owner's own status file is frozen too — its main loop is suspended on
	// its children, which is exactly why it looks abandoned.
	fp := clauded.Session{SessionID: "owner", StatusUpdatedAt: nineteenHoursAgo, UpdatedAt: nineteenHoursAgo}
	rollup := Rollup{Children: []Child{
		{SessionID: "bg1", Demand: DemandHuman, Activity: base.Add(-30 * time.Second)},
		{SessionID: "bg2", Demand: DemandWorking, Activity: base.Add(-2 * time.Minute)},
	}}

	got := EffectiveActivity(row, fp, rollup)
	if !got.Equal(base.Add(-30 * time.Second)) {
		t.Errorf("EffectiveActivity = %v, want the freshest child's activity", got)
	}
	if base.Sub(got) > time.Minute {
		t.Errorf("effective idle age = %v, want seconds not hours", base.Sub(got))
	}
}

// A finished agent's last beat is history and must not pin its owner bright.
func TestEffectiveActivityIgnoresFinishedChildren(t *testing.T) {
	old := base.Add(-19 * time.Hour)
	row := db.Session{LastTalkTS: sql.NullInt64{Int64: old.UnixMilli(), Valid: true}}
	rollup := Rollup{Children: []Child{
		{SessionID: "done1", Demand: DemandDone, Activity: base},
	}}
	if got := EffectiveActivity(row, clauded.Session{}, rollup); !got.Equal(old) {
		t.Errorf("EffectiveActivity = %v, want the session's own %v", got, old)
	}
}

func TestEffectiveActivityWithNoFleetIsTheSessionsOwn(t *testing.T) {
	own := base.Add(-5 * time.Minute)
	row := db.Session{LastTalkTS: sql.NullInt64{Int64: own.UnixMilli(), Valid: true}}
	if got := EffectiveActivity(row, clauded.Session{}, Rollup{}); !got.Equal(own) {
		t.Errorf("EffectiveActivity = %v, want %v", got, own)
	}
}
