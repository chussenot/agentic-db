package daemon

import (
	"database/sql"
	"testing"
	"time"

	"github.com/mrzor/claude-status/internal/db"
	"github.com/mrzor/claude-status/internal/state"
)

// mapResolver is a test workspaceResolver: window id -> workspace id, with an
// explicit "unknown" set for windows the model doesn't track.
type mapResolver map[int]int

func (m mapResolver) WindowWorkspace(windowID int) (int, bool) {
	ws, ok := m[windowID]
	return ws, ok
}

func sess(state string, windowID int, lastTalk int64) db.Session {
	s := db.Session{State: state}
	if windowID >= 0 {
		s.WindowID = sql.NullInt64{Int64: int64(windowID), Valid: true}
	}
	if lastTalk > 0 {
		s.LastTalkTS = sql.NullInt64{Int64: lastTalk, Valid: true}
	}
	return s
}

func TestAggregatePrecedence(t *testing.T) {
	now := time.UnixMilli(1_000_000)
	resolve := mapResolver{10: 1, 11: 1, 12: 1, 20: 2, 30: 3}

	sessions := []db.Session{
		// Workspace 1: idle + prompt + working -> working wins.
		sess("idle", 10, now.UnixMilli()),
		sess("prompt", 11, 0),
		sess("working", 12, 0),
		// Workspace 2: idle + prompt -> prompt wins.
		sess("prompt", 20, 0),
		// Workspace 3: idle only.
		sess("idle", 30, now.UnixMilli()),
	}
	// Add an idle session to ws2 to confirm prompt still beats it.
	sessions = append(sessions, db.Session{State: "idle", WindowID: sql.NullInt64{Int64: 21, Valid: true}})
	resolve[21] = 2

	got := aggregate(sessions, resolve, now)

	if got[1].status != state.Working {
		t.Errorf("ws1 = %v, want working", got[1].status)
	}
	if got[2].status != state.Prompt {
		t.Errorf("ws2 = %v, want prompt", got[2].status)
	}
	if got[3].status != state.Idle {
		t.Errorf("ws3 = %v, want idle", got[3].status)
	}
}

func TestAggregateShellPrecedence(t *testing.T) {
	now := time.UnixMilli(1_000_000)
	resolve := mapResolver{10: 1, 11: 1, 20: 2, 21: 2, 30: 3, 31: 3}

	sessions := []db.Session{
		// Workspace 1: shell + idle -> shell wins (more alive than idle).
		sess("shell", 10, 0),
		sess("idle", 11, now.UnixMilli()),
		// Workspace 2: shell + prompt -> prompt wins (needs-you outranks monitor).
		sess("shell", 20, 0),
		sess("prompt", 21, 0),
		// Workspace 3: shell + working -> working wins.
		sess("shell", 30, 0),
		sess("working", 31, 0),
	}
	got := aggregate(sessions, resolve, now)

	if got[1].status != state.Shell {
		t.Errorf("ws1 = %v, want shell (beats idle)", got[1].status)
	}
	if got[2].status != state.Prompt {
		t.Errorf("ws2 = %v, want prompt (beats shell)", got[2].status)
	}
	if got[3].status != state.Working {
		t.Errorf("ws3 = %v, want working (beats shell)", got[3].status)
	}
}

func TestAggregateDecayMostRecentWins(t *testing.T) {
	now := time.UnixMilli(100 * 60 * 1000) // t = 100 min in ms
	resolve := mapResolver{1: 7, 2: 7}

	// Two idle sessions on ws 7: one talked 30 min ago, one 2 min ago. The most
	// recent (2 min) wins -> brightest of the two: DecayLevel(2m) == level 1.
	old := now.Add(-30 * time.Minute).UnixMilli()
	recent := now.Add(-2 * time.Minute).UnixMilli()
	sessions := []db.Session{
		sess("idle", 1, old),
		sess("idle", 2, recent),
	}
	got := aggregate(sessions, resolve, now)
	if got[7].status != state.Idle {
		t.Fatalf("status = %v, want idle", got[7].status)
	}
	wantLevel := state.DecayLevel(2 * time.Minute)
	if got[7].level != wantLevel {
		t.Fatalf("level = %d, want %d (DecayLevel(2m))", got[7].level, wantLevel)
	}
	if wantLevel != 1 {
		t.Fatalf("sanity: DecayLevel(2m) = %d, expected 1", wantLevel)
	}
}

func TestAggregateSkipsUnresolvable(t *testing.T) {
	now := time.UnixMilli(1000)
	resolve := mapResolver{} // resolves nothing

	sessions := []db.Session{
		sess("working", 5, 0), // window not in model -> skipped
		{State: "working"},    // NULL window_id -> skipped
	}
	got := aggregate(sessions, resolve, now)
	if len(got) != 0 {
		t.Fatalf("expected empty result, got %v", got)
	}
}

func TestAggregateIdleNoTalkIsDimmest(t *testing.T) {
	now := time.UnixMilli(1000)
	resolve := mapResolver{1: 9}
	got := aggregate([]db.Session{sess("idle", 1, 0)}, resolve, now)
	if got[9].level != state.DecayLevels-1 {
		t.Fatalf("level = %d, want %d (dimmest)", got[9].level, state.DecayLevels-1)
	}
}

func TestSlotAllocator(t *testing.T) {
	sa := newSlotAllocator()

	// Sequential assignment hands out the lowest free slot, stably per workspace.
	s1, ok := sa.assign(100)
	if !ok || s1 != 1 {
		t.Fatalf("assign(100) = (%d,%v), want (1,true)", s1, ok)
	}
	s2, _ := sa.assign(200)
	if s2 != 2 {
		t.Fatalf("assign(200) = %d, want 2", s2)
	}
	// Re-assigning the same workspace is idempotent.
	if again, _ := sa.assign(100); again != 1 {
		t.Fatalf("re-assign(100) = %d, want 1", again)
	}

	// Freeing slot 1 lets it be reused by a new workspace.
	sa.free(100)
	if _, ok := sa.slotOf(100); ok {
		t.Fatal("slotOf(100) still present after free")
	}
	s3, _ := sa.assign(300)
	if s3 != 1 {
		t.Fatalf("assign(300) after free = %d, want 1 (reused)", s3)
	}
}

func TestSlotAllocatorExhaustion(t *testing.T) {
	sa := newSlotAllocator()
	for i := 0; i < state.MaxSlots; i++ {
		if _, ok := sa.assign(1000 + i); !ok {
			t.Fatalf("assign #%d unexpectedly failed", i)
		}
	}
	if _, ok := sa.assign(9999); ok {
		t.Fatal("assign past MaxSlots should fail")
	}
}

func TestSlotAllocatorAdopt(t *testing.T) {
	sa := newSlotAllocator()

	// Adopt slot 5 for ws 42 (as if reclaimed from a pre-existing "cw5" name).
	sa.adopt(42, 5)
	if s, ok := sa.slotOf(42); !ok || s != 5 {
		t.Fatalf("slotOf(42) = (%d,%v), want (5,true)", s, ok)
	}
	// A fresh assignment must not hand out the adopted slot 5.
	for i := 0; i < 4; i++ {
		s, _ := sa.assign(100 + i)
		if s == 5 {
			t.Fatalf("assign handed out adopted slot 5")
		}
	}
	// Adopting a slot already owned by another workspace is ignored.
	sa.adopt(43, 5)
	if _, ok := sa.slotOf(43); ok {
		t.Fatal("adopt of taken slot should be ignored")
	}
	// Out-of-range slots are ignored.
	sa.adopt(44, 0)
	sa.adopt(45, state.MaxSlots+1)
	if _, ok := sa.slotOf(44); ok {
		t.Fatal("adopt(slot=0) should be ignored")
	}
	if _, ok := sa.slotOf(45); ok {
		t.Fatal("adopt(slot>MaxSlots) should be ignored")
	}
}
