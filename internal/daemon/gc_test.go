package daemon

import (
	"database/sql"
	"testing"
	"time"

	"github.com/mrzor/claude-status/internal/db"
)

// presenceSet is a test windowPresence: the set of window ids the model knows.
type presenceSet map[int]bool

func (p presenceSet) HasWindow(id int) bool { return p[id] }

func gcSess(windowID, termPID int, lastSeen int64) db.Session {
	s := db.Session{LastSeenTS: lastSeen}
	if windowID >= 0 {
		s.WindowID = sql.NullInt64{Int64: int64(windowID), Valid: true}
	}
	if termPID >= 0 {
		s.TerminalPID = sql.NullInt64{Int64: int64(termPID), Valid: true}
	}
	return s
}

func TestDeadPredicate(t *testing.T) {
	now := time.UnixMilli(1_000_000_000_000)
	freshSeen := now.UnixMilli()
	staleSeen := now.Add(-2 * staleThreshold).UnixMilli()

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
			name: "live: known window, live pid, fresh heartbeat",
			s:    gcSess(100, 42, freshSeen),
			dead: false,
		},
		{
			name: "dead: window absent from model",
			s:    gcSess(101, 42, freshSeen),
			dead: true,
		},
		{
			name: "dead: terminal pid gone",
			s:    gcSess(100, 999, freshSeen),
			dead: true,
		},
		{
			name: "dead: stale heartbeat (kill -9 net)",
			s:    gcSess(100, 42, staleSeen),
			dead: true,
		},
		{
			name: "remote: no window, no pid, fresh -> alive",
			s:    gcSess(-1, -1, freshSeen),
			dead: false,
		},
		{
			name: "remote: no window, no pid, stale -> reaped",
			s:    gcSess(-1, -1, staleSeen),
			dead: true,
		},
	}

	pred := deadPredicate(model, now)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pred(tt.s); got != tt.dead {
				t.Fatalf("dead = %v, want %v", got, tt.dead)
			}
		})
	}
}
