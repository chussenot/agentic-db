package daemon

import (
	"database/sql"
	"testing"
	"time"

	"github.com/mrzor/claude-status/internal/db"
	"github.com/mrzor/claude-status/internal/state"
)

// comboModel is both a workspaceResolver and a windowPresence, so one idle
// session can be run through the real pipeline (GC reap -> aggregate) at many
// simulated instants. Keys are live window ids -> workspace id.
type comboModel map[int]int

func (m comboModel) WindowWorkspace(id int) (int, bool) { ws, ok := m[id]; return ws, ok }
func (m comboModel) HasWindow(id int) bool              { _, ok := m[id]; return ok }

// TestDecayLevelTimeline pins the pure level math. This PASSES — the bucket
// boundaries from the 60-minute design are correct in isolation.
func TestDecayLevelTimeline(t *testing.T) {
	cases := []struct {
		elapsed time.Duration
		want    int
	}{
		{0, 0}, {30 * time.Second, 0}, {59 * time.Second, 0},
		{time.Minute, 1}, {3 * time.Minute, 1},
		{4 * time.Minute, 2}, {9 * time.Minute, 2},
		{10 * time.Minute, 3}, {19 * time.Minute, 3},
		{20 * time.Minute, 4}, {34 * time.Minute, 4},
		{35 * time.Minute, 5}, {59 * time.Minute, 5},
		{60 * time.Minute, 6}, {3 * time.Hour, 6},
	}
	for _, c := range cases {
		if got := state.DecayLevel(c.elapsed); got != c.want {
			t.Errorf("DecayLevel(%s) = %d, want %d", c.elapsed, got, c.want)
		}
	}
}

// TestIdleDecayRendersFullFade simulates a single idle Claude over the whole
// 60-minute decay window and asserts the workspace name walks ci<>l0 -> l6.
//
// It models the REAL daemon pipeline at each instant: first GC (deadPredicate)
// reaps dead rows, then aggregate() names the survivors — exactly what happens
// live, where ReapDead DELETEs the row before the next LoadLive/reconcile.
//
// The terminal/window are kept ALIVE for the entire run (procExists stubbed
// true, window present in the model), so neither the window-closed nor the
// pid-gone arm of deadPredicate can fire. The ONLY thing that can remove the
// row is the last_seen staleness arm — and that is the bug.
func TestIdleDecayRendersFullFade(t *testing.T) {
	orig := procExists
	procExists = func(int) bool { return true } // terminal stays alive throughout
	defer func() { procExists = orig }()

	model := comboModel{100: 5} // window 100 lives on workspace 5, the whole time

	t0 := time.UnixMilli(1_700_000_000_000)
	// An idle Claude fires NO hooks after its Stop, so last_seen == last_talk and
	// both stay frozen at t0 for the entire idle period.
	idle := db.Session{
		SessionID:   "s1",
		State:       string(state.Idle),
		WindowID:    sql.NullInt64{Int64: 100, Valid: true},
		TerminalPID: sql.NullInt64{Int64: 42, Valid: true},
		LastTalkTS:  sql.NullInt64{Int64: t0.UnixMilli(), Valid: true},
		LastSeenTS:  t0.UnixMilli(),
	}

	checkpoints := []struct {
		elapsed time.Duration
		want    string
	}{
		{0, "ci1l0"}, // just stopped: bright ██
		{90 * time.Second, "ci1l1"},
		{5 * time.Minute, "ci1l2"},
		{12 * time.Minute, "ci1l3"},
		{25 * time.Minute, "ci1l4"},
		{45 * time.Minute, "ci1l5"},
		{65 * time.Minute, "ci1l6"}, // fully faded: dim ░░, and should REST here
	}

	for _, c := range checkpoints {
		now := t0.Add(c.elapsed)

		// Real pipeline step 1: GC reaps dead rows.
		pred := deadPredicate(model, nil)
		var live []db.Session
		if !pred(idle) {
			live = append(live, idle)
		}
		// Real pipeline step 2: name the survivors.
		got := aggregate(live, model, now)

		d, present := got[5]
		name := "(no dot — row reaped)"
		if present {
			name = state.Encode(d.status, 1, d.level)
		}
		t.Logf("elapsed=%-8s rendered=%-22s want=%s", c.elapsed, name, c.want)

		if !present || state.Encode(d.status, 1, d.level) != c.want {
			t.Errorf("at elapsed=%s: rendered %q, want %q", c.elapsed, name, c.want)
		}
	}
}
