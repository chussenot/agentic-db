package doctor

import (
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mrzor/claude-status/internal/db"
	"github.com/mrzor/claude-status/internal/fleet"
	"github.com/mrzor/claude-status/internal/state"
)

// colonyFixture lays out a machine mid-colony: an interactive session that spawned
// three background agents, one of which is waiting on a human. Returns the options
// pointing at the fixture.
func colonyFixture(t *testing.T) fleetOpts {
	t.Helper()
	root := t.TempDir()
	sessionsDir := filepath.Join(root, "sessions")
	jobsDir := filepath.Join(root, "jobs")
	for _, d := range []string{sessionsDir, jobsDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}

	// The owner is this test process, so its pid is genuinely alive: clauded's
	// liveness check is real, not stubbed, in this test.
	ownerPID := os.Getpid()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(sessionsDir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("owner.json", `{"pid":`+strconv.Itoa(ownerPID)+`,"sessionId":"owner-1111","kind":"interactive",
		"name":"project-manager-f4","cwd":"/repo","status":"idle","statusUpdatedAt":1786700000000}`)
	// The agents' pids are this process too — the point of the fixture is the
	// join, and a dead pid would (correctly) drop them from the live set.
	for _, a := range []struct{ id, name, job, status, waitingFor string }{
		{"agent-aaaa", "project-manager-3", "aaaa1111", "waiting", "permission prompt"},
		{"agent-bbbb", "project-manager-5", "bbbb2222", "busy", ""},
		{"agent-cccc", "project-manager-helpdesk", "cccc3333", "idle", ""},
	} {
		write(a.id+".json", `{"pid":`+strconv.Itoa(ownerPID)+`,"sessionId":"`+a.id+`","kind":"bg","jobId":"`+a.job+`",
			"name":"`+a.name+`","cwd":"/repo","status":"`+a.status+`","waitingFor":"`+a.waitingFor+`",
			"statusUpdatedAt":1786714000000}`)
	}

	writeJob := func(job, body string) {
		dir := filepath.Join(jobsDir, job)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir job: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(body), 0o600); err != nil {
			t.Fatalf("write job: %v", err)
		}
	}
	writeJob("aaaa1111", `{"state":"working","tempo":"blocked","cwd":"/repo","name":"project-manager-3",
		"sessionId":"agent-aaaa","needs":"approve Bash: rm -rf nothing","detail":"awaiting diff verification",
		"inFlight":{"tasks":2,"queued":0},"tokens":23633,"worktreeBranch":"worktree-pm-3"}`)
	writeJob("bbbb2222", `{"state":"working","tempo":"active","cwd":"/repo","name":"project-manager-5",
		"sessionId":"agent-bbbb","detail":"verifying quotes","inFlight":{"tasks":1,"queued":0}}`)
	writeJob("cccc3333", `{"state":"done","tempo":"idle","cwd":"/repo","name":"project-manager-helpdesk",
		"sessionId":"agent-cccc","detail":"decision sent"}`)

	dbPath := filepath.Join(root, "claude.sqlite")
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()
	// The owner last talked 19 hours before the agents' last activity — the shape
	// of the bug this command exists to show.
	rows := []db.Session{
		{SessionID: "owner-1111", State: string(state.Idle), WindowID: sql.NullInt64{Int64: 68, Valid: true},
			LastTalkTS: sql.NullInt64{Int64: 1786714000000 - 19*3600*1000, Valid: true},
			LastSeenTS: 1786714000000 - 19*3600*1000, CreatedTS: 1},
		{SessionID: "agent-aaaa", State: string(state.Prompt), LastSeenTS: 1786714000000, CreatedTS: 1,
			Kind: sql.NullString{String: "bg", Valid: true}},
		{SessionID: "agent-bbbb", State: string(state.Working), LastSeenTS: 1786714000000, CreatedTS: 1,
			Kind: sql.NullString{String: "bg", Valid: true}},
		// agent-cccc deliberately has NO row: a live agent we are blind to.
	}
	for _, r := range rows {
		if err := d.Upsert(r); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}

	return fleetOpts{
		dbPath:      dbPath,
		sessionsDir: sessionsDir,
		jobsDir:     jobsDir,
		rosterPath:  filepath.Join(root, "no-roster.json"),
	}
}

func TestFleetViewJoinsSourcesAndRanksDemand(t *testing.T) {
	v, err := buildFleetView(colonyFixture(t))
	if err != nil {
		t.Fatalf("buildFleetView: %v", err)
	}
	if len(v.Agents) != 3 {
		t.Fatalf("got %d agents, want 3: %+v", len(v.Agents), v.Agents)
	}
	// Worst demand first.
	if v.Agents[0].Demand != "awaits-human" || v.Agents[0].Name != "project-manager-3" {
		t.Errorf("first agent = %+v, want the one awaiting a human", v.Agents[0])
	}
	a := v.Agents[0]
	if a.Needs != "approve Bash: rm -rf nothing" || a.Lifecycle != "working" || a.InFlight != 2 || a.Tokens != 23633 {
		t.Errorf("job fields not joined: %+v", a)
	}
	if a.Owner != "owner-1111" || a.OwnerSource != string(fleet.OwnerFromCwd) {
		// No spawn env in a fixture, so re-homing on the job's cwd is the rung
		// that should answer.
		t.Errorf("owner = %q via %q, want owner-1111 via cwd", a.Owner, a.OwnerSource)
	}

	// The agent with no DB row must be visibly flagged, not silently equal.
	var blind []string
	for _, ag := range v.Agents {
		if !ag.InDB {
			blind = append(blind, ag.Name)
		}
	}
	if len(blind) != 1 || blind[0] != "project-manager-helpdesk" {
		t.Errorf("blind spots = %v, want just the helpdesk agent", blind)
	}
}

func TestFleetViewShowsOwnIdleVersusFleetIdle(t *testing.T) {
	v, err := buildFleetView(colonyFixture(t))
	if err != nil {
		t.Fatalf("buildFleetView: %v", err)
	}
	if len(v.Fleets) != 1 {
		t.Fatalf("got %d fleets, want 1: %+v", len(v.Fleets), v.Fleets)
	}
	f := v.Fleets[0]
	if f.SessionID != "owner-1111" || f.Children != 3 || f.Live != 2 {
		t.Errorf("rollup = %+v, want owner-1111 with 3 children / 2 live", f)
	}
	if f.Awaiting != 1 || f.Worst != "awaits-human" || f.Neediest != "project-manager-3" {
		t.Errorf("demand summary wrong: %+v", f)
	}
	// The whole point: the session's own idle age is hours, its fleet's is not.
	if f.OwnIdleSeconds < 18*3600 {
		t.Errorf("OwnIdleSeconds = %v, want ~19h (the raw last_talk_ts)", f.OwnIdleSeconds)
	}
	if f.FleetIdleSeconds >= f.OwnIdleSeconds {
		t.Errorf("FleetIdleSeconds (%v) should be much fresher than OwnIdleSeconds (%v)",
			f.FleetIdleSeconds, f.OwnIdleSeconds)
	}
}

func TestFleetTableAndJSONRender(t *testing.T) {
	opts := colonyFixture(t)
	var text strings.Builder
	if err := runFleet(&text, opts); err != nil {
		t.Fatalf("runFleet: %v", err)
	}
	for _, want := range []string{
		"background agents: 3", "project-manager-3", "awaits-human",
		"fleets by session: 1", "OWN_IDLE", "FLEET_IDLE", "approve Bash",
	} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("table output missing %q:\n%s", want, text.String())
		}
	}

	opts.asJSON = true
	var jsonOut strings.Builder
	if err := runFleet(&jsonOut, opts); err != nil {
		t.Fatalf("runFleet --json: %v", err)
	}
	var v fleetView
	if err := json.Unmarshal([]byte(jsonOut.String()), &v); err != nil {
		t.Fatalf("--json is not valid JSON: %v\n%s", err, jsonOut.String())
	}
	if len(v.Agents) != 3 || len(v.Fleets) != 1 {
		t.Errorf("decoded JSON = %d agents / %d fleets, want 3/1", len(v.Agents), len(v.Fleets))
	}
}

// A machine that never ran a background agent must still print a useful page.
func TestFleetViewWithNoBackgroundAgents(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "claude.sqlite")
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	d.Close()

	var out strings.Builder
	err = runFleet(&out, fleetOpts{
		dbPath:      dbPath,
		sessionsDir: filepath.Join(root, "absent-sessions"),
		jobsDir:     filepath.Join(root, "absent-jobs"),
		rosterPath:  filepath.Join(root, "absent-roster.json"),
	})
	if err != nil {
		t.Fatalf("runFleet on a bare machine: %v", err)
	}
	if !strings.Contains(out.String(), "background agents: 0") {
		t.Errorf("expected an empty-but-valid page, got:\n%s", out.String())
	}
}

func TestCrossCheckDiffsBothDirections(t *testing.T) {
	// Stub `claude agents --json`: it knows about bg-1 and bg-3, we know bg-1 and
	// bg-2. So bg-2 is a leak of ours and bg-3 is a blind spot.
	orig := execCommand
	execCommand = func(name string, args ...string) *exec.Cmd {
		payload := `[{"sessionId":"bg-1","kind":"background"},
		             {"sessionId":"bg-3","kind":"background"},
		             {"sessionId":"int-1","kind":"interactive"}]`
		return exec.Command("printf", "%s", payload)
	}
	t.Cleanup(func() { execCommand = orig })

	diff := crossCheckAgents(map[string]fleet.Child{
		"bg-1": {SessionID: "bg-1"},
		"bg-2": {SessionID: "bg-2"},
	})
	if diff.Error != "" {
		t.Fatalf("unexpected error: %s", diff.Error)
	}
	if diff.Ours != 2 || diff.Theirs != 2 {
		t.Errorf("counts = ours %d theirs %d, want 2/2 (interactive rows excluded)", diff.Ours, diff.Theirs)
	}
	if len(diff.Leaked) != 1 || diff.Leaked[0] != "bg-2" {
		t.Errorf("Leaked = %v, want [bg-2]", diff.Leaked)
	}
	if len(diff.Missed) != 1 || diff.Missed[0] != "bg-3" {
		t.Errorf("Missed = %v, want [bg-3]", diff.Missed)
	}
}

func TestCrossCheckMissingBinaryIsNotFatal(t *testing.T) {
	orig := execCommand
	execCommand = func(name string, args ...string) *exec.Cmd {
		return exec.Command("definitely-not-a-real-binary-xyzzy")
	}
	t.Cleanup(func() { execCommand = orig })

	diff := crossCheckAgents(map[string]fleet.Child{})
	if diff.Error == "" {
		t.Error("a missing `claude` binary should be reported, not panic or pass silently")
	}
}

func TestOrphanSweepListsThenDeletes(t *testing.T) {
	root := t.TempDir()
	// A live first-party set is required: it is the liveness authority, and one of
	// its entries is a QUIET background agent whose last_seen is days old. Age must
	// not be enough to condemn it.
	sessionsDir := filepath.Join(root, "sessions")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sessionsDir, "1.json"),
		[]byte(`{"pid":`+strconv.Itoa(os.Getpid())+`,"sessionId":"quiet-monitor","kind":"bg","jobId":"j",
			"name":"project-manager-keeper","status":"shell","statusUpdatedAt":1}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	dbPath := filepath.Join(root, "claude.sqlite")
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()

	now := db.Now().UnixMilli()
	day := int64(24 * 3600 * 1000)
	rows := []db.Session{
		// A pre-columns leak: windowless, no claude_pid, ancient.
		{SessionID: "leaked-1", State: string(state.Idle), LastSeenTS: now - 30*day, CreatedTS: 1,
			Cwd: sql.NullString{String: "/home/e/taf/project-manager", Valid: true}},
		// A windowed session: the ordinary window pass owns it, not this one.
		{SessionID: "windowed", State: string(state.Idle), WindowID: sql.NullInt64{Int64: 68, Valid: true},
			LastSeenTS: now - 30*day, CreatedTS: 1},
		// Old-looking, but its Claude process is alive — a background agent that has
		// been monitoring quietly for days is not an orphan.
		{SessionID: "alive-monitor", State: string(state.Idle), LastSeenTS: now - 3*day, CreatedTS: 1,
			Kind:      sql.NullString{String: "bg", Valid: true},
			ClaudePID: sql.NullInt64{Int64: int64(os.Getpid()), Valid: true}},
		// Windowless and dead, but too fresh to call.
		{SessionID: "fresh", State: string(state.Idle), LastSeenTS: now - 60_000, CreatedTS: 1},
		// A LIVE background agent that has been monitoring quietly for days: no
		// window, no claude_pid recorded, ancient last_seen. Claude Code still
		// lists it, and that alone must save it. This is the case that made the
		// first version of the sweep condemn four real agents.
		{SessionID: "quiet-monitor", State: string(state.Idle), LastSeenTS: now - 5*day, CreatedTS: 1,
			Kind: sql.NullString{String: "bg", Valid: true}},
	}
	for _, r := range rows {
		if err := d.Upsert(r); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}

	// Dry run: lists the leak, touches nothing.
	var dry strings.Builder
	if err := runOrphans(&dry, d, sessionsDir, 24*time.Hour, false); err != nil {
		t.Fatalf("runOrphans dry: %v", err)
	}
	out := dry.String()
	if !strings.Contains(out, "would delete 1") || !strings.Contains(out, "dry run") {
		t.Errorf("dry run output wrong:\n%s", out)
	}
	for _, keep := range []string{"windowed", "alive-monitor", "fresh", "quiet-monitor"} {
		if strings.Contains(out, keep) {
			t.Errorf("%s must not be listed as an orphan:\n%s", keep, out)
		}
	}
	if all, _ := d.LoadLive(); len(all) != 5 {
		t.Errorf("dry run deleted rows: %d remain of 5", len(all))
	}

	// Confirmed: deletes exactly the one row.
	var wet strings.Builder
	if err := runOrphans(&wet, d, sessionsDir, 24*time.Hour, true); err != nil {
		t.Fatalf("runOrphans confirmed: %v", err)
	}
	if !strings.Contains(wet.String(), "deleted 1 row") {
		t.Errorf("delete output wrong:\n%s", wet.String())
	}
	all, _ := d.LoadLive()
	if len(all) != 4 {
		t.Fatalf("%d rows remain, want 4", len(all))
	}
	for _, s := range all {
		if s.SessionID == "leaked-1" {
			t.Error("the orphan survived")
		}
	}

	// Nothing left to do reads as such.
	var empty strings.Builder
	if err := runOrphans(&empty, d, sessionsDir, 24*time.Hour, false); err != nil {
		t.Fatalf("runOrphans empty: %v", err)
	}
	if !strings.Contains(empty.String(), "nothing older than") {
		t.Errorf("empty sweep output wrong:\n%s", empty.String())
	}
}

// With no readable first-party set there is no liveness authority, so the sweep must
// refuse rather than condemn every windowless row it can see.
func TestOrphanSweepRefusesWithoutLivenessAuthority(t *testing.T) {
	root := t.TempDir()
	d, err := db.Open(filepath.Join(root, "claude.sqlite"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()
	if err := d.Upsert(db.Session{SessionID: "windowless", State: string(state.Idle),
		LastSeenTS: 1, CreatedTS: 1}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	var out strings.Builder
	err = runOrphans(&out, d, filepath.Join(root, "absent-sessions"), time.Hour, true)
	if err == nil {
		t.Fatal("expected a refusal when the first-party set is unreadable/empty")
	}
	if !strings.Contains(err.Error(), "refusing to sweep") {
		t.Errorf("error = %v, want a refusal", err)
	}
	if all, _ := d.LoadLive(); len(all) != 1 {
		t.Errorf("the refusal must not delete anything; %d rows remain of 1", len(all))
	}
}
