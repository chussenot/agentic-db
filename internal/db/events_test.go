package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
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

func TestTitleChangedRoundTripAndLatestTitles(t *testing.T) {
	d := openTempDB(t)

	// Two TitleChanged rows for s1 (the later wins) and one for s2.
	rows := []Event{
		{TS: 100, SessionID: "s1", Event: "TitleChanged", NewState: "unchanged",
			WindowID: sql.NullInt64{Int64: 7, Valid: true}, WindowTitle: sql.NullString{String: "old-topic", Valid: true}},
		{TS: 200, SessionID: "s2", Event: "TitleChanged", NewState: "unchanged",
			WindowID: sql.NullInt64{Int64: 9, Valid: true}, WindowTitle: sql.NullString{String: "ci-base", Valid: true}},
		{TS: 300, SessionID: "s1", Event: "TitleChanged", NewState: "unchanged",
			WindowID: sql.NullInt64{Int64: 7, Valid: true}, WindowTitle: sql.NullString{String: "new-topic", Valid: true}},
		// A non-title hook row must never surface in LatestTitles.
		{TS: 400, SessionID: "s1", Event: "Stop", NewState: "idle"},
	}
	for _, e := range rows {
		if err := d.InsertEvent(e); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	// Round-trip: the window_title column survives.
	all, err := d.RecentEvents(100, "s2")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].WindowTitle.String != "ci-base" {
		t.Fatalf("s2 title round-trip: %+v", all)
	}

	latest, err := d.LatestTitles()
	if err != nil {
		t.Fatal(err)
	}
	if latest["s1"] != "new-topic" {
		t.Errorf("LatestTitles[s1] = %q, want new-topic (newest TitleChanged wins)", latest["s1"])
	}
	if latest["s2"] != "ci-base" {
		t.Errorf("LatestTitles[s2] = %q, want ci-base", latest["s2"])
	}
	if len(latest) != 2 {
		t.Errorf("LatestTitles size = %d, want 2 (Stop row excluded)", len(latest))
	}
}

// TestMigrateAddsWindowTitleColumn proves Open() upgrades a database created
// before the window_title column existed, rather than failing on the missing
// column. It builds a legacy events table by hand, then opens it through Open().
func TestMigrateAddsWindowTitleColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.sqlite")

	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	if _, err := legacy.Exec(`CREATE TABLE events (
  id INTEGER PRIMARY KEY AUTOINCREMENT, ts INTEGER NOT NULL, session_id TEXT NOT NULL,
  event TEXT NOT NULL, message TEXT, matched INTEGER, new_state TEXT NOT NULL, window_id INTEGER)`); err != nil {
		t.Fatalf("create legacy events: %v", err)
	}
	if _, err := legacy.Exec(`INSERT INTO events (ts, session_id, event, new_state) VALUES (1, 'old', 'Stop', 'idle')`); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}
	legacy.Close()

	d, err := Open(path)
	if err != nil {
		t.Fatalf("Open should migrate the legacy db, got: %v", err)
	}
	defer d.Close()

	// Opening twice must stay idempotent (ALTER swallows duplicate-column).
	d2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open should be idempotent, got: %v", err)
	}
	d2.Close()

	// The new column is usable and the legacy row reads back NULL for it.
	if err := d.InsertEvent(Event{TS: 2, SessionID: "new", Event: "TitleChanged", NewState: "unchanged",
		WindowTitle: sql.NullString{String: "hello", Valid: true}}); err != nil {
		t.Fatalf("insert after migrate: %v", err)
	}
	all, err := d.RecentEvents(10, "old")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].WindowTitle.Valid {
		t.Fatalf("legacy row should read NULL window_title: %+v", all)
	}
}
