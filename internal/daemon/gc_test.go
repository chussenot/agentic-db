package daemon

import (
	"database/sql"
	"testing"

	"github.com/mrzor/claude-status/internal/clauded"
	"github.com/mrzor/claude-status/internal/db"
)

// fpSet builds a first-party set keyed by session id (status is irrelevant to
// presence, which is all firstPartyDead cares about).
func fpSet(ids ...string) map[string]clauded.Session {
	m := make(map[string]clauded.Session, len(ids))
	for _, id := range ids {
		m[id] = clauded.Session{SessionID: id, Status: clauded.Idle}
	}
	return m
}

// localSess is a session with a resolved window (the trackable, local case).
func localSess(id string, windowID int) db.Session {
	s := gcSess(windowID, -1)
	s.SessionID = id
	return s
}

func newDaemonForTest() *daemon { return &daemon{fpMiss: make(map[string]int)} }

func TestFirstPartyDead(t *testing.T) {
	t.Run("reaps a local session after the miss threshold", func(t *testing.T) {
		d := newDaemonForTest()
		sessions := []db.Session{localSess("gone", 100)}
		fp := fpSet("other") // available, but our session is absent
		for i := 1; i < firstPartyMissThreshold; i++ {
			if dead := d.firstPartyDead(sessions, fp, true); dead["gone"] {
				t.Fatalf("reaped after %d miss(es); threshold is %d", i, firstPartyMissThreshold)
			}
		}
		dead := d.firstPartyDead(sessions, fp, true)
		if !dead["gone"] {
			t.Fatalf("expected reap at miss %d", firstPartyMissThreshold)
		}
	})

	t.Run("a reappearing session resets its counter", func(t *testing.T) {
		d := newDaemonForTest()
		sessions := []db.Session{localSess("flap", 100)}
		// Accumulate misses just short of the threshold...
		for i := 1; i < firstPartyMissThreshold; i++ {
			d.firstPartyDead(sessions, fpSet("other"), true)
		}
		// ...then reappear: counter must reset.
		d.firstPartyDead(sessions, fpSet("flap"), true)
		if d.fpMiss["flap"] != 0 {
			t.Fatalf("counter = %d after reappearance, want 0", d.fpMiss["flap"])
		}
		// One miss after the reset must not immediately reap.
		if dead := d.firstPartyDead(sessions, fpSet("other"), true); dead["flap"] {
			t.Fatal("reaped one tick after a reset")
		}
	})

	t.Run("present session is never reaped", func(t *testing.T) {
		d := newDaemonForTest()
		sessions := []db.Session{localSess("here", 100)}
		for i := 0; i < firstPartyMissThreshold+3; i++ {
			if dead := d.firstPartyDead(sessions, fpSet("here"), true); dead["here"] {
				t.Fatal("reaped a session that is present in the first-party set")
			}
		}
	})

	t.Run("unavailable first-party reaps nothing and clears counters", func(t *testing.T) {
		d := newDaemonForTest()
		sessions := []db.Session{localSess("gone", 100)}
		for i := 1; i < firstPartyMissThreshold; i++ {
			d.firstPartyDead(sessions, fpSet("other"), true)
		}
		if dead := d.firstPartyDead(sessions, nil, false); len(dead) != 0 {
			t.Fatalf("reaped %d while first-party unavailable, want 0", len(dead))
		}
		if len(d.fpMiss) != 0 {
			t.Fatalf("counters not cleared: %v", d.fpMiss)
		}
	})

	t.Run("non-local session is never tracked", func(t *testing.T) {
		d := newDaemonForTest()
		remote := db.Session{SessionID: "remote"} // NULL window_id
		for i := 0; i < firstPartyMissThreshold+3; i++ {
			if dead := d.firstPartyDead([]db.Session{remote}, fpSet("other"), true); dead["remote"] {
				t.Fatal("reaped a non-local (NULL window) session")
			}
		}
		if _, tracked := d.fpMiss["remote"]; tracked {
			t.Fatal("non-local session should never enter the miss map")
		}
	})

	t.Run("counters are pruned when a session leaves the snapshot", func(t *testing.T) {
		d := newDaemonForTest()
		d.firstPartyDead([]db.Session{localSess("gone", 100)}, fpSet("other"), true)
		if d.fpMiss["gone"] == 0 {
			t.Fatal("precondition: expected a miss recorded")
		}
		// Next tick the row is no longer in the snapshot (reaped/deleted).
		d.firstPartyDead(nil, fpSet("other"), true)
		if _, tracked := d.fpMiss["gone"]; tracked {
			t.Fatal("counter for a departed session should be pruned")
		}
	})
}

func TestDeadPredicateFirstParty(t *testing.T) {
	orig := procExists
	procExists = func(int) bool { return true } // all pids alive
	defer func() { procExists = orig }()

	model := presenceSet{100: true}
	pred := deadPredicate(model, map[string]bool{"zombie": true})

	alive := localSess("alive", 100)
	if pred(alive) {
		t.Fatal("a live session not in fpDead must not be reaped")
	}
	zombie := localSess("zombie", 100) // terminal alive, but first-party says dead
	if !pred(zombie) {
		t.Fatal("a session in fpDead must be reaped even with a live window")
	}
}

// presenceSet is a test windowPresence: the set of window ids the model knows.
type presenceSet map[int]bool

func (p presenceSet) HasWindow(id int) bool { return p[id] }

func gcSess(windowID, termPID int) db.Session {
	var s db.Session
	if windowID >= 0 {
		s.WindowID = sql.NullInt64{Int64: int64(windowID), Valid: true}
	}
	if termPID >= 0 {
		s.TerminalPID = sql.NullInt64{Int64: int64(termPID), Valid: true}
	}
	return s
}

func TestDeadPredicate(t *testing.T) {
	// Stub /proc existence: pids in this set are "alive".
	alive := map[int]bool{42: true, 43: true}
	orig := procExists
	procExists = func(pid int) bool { return alive[pid] }
	defer func() { procExists = orig }()

	model := presenceSet{100: true} // only window 100 is live

	tests := []struct {
		name string
		s    db.Session
		dead bool
	}{
		{
			name: "live: known window + live pid",
			s:    gcSess(100, 42),
			dead: false,
		},
		{
			name: "dead: window closed (absent from model)",
			s:    gcSess(101, 42),
			dead: true,
		},
		{
			name: "dead: terminal pid gone",
			s:    gcSess(100, 999),
			dead: true,
		},
		{
			// The whole point of the simplification: a live local session is NOT
			// reaped no matter how long it has been quiet (idle Claudes emit no
			// hooks and must be allowed to fade fully).
			name: "live: known window + live pid, quiet forever -> still alive",
			s:    gcSess(100, 42),
			dead: false,
		},
		{
			// Unresolved (neither window nor pid) shouldn't happen for a local
			// session, but if it does it's untrackable and invisible (maps to no
			// workspace) — never reaped here; SessionEnd clears it on clean exit.
			name: "unresolved: no window, no pid -> not reaped",
			s:    gcSess(-1, -1),
			dead: false,
		},
	}

	pred := deadPredicate(model, nil)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pred(tt.s); got != tt.dead {
				t.Fatalf("dead = %v, want %v", got, tt.dead)
			}
		})
	}
}

// stubProcStart makes the process-start seam serve fixed values.
func stubProcStart(t *testing.T, start map[int]string) {
	t.Helper()
	orig := procStartOf
	procStartOf = func(pid int) (string, bool) {
		v, ok := start[pid]
		return v, ok
	}
	t.Cleanup(func() { procStartOf = orig })
}

func TestClaudeProcessGone(t *testing.T) {
	// 100, 200 and 300 are alive; only 100 and 200 have a readable start time.
	orig := procExists
	alive := map[int]bool{100: true, 200: true, 300: true}
	procExists = func(pid int) bool { return alive[pid] }
	t.Cleanup(func() { procExists = orig })
	stubProcStart(t, map[int]string{100: "aaa", 200: "bbb"})

	cases := []struct {
		name string
		row  db.Session
		want bool
	}{
		{
			name: "no recorded pid: no evidence either way",
			row:  db.Session{},
			want: false,
		},
		{
			name: "pid alive, start time matches",
			row: db.Session{
				ClaudePID:       sql.NullInt64{Int64: 100, Valid: true},
				ClaudeProcStart: sql.NullString{String: "aaa", Valid: true},
			},
			want: false,
		},
		{
			name: "pid gone",
			row:  db.Session{ClaudePID: sql.NullInt64{Int64: 999, Valid: true}},
			want: true,
		},
		{
			// Background agents come from a pre-forked spare pool, so pid reuse is
			// routine rather than theoretical.
			name: "pid reused by a different process",
			row: db.Session{
				ClaudePID:       sql.NullInt64{Int64: 100, Valid: true},
				ClaudeProcStart: sql.NullString{String: "an-older-start", Valid: true},
			},
			want: true,
		},
		{
			name: "pid alive, no recorded start time: not enough to condemn",
			row:  db.Session{ClaudePID: sql.NullInt64{Int64: 200, Valid: true}},
			want: false,
		},
		{
			name: "pid alive, live start time unreadable: not enough to condemn",
			row: db.Session{
				ClaudePID:       sql.NullInt64{Int64: 300, Valid: true},
				ClaudeProcStart: sql.NullString{String: "aaa", Valid: true},
			},
			want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := claudeProcessGone(c.row); got != c.want {
				t.Errorf("claudeProcessGone = %v, want %v", got, c.want)
			}
		})
	}
}

// A dead background agent must be reaped even though it never had a window — the
// leak this whole change set exists to close.
func TestFirstPartyDeadReapsBackgroundRows(t *testing.T) {
	d := &daemon{fpMiss: map[string]int{}}
	bg := db.Session{SessionID: "bg-1", Kind: sql.NullString{String: "bg", Valid: true}}
	// Someone else is still live, so the first-party set is "available".
	fp := map[string]clauded.Session{"other": {SessionID: "other"}}

	for i := 1; i < firstPartyMissThreshold; i++ {
		if dead := d.firstPartyDead([]db.Session{bg}, fp, true); dead["bg-1"] {
			t.Fatalf("reaped after only %d miss(es); want %d", i, firstPartyMissThreshold)
		}
	}
	if dead := d.firstPartyDead([]db.Session{bg}, fp, true); !dead["bg-1"] {
		t.Errorf("a background row absent for %d ticks should be reaped", firstPartyMissThreshold)
	}
}

// ...and an ssh/tmux row, which is ALSO windowless, must never be.
func TestFirstPartyDeadNeverReapsWindowlessNonBackgroundRows(t *testing.T) {
	d := &daemon{fpMiss: map[string]int{}}
	ssh := db.Session{SessionID: "ssh-1"} // no window, no kind, no job
	fp := map[string]clauded.Session{"other": {SessionID: "other"}}

	for range firstPartyMissThreshold * 3 {
		if dead := d.firstPartyDead([]db.Session{ssh}, fp, true); dead["ssh-1"] {
			t.Fatal("a windowless row with no background marker must never be reaped here")
		}
	}
	if len(d.fpMiss) != 0 {
		t.Errorf("untracked rows should not even get a miss counter: %+v", d.fpMiss)
	}
}

// A background agent that reappears has its counter cleared, like any other row.
func TestFirstPartyDeadResetsBackgroundCounterOnReappearance(t *testing.T) {
	d := &daemon{fpMiss: map[string]int{}}
	bg := db.Session{SessionID: "bg-1", JobID: sql.NullString{String: "j", Valid: true}}
	absent := map[string]clauded.Session{"other": {SessionID: "other"}}
	present := map[string]clauded.Session{"bg-1": {SessionID: "bg-1"}}

	d.firstPartyDead([]db.Session{bg}, absent, true)
	d.firstPartyDead([]db.Session{bg}, present, true)
	if d.fpMiss["bg-1"] != 0 {
		t.Errorf("counter = %d after reappearance, want 0", d.fpMiss["bg-1"])
	}
}
