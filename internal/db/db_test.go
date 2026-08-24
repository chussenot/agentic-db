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

func strp(s string) *string { return &s }
func i64p(i int64) *int64   { return &i }

func TestIdentityRoundTrip(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "claude.sqlite"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	s := Session{
		SessionID:       "bg-1",
		Cwd:             newStr("/home/e/taf/project-manager/.claude/worktrees/project-manager-3"),
		State:           string(state.Working),
		LastSeenTS:      1000,
		CreatedTS:       1000,
		Kind:            newStr("bg"),
		SessionName:     newStr("project-manager-3"),
		JobID:           newStr("44644cdb"),
		ClaudePID:       newInt(2920644),
		ClaudeProcStart: newStr("27771327"),
		OwnerSessionID:  newStr("2e1ce074"),
	}
	if err := d.Upsert(s); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, found, err := d.Get("bg-1")
	if err != nil || !found {
		t.Fatalf("Get: found=%v err=%v", found, err)
	}
	if got.Kind != s.Kind || got.SessionName != s.SessionName || got.JobID != s.JobID ||
		got.ClaudePID != s.ClaudePID || got.ClaudeProcStart != s.ClaudeProcStart ||
		got.OwnerSessionID != s.OwnerSessionID {
		t.Errorf("identity round-trip mismatch:\n got %+v\nwant %+v", got, s)
	}
	if !got.IsBackground() {
		t.Error("IsBackground() = false for a kind=bg row")
	}
	// Console edge was never set, so it stays NULL rather than defaulting to
	// anything: NULL means "not resolved", never "no".
	if got.ConsoleSessionID.Valid || got.ConsoleTS.Valid {
		t.Errorf("console edge should be NULL, got %+v / %+v", got.ConsoleSessionID, got.ConsoleTS)
	}
}

func TestIsBackgroundIgnoresMissingWindow(t *testing.T) {
	// An ssh/tmux session also has no window. Only the positive markers count.
	if (Session{SessionID: "ssh"}).IsBackground() {
		t.Error("a windowless row with no kind/job_id must not read as background")
	}
	if !(Session{Kind: newStr("bg")}).IsBackground() {
		t.Error("kind=bg should read as background")
	}
	if !(Session{JobID: newStr("44644cdb")}).IsBackground() {
		t.Error("a job_id should read as background")
	}
	if (Session{Kind: newStr("interactive"), WindowID: newInt(16)}).IsBackground() {
		t.Error("an interactive row must not read as background")
	}
}

// A hook Upsert must not null out identity the daemon resolved. The hook builds
// its row from a Get that may predate the daemon's write, so the merge has to be
// "last non-NULL wins", not "excluded wins".
func TestUpsertDoesNotClobberDaemonIdentity(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "claude.sqlite"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	// 1. The hook creates the row knowing only what its environment told it.
	hookRow := Session{
		SessionID:  "bg-2",
		State:      string(state.Working),
		LastSeenTS: 1000,
		CreatedTS:  1000,
		Kind:       newStr("bg"),
		JobID:      newStr("44644cdb"),
	}
	if err := d.Upsert(hookRow); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// 2. The daemon resolves the name and the owner.
	changed, err := d.SetIdentity("bg-2", Identity{
		SessionName:    strp("project-manager-3"),
		OwnerSessionID: strp("2e1ce074"),
	})
	if err != nil || !changed {
		t.Fatalf("SetIdentity: changed=%v err=%v", changed, err)
	}

	// 3. The next hook fires with the row it read in step 1 (no name, no owner).
	hookRow.LastSeenTS = 2000
	if err := d.Upsert(hookRow); err != nil {
		t.Fatalf("second Upsert: %v", err)
	}

	got, _, err := d.Get("bg-2")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.SessionName.String != "project-manager-3" {
		t.Errorf("session_name clobbered by hook Upsert: %+v", got.SessionName)
	}
	if got.OwnerSessionID.String != "2e1ce074" {
		t.Errorf("owner_session_id clobbered by hook Upsert: %+v", got.OwnerSessionID)
	}
	if got.LastSeenTS != 2000 {
		t.Errorf("last_seen_ts = %d, want 2000 (non-identity columns still overwrite)", got.LastSeenTS)
	}
}

func TestSetIdentityNarrowAndTolerant(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "claude.sqlite"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	if err := d.Upsert(Session{
		SessionID: "s", State: string(state.Idle), LastSeenTS: 1, CreatedTS: 1,
		Kind: newStr("bg"), SessionName: newStr("keeper"),
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	// A patch with no fields writes nothing and is not an error.
	if changed, err := d.SetIdentity("s", Identity{}); err != nil || changed {
		t.Errorf("empty patch: changed=%v err=%v", changed, err)
	}
	// The console edge is allowed to MOVE (a fleet can be watched from elsewhere).
	if _, err := d.SetIdentity("s", Identity{ConsoleSessionID: strp("adf10a5f"), ConsoleTS: i64p(5)}); err != nil {
		t.Fatalf("SetIdentity console: %v", err)
	}
	if _, err := d.SetIdentity("s", Identity{ConsoleSessionID: strp("2e1ce074"), ConsoleTS: i64p(9)}); err != nil {
		t.Fatalf("SetIdentity console move: %v", err)
	}
	got, _, _ := d.Get("s")
	if got.ConsoleSessionID.String != "2e1ce074" || got.ConsoleTS.Int64 != 9 {
		t.Errorf("console edge did not move: %+v / %+v", got.ConsoleSessionID, got.ConsoleTS)
	}
	// Untouched fields survive.
	if got.SessionName.String != "keeper" {
		t.Errorf("session_name = %+v, want keeper", got.SessionName)
	}
	// A reaped row is not an error, just no change.
	if changed, err := d.SetIdentity("gone", Identity{Kind: strp("bg")}); err != nil || changed {
		t.Errorf("missing row: changed=%v err=%v", changed, err)
	}
}

// A DB created before these columns existed must migrate in place, keeping its
// rows, with the new columns reading NULL.
func TestMigrateAddsIdentityColumnsToOldDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.sqlite")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := old.Exec(`
CREATE TABLE sessions (
  session_id    TEXT PRIMARY KEY,
  cwd           TEXT,
  window_id     INTEGER,
  terminal_pid  INTEGER,
  state         TEXT NOT NULL,
  notify_kind   TEXT,
  last_talk_ts  INTEGER,
  last_event_ts INTEGER,
  last_seen_ts  INTEGER NOT NULL,
  created_ts    INTEGER NOT NULL
);
INSERT INTO sessions (session_id, cwd, state, last_seen_ts, created_ts)
VALUES ('legacy', '/home/e/old', 'idle', 42, 42);`); err != nil {
		t.Fatalf("seed old schema: %v", err)
	}
	old.Close()

	d, err := Open(path)
	if err != nil {
		t.Fatalf("Open (migrate): %v", err)
	}
	defer d.Close()

	got, found, err := d.Get("legacy")
	if err != nil || !found {
		t.Fatalf("legacy row lost: found=%v err=%v", found, err)
	}
	if got.Cwd.String != "/home/e/old" || got.LastSeenTS != 42 {
		t.Errorf("legacy row corrupted: %+v", got)
	}
	if got.Kind.Valid || got.JobID.Valid || got.ClaudePID.Valid || got.OwnerSessionID.Valid {
		t.Errorf("new columns should be NULL on a migrated row: %+v", got)
	}
	if got.IsBackground() {
		t.Error("a migrated legacy row must not read as background")
	}
	// And the migrated DB is writable through the new path.
	if changed, err := d.SetIdentity("legacy", Identity{Kind: strp("interactive")}); err != nil || !changed {
		t.Fatalf("SetIdentity on migrated row: changed=%v err=%v", changed, err)
	}
}
