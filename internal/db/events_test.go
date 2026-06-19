package db

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func openTempDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "claude.sqlite"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestEventsRoundTripFilterPrune(t *testing.T) {
	d := openTempDB(t)

	// Insert 20 events, alternating sessions; one is a matched Notification.
	for i := 0; i < 20; i++ {
		sid := "s1"
		if i%2 == 0 {
			sid = "s2"
		}
		e := Event{TS: int64(1000 + i), SessionID: sid, Event: "Stop", NewState: "idle"}
		if i == 7 { // a Notification we want to find later
			e = Event{
				TS: int64(1000 + i), SessionID: "s1", Event: "Notification",
				Message:  sql.NullString{String: "Claude needs your permission", Valid: true},
				Matched:  sql.NullBool{Bool: true, Valid: true},
				NewState: "prompt",
			}
		}
		if err := d.InsertEvent(e); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	// Newest-first ordering.
	all, err := d.RecentEvents(100, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 20 {
		t.Fatalf("RecentEvents = %d rows, want 20", len(all))
	}
	if all[0].TS != 1019 {
		t.Errorf("newest row TS = %d, want 1019 (newest first)", all[0].TS)
	}

	// Session filter.
	s1, err := d.RecentEvents(100, "s1")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range s1 {
		if e.SessionID != "s1" {
			t.Fatalf("filter leak: got session %q", e.SessionID)
		}
	}

	// The matched Notification preserved message + matched.
	var notif *Event
	for i := range all {
		if all[i].Event == "Notification" {
			notif = &all[i]
			break
		}
	}
	if notif == nil {
		t.Fatal("notification event not found")
	}
	if !notif.Matched.Valid || !notif.Matched.Bool {
		t.Errorf("matched = %v, want true", notif.Matched)
	}
	if notif.Message.String != "Claude needs your permission" {
		t.Errorf("message = %q, want the permission text", notif.Message.String)
	}
	if notif.NewState != "prompt" {
		t.Errorf("new_state = %q, want prompt", notif.NewState)
	}

	// Limit honored.
	five, err := d.RecentEvents(5, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(five) != 5 {
		t.Errorf("limit 5 returned %d rows", len(five))
	}

	// Prune keeps only the newest N.
	if _, err := d.PruneEvents(6); err != nil {
		t.Fatal(err)
	}
	after, err := d.RecentEvents(100, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 6 {
		t.Fatalf("after PruneEvents(6): %d rows, want 6", len(after))
	}
	if after[0].TS != 1019 {
		t.Errorf("prune dropped the newest rows; newest TS = %d, want 1019", after[0].TS)
	}
	// The oldest survivor should be the 6 newest (TS 1014..1019).
	if after[len(after)-1].TS != 1014 {
		t.Errorf("oldest survivor TS = %d, want 1014", after[len(after)-1].TS)
	}
}
