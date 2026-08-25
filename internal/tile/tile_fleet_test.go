package tile

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mrzor/claude-status/internal/db"
	"github.com/mrzor/claude-status/internal/fleet"
	"github.com/mrzor/claude-status/internal/niri"
	"github.com/mrzor/claude-status/internal/state"
)

// fleetSummary builds a Summary with one demand per count, worst first — the shape
// Rollups produces.
func fleetSummary(awaiting, working, blocked, idle, done int, neediest, needs string) fleet.Summary {
	s := fleet.Summary{
		Awaiting: awaiting, Working: working, Blocked: blocked, Idle: idle, Done: done,
		Neediest: neediest, Needs: needs,
	}
	add := func(n int, d fleet.Demand) {
		for range n {
			s.Demands = append(s.Demands, d)
		}
	}
	add(awaiting, fleet.DemandHuman)
	add(working, fleet.DemandWorking)
	add(blocked, fleet.DemandBlocked)
	add(idle, fleet.DemandIdle)
	add(done, fleet.DemandDone)
	s.Total = len(s.Demands)
	s.Live = s.Total - done
	if len(s.Demands) > 0 {
		s.Worst = s.Demands[0]
	}
	return s
}

// A console window runs the agent view, not a conversation, so the fleet REPLACES
// its session row. This is the exact bug: the tile used to describe such a desktop
// by its resting host session ("idle 22h") while eleven agents were moving on it.
func TestConsoleWindowShowsFleetInsteadOfItsRestingSession(t *testing.T) {
	now := time.UnixMilli(1_000_000_000)
	dayOld := now.Add(-22 * time.Hour).UnixMilli()

	w := ws(5, "HDMI-A-1", 11, false)
	wins := []niri.Window{win(16, 11, "kitty", "6 awaiting input · claude agents")}
	sessions := []db.Session{claudeSession(16, "/home/e/taf/project-manager-1", "idle", dayOld)}
	fv := FleetView{
		Whole:    fleetSummary(4, 2, 0, 2, 3, "project-manager-7", "approve Bash: git mv …"),
		Consoles: map[int64]bool{16: true},
	}

	p := PayloadFor(w, wins, sessions, fv, now)
	if len(p.Items) != 1 {
		t.Fatalf("want a single row, got %d: %+v", len(p.Items), p.Items)
	}
	it := p.Items[0]
	if it.Fleet == nil {
		t.Fatal("the console row should carry fleet data")
	}
	if it.State != string(state.Prompt) {
		t.Errorf("state = %q, want prompt (an agent is waiting on the human)", it.State)
	}
	// The whole point: no stale idle age anywhere on this row.
	if it.IdleAgo != "" || it.IdleLevel != 0 {
		t.Errorf("a prompting fleet row must not carry an idle age: ago=%q level=%d", it.IdleAgo, it.IdleLevel)
	}
	if it.Fleet.Scope != ScopeWhole {
		t.Errorf("scope = %q, want %q (the agent view is machine-wide)", it.Fleet.Scope, ScopeWhole)
	}
	if it.Fleet.Total != 11 || it.Fleet.Live != 8 || it.Fleet.Awaiting != 4 {
		t.Errorf("counts wrong: %+v", it.Fleet)
	}
	if len(it.Fleet.Demands) != 11 || it.Fleet.Demands[0] != "awaits-human" {
		t.Errorf("bead strip wrong: %v", it.Fleet.Demands)
	}
	if it.Fleet.Neediest != "project-manager-7" || !strings.Contains(it.Fleet.Needs, "git mv") {
		t.Errorf("neediest/needs wrong: %+v", it.Fleet)
	}
	// Clicking the row must still focus the console window.
	if it.WinID != 16 {
		t.Errorf("win_id = %d, want 16", it.WinID)
	}
	if !strings.Contains(it.Title, "11 agents") || !strings.Contains(it.Title, "project-manager-7") {
		t.Errorf("title should degrade gracefully for a fleet-unaware template: %q", it.Title)
	}
}

// An owner desktop keeps its conversation AND gains a fleet row — the two-row layout
// finally earning its keep.
func TestOwnerDesktopGetsBothRows(t *testing.T) {
	now := time.UnixMilli(1_000_000_000)
	w := ws(10, "HDMI-A-1", 68, false)
	wins := []niri.Window{win(68, 68, "kitty", "project-manager-helpdesk")}
	s := claudeSession(68, "/home/e/taf/project-manager", "idle", now.Add(-4*time.Hour).UnixMilli())
	fv := FleetView{
		ByOwner: map[string]fleet.Summary{
			s.SessionID: fleetSummary(0, 3, 1, 0, 0, "", ""),
		},
	}

	p := PayloadFor(w, wins, []db.Session{s}, fv, now)
	if len(p.Items) != 2 {
		t.Fatalf("want 2 rows (session + fleet), got %d: %+v", len(p.Items), p.Items)
	}
	var sessionRow, fleetRow *Item
	for i := range p.Items {
		if p.Items[i].Fleet != nil {
			fleetRow = &p.Items[i]
		} else {
			sessionRow = &p.Items[i]
		}
	}
	if sessionRow == nil || fleetRow == nil {
		t.Fatalf("expected one session row and one fleet row: %+v", p.Items)
	}
	// The conversation row is untouched — it really is idle, and saying so is right.
	if sessionRow.State != "idle" || sessionRow.Folder != "project-manager" {
		t.Errorf("session row should be unchanged: %+v", sessionRow)
	}
	if fleetRow.State != string(state.Working) {
		t.Errorf("fleet row state = %q, want working", fleetRow.State)
	}
	if fleetRow.Fleet.Scope != ScopeOwned {
		t.Errorf("scope = %q, want %q", fleetRow.Fleet.Scope, ScopeOwned)
	}
	if fleetRow.Folder != "project-manager" {
		t.Errorf("fleet row folder = %q, want the owner's folder", fleetRow.Folder)
	}
}

// With more rows than the cap allows, the fleet row must survive: it is the one
// carrying "someone is waiting on you".
func TestFleetRowSurvivesTheCap(t *testing.T) {
	now := time.UnixMilli(1_000_000_000)
	w := ws(3, "HDMI-A-1", 30, false)
	wins := []niri.Window{
		win(40, 30, "kitty", "conversation"),
		win(41, 30, "firefox", "a browser"),
	}
	s := claudeSession(40, "/home/e/repo", "idle", now.Add(-time.Hour).UnixMilli())
	fv := FleetView{
		ByOwner: map[string]fleet.Summary{
			s.SessionID: fleetSummary(2, 0, 0, 0, 0, "worker-1", "approve rm"),
		},
	}

	p := PayloadFor(w, wins, []db.Session{s}, fv, now)
	if len(p.Items) != maxItemsPerTile {
		t.Fatalf("want %d rows, got %d", maxItemsPerTile, len(p.Items))
	}
	if p.Items[0].Fleet == nil || p.Items[0].State != string(state.Prompt) {
		t.Errorf("the prompting fleet row must come first: %+v", p.Items[0])
	}
	for _, it := range p.Items {
		if it.Kind == KindApp {
			t.Errorf("the app row should have been dropped before the fleet row: %+v", p.Items)
		}
	}
}

// A console window with no agents at all stays an ordinary session row: there is no
// fleet to describe, and inventing one would be its own lie.
func TestConsoleWithNoAgentsStaysASessionRow(t *testing.T) {
	now := time.UnixMilli(1_000_000_000)
	w := ws(5, "HDMI-A-1", 11, false)
	wins := []niri.Window{win(16, 11, "kitty", "0 awaiting input · claude agents")}
	sessions := []db.Session{claudeSession(16, "/home/e/repo", "idle", now.Add(-time.Hour).UnixMilli())}

	p := PayloadFor(w, wins, sessions, FleetView{Consoles: map[int64]bool{16: true}}, now)
	if len(p.Items) != 1 || p.Items[0].Fleet != nil {
		t.Fatalf("want a plain session row: %+v", p.Items)
	}
	if p.Items[0].IdleAgo == "" {
		t.Error("a genuinely idle session should still report its age")
	}
}

// The zero FleetView must reproduce the pre-fleet payload exactly — the guard that
// this change cannot affect a desktop that has no background agents.
func TestNoFleetViewIsUnchangedBehaviour(t *testing.T) {
	now := time.UnixMilli(1_000_000_000)
	w := ws(7, "HDMI-A-1", 70, true)
	wins := []niri.Window{win(50, 70, "kitty", "some work"), win(51, 70, "firefox", "FF")}
	sessions := []db.Session{claudeSession(50, "/home/e/repo", "working", 0)}

	got := PayloadFor(w, wins, sessions, FleetView{}, now)
	for _, it := range got.Items {
		if it.Fleet != nil {
			t.Errorf("no fleet data was supplied, yet a row carries some: %+v", it)
		}
	}
	if len(got.Items) != 2 || got.Items[0].State != "working" || got.Items[1].Kind != KindApp {
		t.Errorf("payload shape changed: %+v", got.Items)
	}
}

func TestFleetItemStateMapping(t *testing.T) {
	now := time.UnixMilli(1_000_000_000)
	cases := []struct {
		name    string
		sum     fleet.Summary
		want    string
		wantAgo bool
	}{
		{"anyone awaiting a human pulses", fleetSummary(1, 5, 2, 0, 0, "a", "approve"), string(state.Prompt), false},
		{"else working", fleetSummary(0, 2, 1, 0, 0, "", ""), string(state.Working), false},
		{
			// Blocked on ANOTHER AGENT, not on a person: shell is the "parked,
			// monitoring" indicator, which is what that actually is. Calling it
			// working would overstate it; idle would hide it.
			"blocked on itself is parked, not working",
			fleetSummary(0, 0, 3, 0, 0, "", ""), string(state.Shell), false,
		},
		{"all resting reports an age", fleetSummary(0, 0, 0, 2, 1, "", ""), string(state.Idle), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sum := c.sum
			sum.NewestActivity = now.Add(-9 * time.Minute).UnixMilli()
			it := fleetItem(sum, ScopeOwned, "repo", 1, now)
			if it.State != c.want {
				t.Errorf("state = %q, want %q", it.State, c.want)
			}
			if hasAgo := it.IdleAgo != ""; hasAgo != c.wantAgo {
				t.Errorf("idle_ago = %q, wantAgo=%v", it.IdleAgo, c.wantAgo)
			}
		})
	}
}

// An all-resting fleet ages on the FLEET's newest activity, not on the hosting
// session's last turn. That substitution is the whole fix.
func TestFleetRowAgesOnFleetActivityNotTheSession(t *testing.T) {
	now := time.UnixMilli(1_000_000_000)
	sum := fleetSummary(0, 0, 0, 1, 0, "", "")
	sum.NewestActivity = now.Add(-9 * time.Minute).UnixMilli()

	it := fleetItem(sum, ScopeWhole, "agents", 16, now)
	if it.IdleAgo != "9m" {
		t.Errorf("idle_ago = %q, want 9m (the fleet's age)", it.IdleAgo)
	}
	if it.IdleLevel != state.DecayLevel(9*time.Minute) {
		t.Errorf("idle_level = %d, want the level for 9m", it.IdleLevel)
	}
	// And with no activity known at all it must read dimmest rather than bright.
	sum.NewestActivity = 0
	if dim := fleetItem(sum, ScopeWhole, "agents", 16, now); dim.IdleLevel != state.DecayLevels-1 {
		t.Errorf("unknown activity should be dimmest, got level %d", dim.IdleLevel)
	}
}

// The row must stay VALID against the tile contract: rows are discriminated by
// `state` and `kind` is a closed enum, so a fleet row has to remain a session row
// with a state. A `kind: "fleet"` row would be schema-invalid and might not paint.
func TestFleetRowStaysContractValid(t *testing.T) {
	now := time.UnixMilli(1_000_000_000)
	it := fleetItem(fleetSummary(1, 0, 0, 0, 0, "a", "approve"), ScopeWhole, "agents", 16, now)
	if it.Kind != KindSession {
		t.Errorf("kind = %q, want %q — the contract's enum has no fleet kind", it.Kind, KindSession)
	}
	if it.State == "" {
		t.Error("a fleet row must carry a state; that is how the contract identifies a session row")
	}
	// And the extra data must serialize under a single additive key.
	raw, err := json.Marshal(it)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := decoded["fleet"]; !ok {
		t.Errorf("expected an additive `fleet` key: %s", raw)
	}
	if decoded["kind"] != KindSession {
		t.Errorf("serialized kind = %v", decoded["kind"])
	}
}

func TestFleetViewFromDetectsConsolesAndSummarizes(t *testing.T) {
	children := map[string]fleet.Child{
		"bg-1": {SessionID: "bg-1", Name: "project-manager-7", Demand: fleet.DemandHuman,
			Needs: "approve", OwnerSessionID: "owner"},
		"bg-2": {SessionID: "bg-2", Name: "project-manager-8", Demand: fleet.DemandWorking,
			OwnerSessionID: "owner"},
	}
	rollups := fleet.Rollups(children)
	fv := FleetViewFrom(children, rollups, map[int64]bool{16: true}, nil)
	if !fv.Consoles[16] {
		t.Error("the sticky console set should carry through")
	}
	if fv.Consoles[18] {
		t.Error("a window not in the sticky set must not be a console")
	}
	if fv.Whole.Total != 2 || fv.Whole.Awaiting != 1 {
		t.Errorf("whole fleet = %+v, want 2 agents / 1 awaiting", fv.Whole)
	}
	if got := fv.ByOwner["owner"]; got.Total != 2 {
		t.Errorf("owner summary = %+v", got)
	}
}

// A console row already lists EVERY agent, so an owned fleet row on the same desktop
// is a subset of it. Emitting both spent the tile's two rows on the same agents
// counted twice and pushed the conversations off the tile entirely.
func TestConsoleRowSuppressesRedundantOwnedFleetRows(t *testing.T) {
	now := time.UnixMilli(1_000_000_000)
	w := ws(4, "HDMI-A-1", 40, false)
	wins := []niri.Window{
		win(15, 40, "kitty", "a conversation"),                   // an owner's window
		win(16, 40, "kitty", "3 awaiting input · claude agents"), // the console
	}
	owner := claudeSession(15, "/home/e/repo", "idle", now.Add(-time.Hour).UnixMilli())
	console := claudeSession(16, "/home/e/other", "idle", now.Add(-22*time.Hour).UnixMilli())
	fv := FleetView{
		ByOwner:  map[string]fleet.Summary{owner.SessionID: fleetSummary(1, 6, 0, 0, 0, "w1", "approve")},
		Whole:    fleetSummary(3, 8, 0, 1, 1, "w1", "approve"),
		Consoles: map[int64]bool{16: true},
	}

	p := PayloadFor(w, wins, []db.Session{owner, console}, fv, now)
	var whole, owned int
	for _, it := range p.Items {
		if it.Fleet == nil {
			continue
		}
		switch it.Fleet.Scope {
		case ScopeWhole:
			whole++
		case ScopeOwned:
			owned++
		}
	}
	if whole != 1 {
		t.Errorf("want exactly one whole-scope row, got %d: %+v", whole, p.Items)
	}
	if owned != 0 {
		t.Errorf("an owned row is redundant beside a console row, got %d", owned)
	}
	// And the freed row goes back to the conversation, which is the point.
	hasSession := false
	for _, it := range p.Items {
		if it.Fleet == nil && it.Kind == KindSession {
			hasSession = true
		}
	}
	if !hasSession {
		t.Errorf("the conversation row should survive once the redundant fleet row is gone: %+v", p.Items)
	}
}

// Without a console on the desktop, an owner still gets its own fleet row.
func TestOwnedFleetRowStillAppearsWithoutAConsole(t *testing.T) {
	now := time.UnixMilli(1_000_000_000)
	w := ws(4, "HDMI-A-1", 40, false)
	wins := []niri.Window{win(15, 40, "kitty", "a conversation")}
	owner := claudeSession(15, "/home/e/repo", "idle", now.Add(-time.Hour).UnixMilli())
	fv := FleetView{
		ByOwner: map[string]fleet.Summary{owner.SessionID: fleetSummary(0, 2, 0, 0, 0, "", "")},
		Whole:   fleetSummary(0, 9, 0, 0, 0, "", ""), // a fleet exists, but no console here
	}
	p := PayloadFor(w, wins, []db.Session{owner}, fv, now)
	if len(p.Items) != 2 || p.Items[0].Fleet == nil && p.Items[1].Fleet == nil {
		t.Fatalf("want a session row plus an owned fleet row: %+v", p.Items)
	}
}

// A desktop can hold several agent-view windows. Their whole-scope summaries are
// machine-wide and therefore identical, so one row per window fills the tile's
// two-row budget with the same dashboard twice — seen live, two byte-identical
// "agents ⚠1 1/1" rows — and pushes everything else off.
func TestMultipleConsoleWindowsCollapseToOneRow(t *testing.T) {
	now := time.UnixMilli(1_000_000_000)
	w := ws(3, "HDMI-A-1", 30, false)
	wins := []niri.Window{
		win(41, 30, "kitty", "3 awaiting input · claude agents"),
		win(16, 30, "kitty", "2 awaiting input · claude agents"),
		win(55, 30, "firefox", "a browser"),
	}
	a := claudeSession(41, "/home/e/one", "idle", now.Add(-22*time.Hour).UnixMilli())
	b := claudeSession(16, "/home/e/two", "idle", now.Add(-22*time.Hour).UnixMilli())
	fv := FleetView{
		Whole:    fleetSummary(1, 0, 0, 0, 0, "worker-1", "approve rm"),
		Consoles: map[int64]bool{41: true, 16: true},
	}

	p := PayloadFor(w, wins, []db.Session{a, b}, fv, now)
	fleetRows := 0
	for _, it := range p.Items {
		if it.Fleet != nil {
			fleetRows++
			// Deterministically the lowest window id, so the tile cannot flip
			// between the two consoles from one rebuild to the next.
			if it.WinID != 16 {
				t.Errorf("fleet row win_id = %d, want the lowest console id 16", it.WinID)
			}
		}
	}
	if fleetRows != 1 {
		t.Fatalf("got %d fleet rows, want exactly 1: %+v", fleetRows, p.Items)
	}
	// The losing console must NOT reappear as a plain session row — that would put
	// the "idle 22h" reading straight back on the tile.
	for _, it := range p.Items {
		if it.Fleet == nil && it.Kind == KindSession {
			t.Errorf("a losing console fell back to a session row: %+v", it)
		}
	}
	// And the freed row goes to the browser that was being pushed off.
	if len(p.Items) != 2 || p.Items[1].Kind != KindApp {
		t.Errorf("want the fleet row plus the app row, got %+v", p.Items)
	}
}

// THE FOSSIL CONSOLE. Measured 2026-08-25: windows 12 and 34 were still titled
// "claude agents" — sticky marker intact, kitty alive, `claude agents` process alive
// — with their host sessions silent for 188h and 187h. They failed to claim a row
// only because the whole-fleet set happened to be empty; the moment any background
// agent exists, an eight-day fossil takes the desktop's console row. It must not.
func TestFossilConsoleWindowIsNotBelieved(t *testing.T) {
	now := time.UnixMilli(1_787_660_000_000)
	w := ws(1, "HDMI-A-1", 11, false)
	wins := []niri.Window{win(12, 11, "kitty", "0 awaiting input · claude agents")}
	fossil := claudeSession(12, "/home/e/taf/project-manager", "idle", now.Add(-188*time.Hour).UnixMilli())
	fv := FleetView{
		Whole:    fleetSummary(1, 2, 0, 0, 0, "worker-1", "approve rm"),
		Consoles: map[int64]bool{12: true},
	}

	p := PayloadFor(w, wins, []db.Session{fossil}, fv, now)
	if len(p.Items) != 1 {
		t.Fatalf("want a single row, got %d: %+v", len(p.Items), p.Items)
	}
	if p.Items[0].Fleet != nil {
		t.Errorf("an eight-day-silent console must not claim a fleet row: %+v", p.Items[0])
	}
	// It is an ordinary resting conversation now, and saying so is the honest read.
	if p.Items[0].Kind != KindSession || p.Items[0].IdleAgo == "" {
		t.Errorf("want a plain session row carrying its age: %+v", p.Items[0])
	}
}

// A console marker whose window hosts NO tracked session at all is the other fossil
// shape: the claude exited and kitty kept the title. Believing it costs more than a
// wrong row — hasConsole suppresses every owned fleet row on the desktop, so a
// neighbour's live fleet would disappear behind a dashboard nobody is running.
func TestConsoleMarkerWithNoLiveSessionDoesNotSuppressARealFleetRow(t *testing.T) {
	now := time.UnixMilli(1_787_660_000_000)
	w := ws(1, "HDMI-A-1", 11, false)
	wins := []niri.Window{
		win(12, 11, "kitty", "claude agents"), // fossil: titled, marked, no session
		win(15, 11, "kitty", "a conversation"),
	}
	owner := claudeSession(15, "/home/e/taf/project-manager", "idle", now.Add(-time.Hour).UnixMilli())
	fv := FleetView{
		ByOwner:  map[string]fleet.Summary{owner.SessionID: fleetSummary(0, 2, 0, 0, 0, "", "")},
		Whole:    fleetSummary(0, 2, 0, 0, 0, "", ""),
		Consoles: map[int64]bool{12: true},
	}

	p := PayloadFor(w, wins, []db.Session{owner}, fv, now)
	var owned, whole int
	for _, it := range p.Items {
		if it.Fleet == nil {
			continue
		}
		switch it.Fleet.Scope {
		case ScopeOwned:
			owned++
		case ScopeWhole:
			whole++
		}
	}
	if whole != 0 {
		t.Errorf("a session-less console marker must not produce a whole-scope row: %+v", p.Items)
	}
	if owned != 1 {
		t.Errorf("the owner's real fleet row should survive, got %d: %+v", owned, p.Items)
	}
}

// The no-regression half, and the stickiness rationale intact: a console whose host
// session is live keeps its row even once the view has retitled itself to something
// that is not a console title at all (the daemon's sticky marker is what carries it).
func TestLiveConsoleKeepsItsRowAcrossARetitle(t *testing.T) {
	now := time.UnixMilli(1_787_660_000_000)
	w := ws(1, "HDMI-A-1", 11, false)
	// The view has scrolled onto an agent's name — no longer a console title.
	wins := []niri.Window{win(16, 11, "kitty", "✳ project-manager-5")}
	console := claudeSession(16, "/home/e/taf/project-manager", "idle", now.Add(-22*time.Hour).UnixMilli())
	fv := FleetView{
		Whole:    fleetSummary(1, 2, 0, 0, 0, "worker-1", "approve rm"),
		Consoles: map[int64]bool{16: true},
	}

	p := PayloadFor(w, wins, []db.Session{console}, fv, now)
	if len(p.Items) != 1 || p.Items[0].Fleet == nil {
		t.Fatalf("a live console must keep its fleet row across a retitle: %+v", p.Items)
	}
	if p.Items[0].Fleet.Scope != ScopeWhole || p.Items[0].WinID != 16 {
		t.Errorf("console row wrong: %+v", p.Items[0])
	}
}
