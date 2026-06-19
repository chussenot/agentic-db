package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/mrzor/claude-status/internal/state"
)

func newInt(i int64) sql.NullInt64   { return sql.NullInt64{Int64: i, Valid: true} }
func newStr(s string) sql.NullString { return sql.NullString{String: s, Valid: true} }

func TestOpenUpsertGetDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude.sqlite")
	d, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	s := Session{
		SessionID:   "sess-1",
		Cwd:         newStr("/home/zor/proj"),
		WindowID:    newInt(226),
		TerminalPID: newInt(489107),
		State:       string(state.Idle),
		LastTalkTS:  newInt(1000),
		LastEventTS: newInt(1000),
		LastSeenTS:  1000,
		CreatedTS:   1000,
	}
	if err := d.Upsert(s); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, found, err := d.Get("sess-1")
	if err != nil || !found {
		t.Fatalf("Get: found=%v err=%v", found, err)
	}
	if got.State != string(state.Idle) || got.WindowID.Int64 != 226 {
		t.Errorf("Get returned %+v", got)
	}

	// Update via upsert.
	s.State = string(state.Working)
	s.LastSeenTS = 2000
	if err := d.Upsert(s); err != nil {
		t.Fatalf("Upsert update: %v", err)
	}
	got, _, _ = d.Get("sess-1")
	if got.State != string(state.Working) || got.LastSeenTS != 2000 {
		t.Errorf("update not applied: %+v", got)
	}
	if got.CreatedTS != 1000 {
		t.Errorf("created_ts changed on update: %d", got.CreatedTS)
	}

	all, err := d.LoadLive()
	if err != nil || len(all) != 1 {
		t.Fatalf("LoadLive: n=%d err=%v", len(all), err)
	}

	if err := d.Delete("sess-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, found, _ := d.Get("sess-1"); found {
		t.Errorf("row still present after delete")
	}
	// Deleting a missing row is fine.
	if err := d.Delete("nope"); err != nil {
		t.Errorf("Delete missing: %v", err)
	}
}

func TestReapDead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude.sqlite")
	d, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	for _, id := range []string{"a", "b", "c"} {
		if err := d.Upsert(Session{SessionID: id, State: string(state.Idle), LastSeenTS: 1, CreatedTS: 1}); err != nil {
			t.Fatalf("Upsert %s: %v", id, err)
		}
	}
	// Reap everything but "b".
	n, err := d.ReapDead(func(s Session) bool { return s.SessionID != "b" })
	if err != nil || n != 2 {
		t.Fatalf("ReapDead n=%d err=%v", n, err)
	}
	all, _ := d.LoadLive()
	if len(all) != 1 || all[0].SessionID != "b" {
		t.Errorf("after reap: %+v", all)
	}
}

func TestNullablesPreserved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude.sqlite")
	d, _ := Open(path)
	defer d.Close()
	// Remote session: no window, no pid.
	if err := d.Upsert(Session{SessionID: "remote", State: string(state.Idle), LastSeenTS: 5, CreatedTS: 5}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, _, _ := d.Get("remote")
	if got.WindowID.Valid || got.TerminalPID.Valid {
		t.Errorf("expected NULL window/pid, got %+v", got)
	}
}
