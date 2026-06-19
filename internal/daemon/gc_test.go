package daemon

import (
	"database/sql"
	"testing"

	"github.com/mrzor/claude-status/internal/db"
)

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

	pred := deadPredicate(model)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pred(tt.s); got != tt.dead {
				t.Fatalf("dead = %v, want %v", got, tt.dead)
			}
		})
	}
}
