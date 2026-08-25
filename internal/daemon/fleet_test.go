package daemon

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mrzor/claude-status/internal/clauded"
	"github.com/mrzor/claude-status/internal/db"
	"github.com/mrzor/claude-status/internal/fleet"
	"github.com/mrzor/claude-status/internal/herdr"
	"github.com/mrzor/claude-status/internal/niri"
	"github.com/mrzor/claude-status/internal/state"
)

// fleetHarness builds a daemon over a temp DB and a job-record fixture.
func fleetHarness(t *testing.T, jobs map[string]string) (*daemon, string) {
	t.Helper()
	root := t.TempDir()
	jobsDir := filepath.Join(root, "jobs")
	for id, body := range jobs {
		dir := filepath.Join(jobsDir, id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(body), 0o600); err != nil {
			t.Fatalf("write job: %v", err)
		}
	}
	dbPath := filepath.Join(root, "claude.sqlite")
	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	return &daemon{
		db:           database,
		fleetReader:  fleet.NewReader(jobsDir),
		owners:       fleet.NewOwnerResolver(),
		identified:   map[string]identityRecord{},
		lastFleetLog: map[string]string{},
	}, dbPath
}

func bgFP(id, name, jobID string, pid int, status clauded.Status, needsPrompt bool) clauded.Session {
	s := clauded.Session{
		SessionID: id, Name: name, JobID: jobID, PID: pid, ProcStart: "1",
		Kind: clauded.KindBackground, Status: status,
		StatusUpdatedAt: time.UnixMilli(1786714000000), UpdatedAt: time.UnixMilli(1786714000000),
	}
	if needsPrompt {
		s.WaitingFor = "permission prompt"
	}
	return s
}

func TestTickFleetPersistsIdentityOnceAndPrunes(t *testing.T) {
	d, _ := fleetHarness(t, map[string]string{
		"job1": `{"state":"working","tempo":"active","cwd":"/repo","name":"pm-3","sessionId":"bg-1"}`,
	})
	// The owner row exists (a hook created it) and so does the agent's.
	for _, row := range []db.Session{
		{SessionID: "owner-1", State: string(state.Idle), LastSeenTS: 1, CreatedTS: 1},
		{SessionID: "bg-1", State: string(state.Working), LastSeenTS: 1, CreatedTS: 1},
	} {
		if err := d.db.Upsert(row); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}
	d.sessions, _ = d.db.LoadLive()

	fp := map[string]clauded.Session{
		"owner-1": {SessionID: "owner-1", Name: "project-manager-f4", PID: 100, ProcStart: "1",
			Kind: clauded.KindInteractive, Cwd: "/repo", Status: clauded.Idle},
		"bg-1": bgFP("bg-1", "pm-3", "job1", 200, clauded.Busy, false),
	}

	d.tickFleet(fp)

	// The agent's name — which the hook can never see — is now persisted, along
	// with its kind, job and owner.
	got, found, err := d.db.Get("bg-1")
	if err != nil || !found {
		t.Fatalf("get bg-1: found=%v err=%v", found, err)
	}
	if got.SessionName.String != "pm-3" || got.Kind.String != "bg" || got.JobID.String != "job1" {
		t.Errorf("identity not persisted: %+v", got)
	}
	if got.OwnerSessionID.String != "owner-1" {
		t.Errorf("owner = %+v, want owner-1 (re-homed on the job's cwd)", got.OwnerSessionID)
	}
	// The interactive session gets its name too — useful to recap, and free here.
	owner, _, _ := d.db.Get("owner-1")
	if owner.SessionName.String != "project-manager-f4" {
		t.Errorf("owner name = %+v", owner.SessionName)
	}

	// A steady state writes nothing more.
	if len(d.identified) != 2 {
		t.Fatalf("identified = %d entries, want 2", len(d.identified))
	}
	before := d.identified["bg-1"]
	for range 5 {
		d.tickFleet(fp)
	}
	if d.identified["bg-1"] != before {
		t.Errorf("identity record churned across ticks: %+v -> %+v", before, d.identified["bg-1"])
	}

	// When the agent goes away, its bookkeeping goes with it.
	delete(fp, "bg-1")
	d.tickFleet(fp)
	if _, still := d.identified["bg-1"]; still {
		t.Error("identity bookkeeping should be pruned once the session is gone")
	}
}

func TestTickFleetComputesRollup(t *testing.T) {
	d, _ := fleetHarness(t, map[string]string{
		"job1": `{"state":"working","tempo":"blocked","cwd":"/repo","name":"pm-3","sessionId":"bg-1",
			"needs":"approve Bash: rm -rf nothing","inFlight":{"tasks":2}}`,
		"job2": `{"state":"working","tempo":"active","cwd":"/repo","name":"pm-5","sessionId":"bg-2"}`,
		"job3": `{"state":"done","tempo":"idle","cwd":"/repo","name":"helpdesk","sessionId":"bg-3"}`,
	})
	fp := map[string]clauded.Session{
		"owner-1": {SessionID: "owner-1", Name: "pm-f4", PID: 100, ProcStart: "1",
			Kind: clauded.KindInteractive, Cwd: "/repo", Status: clauded.Idle},
		"bg-1": bgFP("bg-1", "pm-3", "job1", 200, clauded.Waiting, true),
		"bg-2": bgFP("bg-2", "pm-5", "job2", 201, clauded.Busy, false),
		"bg-3": bgFP("bg-3", "helpdesk", "job3", 202, clauded.Idle, false),
	}
	d.tickFleet(fp)

	r, ok := d.fleetRollups["owner-1"]
	if !ok {
		t.Fatalf("no rollup for the owner: %+v", d.fleetRollups)
	}
	if r.Total != 3 || r.Live != 2 {
		t.Errorf("Total/Live = %d/%d, want 3/2", r.Total, r.Live)
	}
	if r.Worst != fleet.DemandHuman || r.Neediest.Name != "pm-3" {
		t.Errorf("worst = %v via %q, want awaits-human via pm-3", r.Worst, r.Neediest.Name)
	}
	if len(d.fleetChildren) != 3 {
		t.Errorf("children = %d, want 3 (interactive sessions are not children)", len(d.fleetChildren))
	}
}

// With no first-party data (or the feature disabled) the daemon must hold no fleet
// state at all, rather than a stale copy.
func TestTickFleetClearsWhenNothingLive(t *testing.T) {
	d, _ := fleetHarness(t, map[string]string{
		"job1": `{"state":"working","cwd":"/repo","name":"pm-3","sessionId":"bg-1"}`,
	})
	fp := map[string]clauded.Session{
		"owner-1": {SessionID: "owner-1", PID: 100, Kind: clauded.KindInteractive, Cwd: "/repo"},
		"bg-1":    bgFP("bg-1", "pm-3", "job1", 200, clauded.Busy, false),
	}
	d.tickFleet(fp)
	if len(d.fleetRollups) == 0 {
		t.Fatal("expected a rollup before clearing")
	}
	d.tickFleet(map[string]clauded.Session{})
	if len(d.fleetRollups) != 0 || len(d.fleetChildren) != 0 {
		t.Errorf("fleet state should be cleared, got %d rollups / %d children",
			len(d.fleetRollups), len(d.fleetChildren))
	}
}

func TestTickFleetDisabledIsInert(t *testing.T) {
	d, _ := fleetHarness(t, nil)
	d.fleetReader = nil // --jobs-dir=""
	d.tickFleet(map[string]clauded.Session{
		"bg-1": bgFP("bg-1", "pm-3", "job1", 200, clauded.Busy, false),
	})
	if len(d.fleetRollups) != 0 {
		t.Errorf("background tracking disabled should compute nothing, got %+v", d.fleetRollups)
	}
}

// The guard for the whole exercise: none of this may reach the renderer yet. A
// background agent has no window, so what the bar shows must be byte-identical
// whether or not fleet data exists.
func TestFleetDataDoesNotChangeWhatTheBarShows(t *testing.T) {
	now := time.UnixMilli(1786714000000)
	talk := now.Add(-19 * time.Hour).UnixMilli()
	sessions := []db.Session{
		// An interactive session with a window, idle for 19 hours.
		{SessionID: "owner-1", State: string(state.Idle),
			WindowID:   sql.NullInt64{Int64: 68, Valid: true},
			LastTalkTS: sql.NullInt64{Int64: talk, Valid: true}, LastSeenTS: talk, CreatedTS: 1},
		// Its background agents: windowless, and one of them is at a prompt.
		{SessionID: "bg-1", State: string(state.Prompt), LastSeenTS: now.UnixMilli(), CreatedTS: 1,
			Kind: sql.NullString{String: "bg", Valid: true}},
		{SessionID: "bg-2", State: string(state.Working), LastSeenTS: now.UnixMilli(), CreatedTS: 1,
			Kind: sql.NullString{String: "bg", Valid: true}},
	}
	resolve := mapResolver{68: 10}

	want := aggregate([]db.Session{sessions[0]}, resolve, now)
	got := aggregate(sessions, resolve, now)

	if len(got) != len(want) {
		t.Fatalf("workspace count changed: %d vs %d", len(got), len(want))
	}
	for ws, w := range want {
		g, ok := got[ws]
		if !ok {
			t.Fatalf("workspace %d disappeared", ws)
		}
		if g != w {
			t.Errorf("workspace %d changed: %+v -> %+v (background rows must not reach the bar yet)", ws, w, g)
		}
	}
	// And specifically: the agent at a prompt must NOT have turned the owner's
	// workspace into a "?" — that is the rendering decision, deliberately deferred.
	if got[10].status == state.Prompt {
		t.Error("a background agent's prompt leaked into the workspace glyph")
	}
}

func TestPersistIdentitySkipsMissingRows(t *testing.T) {
	d, _ := fleetHarness(t, map[string]string{
		"job1": `{"state":"working","cwd":"/repo","name":"pm-3","sessionId":"bg-1"}`,
	})
	// No rows at all: the agent is live but no hook has fired for it yet.
	fp := map[string]clauded.Session{
		"bg-1": bgFP("bg-1", "pm-3", "job1", 200, clauded.Busy, false),
	}
	d.tickFleet(fp) // must not panic, must not record a write that never happened
	if _, recorded := d.identified["bg-1"]; recorded {
		t.Error("identity must not be recorded as written when the row does not exist")
	}
	// Once the row appears, the next tick fills it in.
	if err := d.db.Upsert(db.Session{SessionID: "bg-1", State: string(state.Working), LastSeenTS: 1, CreatedTS: 1}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	d.sessions, _ = d.db.LoadLive()
	d.tickFleet(fp)
	got, _, _ := d.db.Get("bg-1")
	if got.SessionName.String != "pm-3" {
		t.Errorf("identity not backfilled after the row appeared: %+v", got.SessionName)
	}
}

// The agent view retitles its terminal as you move through the list, so console
// detection has to be sticky: a window that has BEEN a console stays one until it
// closes. Without this, a console desktop flips back to reporting its resting host
// session ("idle 22h") the instant you scroll.
func TestNoteConsolesIsStickyUntilTheWindowCloses(t *testing.T) {
	d := &daemon{consoleWins: map[int64]bool{}}

	// Seen running the agent view.
	d.noteConsoles([]niri.Window{{ID: 16, Title: "6 awaiting input · claude agents"}})
	if !d.consoleWins[16] {
		t.Fatal("the aggregate title should mark a console")
	}

	// The view retitles to the focused agent — still the same console.
	d.noteConsoles([]niri.Window{{ID: 16, Title: "✳ project-manager-5"}})
	if !d.consoleWins[16] {
		t.Error("a console must stay one when its view retitles")
	}

	// An ordinary terminal is never a console, however it is titled.
	d.noteConsoles([]niri.Window{
		{ID: 16, Title: "✳ project-manager-5"},
		{ID: 29, Title: "~/taf/project-manager"},
	})
	if d.consoleWins[29] {
		t.Error("an ordinary terminal must not be marked")
	}

	// Closing the window is the one thing that clears it.
	d.noteConsoles([]niri.Window{{ID: 29, Title: "~/taf/project-manager"}})
	if d.consoleWins[16] {
		t.Error("a closed window's console marker should be dropped")
	}
	if len(d.consoleWins) != 0 {
		t.Errorf("console set should be empty, got %v", d.consoleWins)
	}
}

// herdrHarness builds a daemon whose herdr provider reads a fixed roster instead of
// spawning herdr, plus a session.json for the labels.
func herdrHarness(t *testing.T, sessionJSON string, agents []herdr.Agent) *daemon {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.json")
	if err := os.WriteFile(path, []byte(sessionJSON), 0o600); err != nil {
		t.Fatalf("write session.json: %v", err)
	}
	database, err := db.Open(filepath.Join(t.TempDir(), "claude.sqlite"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return &daemon{
		db: database,
		herdr: fleet.NewHerdrProviderFrom(
			func(context.Context) ([]herdr.Agent, error) { return agents, nil }, path),
	}
}

const herdrSessionFixture = `{"version":3,"workspaces":[
  {"id":"w1","custom_name":"project-manager-6","identity_cwd":"/home/e/pm",
   "worktree_space":{"key":"/home/e/pm/.git","label":"project-manager","repo_root":"/home/e/pm","checkout_path":"/home/e/pm","is_linked_worktree":false},
   "tabs":[{"panes":{"7":{"cwd":"/home/e/pm","agent_name":"keeper","managed_agent_kind":"claude"}}}]}
]}`

// The herd is read on the 1s tick and lands where the tile will look for it. A herdr
// pane is an ORDINARY interactive session, so none of this can hang off the
// background-agent path — hence tickHerds being separate from tickFleet.
func TestTickHerdsBuildsGroupsFromTheRoster(t *testing.T) {
	d := herdrHarness(t, herdrSessionFixture, []herdr.Agent{
		{Kind: "claude", Status: herdr.StatusBlocked, WorkspaceID: "w1", PaneID: "w1:pA", Name: "keeper"},
		{Kind: "claude", Status: herdr.StatusDone, WorkspaceID: "w1", PaneID: "w1:pB", Name: "helpdesk"},
	})
	d.tickHerds(context.Background(), map[string]clauded.Session{})

	if len(d.herds) != 1 {
		t.Fatalf("want one herd, got %+v", d.herds)
	}
	g := d.herds[0]
	if g.Key.Provider != fleet.ProviderHerdr || g.Key.Key != "w1" {
		t.Errorf("herd key = %+v, want herdr/w1", g.Key)
	}
	if g.Label != "project-manager-6" {
		t.Errorf("label = %q, want the workspace's custom name", g.Label)
	}
	sum := g.Summarize()
	if sum.Total != 2 || sum.Awaiting != 1 || sum.Live != 2 {
		t.Errorf("summary = %+v, want 2 agents, 1 awaiting, 2 live (`done` is idle)", sum)
	}
}

// A herdr that is not there must not leave a stale herd on the bar, and must not
// take the tick down either.
// A failed roster read is TOLERATED for a few ticks and only then believed. The
// original contract cleared on the first failure, on the reasoning that an
// unreachable herdr means no herd — right about a herdr that is genuinely gone, and
// destructive about a flaky read: one bad tick wiped every herd row off the bar AND
// pruned the per-workspace freshness ratchet, i.e. it destroyed the churn resistance
// this code exists to provide, a second before the next read succeeded. A stale herd
// row beats no row; three consecutive failures is a state worth believing.
func TestTickHerdsHoldsHerdsAcrossTransientFailuresThenClears(t *testing.T) {
	d := herdrHarness(t, herdrSessionFixture, []herdr.Agent{
		{Kind: "claude", Status: herdr.StatusIdle, WorkspaceID: "w1", PaneID: "w1:pA", Name: "keeper"},
	})
	d.tickHerds(context.Background(), nil)
	if len(d.herds) != 1 {
		t.Fatalf("setup: want one herd, got %+v", d.herds)
	}

	d.herdr = fleet.NewHerdrProviderFrom(
		func(context.Context) ([]herdr.Agent, error) {
			return nil, &herdr.ServerError{Code: "server_not_running"}
		},
		filepath.Join(t.TempDir(), "session.json"))

	for i := 1; i < herdrMissThreshold; i++ {
		d.tickHerds(context.Background(), nil)
		if len(d.herds) != 1 {
			t.Fatalf("failure %d of %d already dropped the herd: %+v", i, herdrMissThreshold, d.herds)
		}
	}
	d.tickHerds(context.Background(), nil)
	if d.herds != nil {
		t.Errorf("after %d consecutive failures the herd should be gone, got %+v", herdrMissThreshold, d.herds)
	}

	d.herdr = nil
	d.tickHerds(context.Background(), nil) // must not panic
}

// A failure streak that recovers must not leave the counter armed, or the next
// isolated hiccup would clear immediately.
func TestTickHerdsResetsToleranceOnRecovery(t *testing.T) {
	agents := []herdr.Agent{
		{Kind: "claude", Status: herdr.StatusIdle, WorkspaceID: "w1", PaneID: "w1:pA", Name: "keeper"},
	}
	d := herdrHarness(t, herdrSessionFixture, agents)
	d.tickHerds(context.Background(), nil)

	failing := fleet.NewHerdrProviderFrom(
		func(context.Context) ([]herdr.Agent, error) {
			return nil, &herdr.ServerError{Code: "server_not_running"}
		},
		filepath.Join(t.TempDir(), "session.json"))
	good := d.herdr

	for i := 1; i < herdrMissThreshold; i++ {
		d.herdr = failing
		d.tickHerds(context.Background(), nil)
	}
	d.herdr = good
	d.tickHerds(context.Background(), nil)
	if d.herdrMiss != 0 {
		t.Fatalf("a good read left the tolerance counter at %d", d.herdrMiss)
	}
	// One isolated failure after recovery must still be tolerated.
	d.herdr = failing
	d.tickHerds(context.Background(), nil)
	if len(d.herds) != 1 {
		t.Errorf("an isolated failure after recovery dropped the herd: %+v", d.herds)
	}
}

// The hook can only record a pane at SessionStart, so a pane that started earlier —
// or that simply runs for hours — never gets one, and the tile (which groups on the
// ROW's workspace id) cannot collapse it. herdr's roster names the session, so the
// daemon writes the membership back.
func TestPersistHerdMembershipBackfillsAndCorrects(t *testing.T) {
	d := herdrHarness(t, herdrSessionFixture, nil)
	rows := []db.Session{
		// A long-lived pane with no ids at all: the "active claude" case.
		{SessionID: "long-lived", State: string(state.Working), LastSeenTS: 1, CreatedTS: 1},
		// A pane whose id is STALE — herdr pane ids churn under a live session.
		{SessionID: "moved", State: string(state.Working), LastSeenTS: 1, CreatedTS: 1,
			HerdrWorkspaceID: sql.NullString{String: "w1", Valid: true},
			HerdrPaneID:      sql.NullString{String: "w1:pOLD", Valid: true}},
	}
	for _, r := range rows {
		if err := d.db.Upsert(r); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}
	d.sessions, _ = d.db.LoadLive()

	d.persistHerdMembership([]fleet.Group{{
		Key:   fleet.GroupKey{Provider: fleet.ProviderHerdr, Key: "w1"},
		Label: "project-manager",
		Members: []fleet.Member{
			{Name: "keeper", Ref: "w1:pNEW", SessionID: "long-lived"},
			{Name: "helpdesk", Ref: "w1:pNOW", SessionID: "moved"},
			{Name: "between-runs", Ref: "w1:pX"}, // no session: must be skipped, not panic
			{Name: "no-row", Ref: "w1:pY", SessionID: "absent"},
		},
	}})

	got, _, err := d.db.Get("long-lived")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.HerdrWorkspaceID.String != "w1" || got.HerdrPaneID.String != "w1:pNEW" {
		t.Errorf("backfill failed: ws=%+v pane=%+v", got.HerdrWorkspaceID, got.HerdrPaneID)
	}
	// A churned pane id is CORRECTED, which the hook's COALESCE merge cannot do.
	moved, _, _ := d.db.Get("moved")
	if moved.HerdrPaneID.String != "w1:pNOW" {
		t.Errorf("stale pane id not corrected: %+v", moved.HerdrPaneID)
	}
}

// A steady state must write nothing at all — this runs every tick.
func TestPersistHerdMembershipIsQuietWhenItAgrees(t *testing.T) {
	d := herdrHarness(t, herdrSessionFixture, nil)
	if err := d.db.Upsert(db.Session{
		SessionID: "s", State: string(state.Working), LastSeenTS: 1, CreatedTS: 1,
		HerdrWorkspaceID: sql.NullString{String: "w1", Valid: true},
		HerdrPaneID:      sql.NullString{String: "w1:pA", Valid: true},
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	d.sessions, _ = d.db.LoadLive()
	before, _, _ := d.db.Get("s")

	for range 5 {
		d.persistHerdMembership([]fleet.Group{{
			Key:     fleet.GroupKey{Provider: fleet.ProviderHerdr, Key: "w1"},
			Members: []fleet.Member{{Name: "keeper", Ref: "w1:pA", SessionID: "s"}},
		}})
	}
	after, _, _ := d.db.Get("s")
	if before.HerdrWorkspaceID != after.HerdrWorkspaceID || before.HerdrPaneID != after.HerdrPaneID {
		t.Errorf("a steady state changed the row: %+v -> %+v", before, after)
	}
}
