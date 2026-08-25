package tile

import (
	"database/sql"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mrzor/claude-status/internal/db"
	"github.com/mrzor/claude-status/internal/fleet"
	"github.com/mrzor/claude-status/internal/niri"
	"github.com/mrzor/claude-status/internal/state"
)

// herdSession is one pane of a herd as our DB holds it: an ORDINARY interactive
// session whose only marker is the herdr_workspace_id column the hook captured from
// the pane's environment.
func herdSession(id string, winID int, workspace, pane, cwd, st string, lastTalk int64) db.Session {
	return db.Session{
		SessionID:        id,
		WindowID:         sql.NullInt64{Int64: int64(winID), Valid: true},
		Cwd:              sql.NullString{String: cwd, Valid: true},
		State:            st,
		LastTalkTS:       sql.NullInt64{Int64: lastTalk, Valid: true},
		HerdrWorkspaceID: sql.NullString{String: workspace, Valid: workspace != ""},
		HerdrPaneID:      sql.NullString{String: pane, Valid: pane != ""},
	}
}

// herd builds a group with one member per demand given, in the arbitrary order a
// provider hands them over (Summarize is what imposes worst-first).
func herd(key, label string, freshness time.Time, members ...fleet.Member) fleet.Group {
	return fleet.Group{
		Key:       fleet.GroupKey{Provider: fleet.ProviderHerdr, Key: key},
		Label:     label,
		Members:   members,
		Freshness: freshness,
	}
}

func mem(name string, d fleet.Demand, activity time.Time) fleet.Member {
	return fleet.Member{Name: name, Ref: "w1:p" + name, Demand: d, Activity: activity}
}

// THE USER-FACING FIX. Four panes of one herd share one niri window, one title and
// (before this) contributed four indistinguishable session rows that the two-row cap
// resolved by winID and then lexicographically by session uuid. Now: ONE row.
func TestHerdrWindowEmitsOneGroupRowNotNSessionRows(t *testing.T) {
	now := time.UnixMilli(1_000_000_000)
	fresh := now.Add(-3 * time.Second)

	w := ws(3, "HDMI-A-1", 30, false)
	// herdr's own window title, fanned out to every pane behind it.
	wins := []niri.Window{win(33, 30, "kitty", "ML-PF603D2Z: project-manager")}
	sessions := []db.Session{
		herdSession("aaa", 33, "w1", "w1:pCD", "/home/e/pm", "idle", now.Add(-45*time.Minute).UnixMilli()),
		herdSession("bbb", 33, "w1", "w1:pCH", "/home/e/pm/.claude/worktrees/pm-6", "working", fresh.UnixMilli()),
		herdSession("ccc", 33, "w1", "w1:pCJ", "/home/e/pm/.claude/worktrees/pm-7", "idle", now.Add(-2*time.Hour).UnixMilli()),
		herdSession("ddd", 33, "w1", "w1:pCK", "/home/e/pm", "idle", now.Add(-3*time.Hour).UnixMilli()),
	}
	fv := FleetView{Herds: map[string]fleet.Group{
		"w1": herd("w1", "project-manager", time.Time{},
			mem("keeper", fleet.DemandHuman, fresh),
			mem("helpdesk", fleet.DemandIdle, now.Add(-3*time.Hour)),
			mem("integrator", fleet.DemandWorking, fresh),
			mem("scout", fleet.DemandIdle, now.Add(-2*time.Hour)),
		),
	}}

	p := PayloadFor(w, wins, sessions, fv, now)
	if len(p.Items) != 1 {
		t.Fatalf("want ONE herd row, got %d: %+v", len(p.Items), p.Items)
	}
	it := p.Items[0]
	if it.Fleet == nil {
		t.Fatalf("the row is not a fleet row: %+v", it)
	}
	if it.Fleet.Scope != ScopeHerd {
		t.Errorf("scope = %q, want %q", it.Fleet.Scope, ScopeHerd)
	}
	// The label slot carries the COUNT (it always fits at 179px); the repository
	// moves to the wrapped title, where a clipped "project-…" would have identified
	// nothing.
	// The project, abbreviated, AND the count — the full spelling goes to the title.
	if it.Folder != "PM ×4" {
		t.Errorf("label = %q, want the project initials and the count", it.Folder)
	}
	if !strings.Contains(it.Title, "project-manager") {
		t.Errorf("title = %q, want it to name the repository", it.Title)
	}
	if it.Fleet.Total != 4 || it.Fleet.Live != 4 {
		t.Errorf("counts = %d/%d, want 4/4 agents", it.Fleet.Live, it.Fleet.Total)
	}
	if it.Fleet.Awaiting != 1 {
		t.Errorf("awaiting = %d, want the one blocked agent (this is the ⚠N)", it.Fleet.Awaiting)
	}
	// Beads WORST FIRST — the user's choice over stable-by-agent.
	want := []string{"awaits-human", "working", "idle", "idle"}
	if !reflect.DeepEqual(it.Fleet.Demands, want) {
		t.Errorf("bead strip = %v, want %v", it.Fleet.Demands, want)
	}
	if it.Fleet.Neediest != "keeper" {
		t.Errorf("neediest = %q, want the blocked agent named", it.Fleet.Neediest)
	}
	// An agent awaiting a human makes the whole tile pulse.
	if it.State != string(state.Prompt) {
		t.Errorf("state = %q, want %q so the tile pulses", it.State, state.Prompt)
	}
	// NO INVENTED ASK. herdr has no equivalent of a background job's `needs`.
	if it.Fleet.Needs != "" {
		t.Errorf("needs = %q; herdr has no ask to quote and none may be invented", it.Fleet.Needs)
	}
	// The row still NAMES the blocked agent even with no ask to quote.
	if !strings.Contains(it.Title, "keeper") {
		t.Errorf("title %q does not name the blocked agent", it.Title)
	}
	if it.WinID != 33 {
		t.Errorf("win_id = %d, want the herd's window so a click lands on it", it.WinID)
	}
}

// The row must be IDENTICAL across two renders while ticks recycle underneath. This
// is the acceptance criterion: nothing about the row may be derived from a member.
func TestHerdRowIsStableWhileTicksRecycle(t *testing.T) {
	now := time.UnixMilli(1_000_000_000)
	fresh := now.Add(-2 * time.Second)
	w := ws(3, "HDMI-A-1", 30, false)
	wins := []niri.Window{win(33, 30, "kitty", "ML-PF603D2Z: project-manager")}
	g := herd("w1", "project-manager", fresh,
		mem("keeper", fleet.DemandIdle, fresh),
		mem("tick", fleet.DemandWorking, fresh),
	)
	fv := FleetView{Herds: map[string]fleet.Group{"w1": g}}

	render := func(sessions []db.Session) string {
		p := PayloadFor(w, wins, sessions, fv, now)
		b, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	// Two ticks: the pane ids churn (w1:pCD -> w1:pCH), one session id is replaced
	// entirely, and the surviving one reports a different cwd basename.
	first := render([]db.Session{
		herdSession("aaa", 33, "w1", "w1:pCD", "/home/e/pm", "idle", fresh.UnixMilli()),
		herdSession("bbb", 33, "w1", "w1:pT1", "/home/e/pm/.claude/worktrees/pm-6", "working", fresh.UnixMilli()),
	})
	second := render([]db.Session{
		herdSession("aaa", 33, "w1", "w1:pCH", "/home/e/pm", "idle", fresh.UnixMilli()),
		herdSession("zzz", 33, "w1", "w1:pT2", "/home/e/pm/.claude/worktrees/pm-9", "working", fresh.UnixMilli()),
	})
	if first != second {
		t.Errorf("the herd row changed while its members churned:\n  %s\n  %s", first, second)
	}
}

// Aged on the GROUP's freshness, not on whichever session the cap happened to pick.
// The live symptom was "idle 45m" for a herd with an agent actively working in it.
func TestIdleHerdAgesOnTheGroupNotOnASession(t *testing.T) {
	now := time.UnixMilli(1_000_000_000)
	w := ws(3, "HDMI-A-1", 30, false)
	wins := []niri.Window{win(33, 30, "kitty", "herdr")}
	// Every pane's own last_talk is hours old...
	sessions := []db.Session{
		herdSession("aaa", 33, "w1", "w1:pA", "/home/e/pm", "idle", now.Add(-3*time.Hour).UnixMilli()),
		herdSession("bbb", 33, "w1", "w1:pB", "/home/e/pm", "idle", now.Add(-45*time.Minute).UnixMilli()),
	}
	// ...while the group's freshness says something moved 10 seconds ago.
	fv := FleetView{Herds: map[string]fleet.Group{
		"w1": herd("w1", "project-manager", now.Add(-10*time.Second),
			mem("a", fleet.DemandIdle, now.Add(-3*time.Hour)),
			mem("b", fleet.DemandIdle, now.Add(-45*time.Minute)),
		),
	}}

	it := PayloadFor(w, wins, sessions, fv, now).Items[0]
	if it.State != string(state.Idle) {
		t.Fatalf("state = %q, want idle", it.State)
	}
	if it.IdleLevel != state.DecayLevel(10*time.Second) {
		t.Errorf("idle_level = %d, want the level for 10s (%d) — a 45m-old pane must not age the herd",
			it.IdleLevel, state.DecayLevel(10*time.Second))
	}
	if it.IdleAgo != fmtAgo(10*time.Second) || it.IdleAgo == fmtAgo(45*time.Minute) {
		t.Errorf("idle_ago = %q, want the group's %q and not a pane's %q",
			it.IdleAgo, fmtAgo(10*time.Second), fmtAgo(45*time.Minute))
	}
}

// One herdr WINDOW holds several herdr WORKSPACES — w1, w9 and wA were all live
// behind niri window 33. Each gets its own row, distinguished by its own label, and
// the two-row cap keeps them worst-first (deterministically, not by session uuid).
// Herds sharing ONE WINDOW merge into ONE row. This reverses the earlier
// per-workspace contract, and the evidence is a render at the real 179px width: two
// bound herds produced two rows whose labels BOTH clipped to "project-…", so they
// read as the same thing twice, while the 2-row cap was about to drop three more of
// the five herds bound to that window. Most herdr workspaces here hold exactly one
// agent, so a row per workspace spends the whole tile on single-bead rows.
func TestHerdsSharingOneWindowMergeIntoOneRow(t *testing.T) {
	now := time.UnixMilli(1_000_000_000)
	fresh := now.Add(-time.Second)
	w := ws(3, "HDMI-A-1", 30, false)
	wins := []niri.Window{win(33, 30, "kitty", "ML-PF603D2Z: project-manager")}
	sessions := []db.Session{
		herdSession("aaa", 33, "w1", "w1:pA", "/home/e/pm", "idle", fresh.UnixMilli()),
		herdSession("bbb", 33, "w9", "w9:pA", "/home/e/wt/6", "idle", fresh.UnixMilli()),
		herdSession("ccc", 33, "wA", "wA:pA", "/home/e/wt/7", "idle", fresh.UnixMilli()),
	}
	fv := FleetView{Herds: map[string]fleet.Group{
		"w1": herd("w1", "project-manager", fresh, mem("keeper", fleet.DemandIdle, fresh)),
		"w9": herd("w9", "project-manager-6", fresh, mem("helpdesk", fleet.DemandHuman, fresh)),
		"wA": herd("wA", "project-manager-7", fresh, mem("integrator", fleet.DemandWorking, fresh)),
	}}

	p := PayloadFor(w, wins, sessions, fv, now)
	if len(p.Items) != 1 {
		t.Fatalf("want ONE merged row, got %d: %+v", len(p.Items), p.Items)
	}
	it := p.Items[0]
	if it.Fleet == nil {
		t.Fatal("merged row carries no fleet")
	}
	// Every agent is represented — nothing is dropped by the cap any more.
	if it.Fleet.Total != 3 || len(it.Fleet.Demands) != 3 {
		t.Errorf("want 3 agents and 3 beads, got total=%d beads=%v", it.Fleet.Total, it.Fleet.Demands)
	}
	// Beads re-sorted worst-first ACROSS the herds, not concatenated per herd.
	if it.Fleet.Demands[0] != "awaits-human" || it.Fleet.Demands[2] != "idle" {
		t.Errorf("beads not globally worst-first: %v", it.Fleet.Demands)
	}
	// The row is named by what the herds share — the repository — not by a
	// truncation of one of them.
	if it.Folder != "PM ×3" {
		t.Errorf("label = %q, want the shared project and the count", it.Folder)
	}
	// The shared prefix — the repository — is what the title carries.
	if !strings.Contains(it.Title, "project-manager") {
		t.Errorf("title = %q, want the shared prefix", it.Title)
	}
	// The worst demand still drives the row's state and names its agent.
	if it.State != string(state.Prompt) || it.Fleet.Neediest != "helpdesk" {
		t.Errorf("state=%q neediest=%q, want prompt/helpdesk", it.State, it.Fleet.Neediest)
	}
	// Stable across rebuilds: the herds come out of a map.
	for range 20 {
		q := PayloadFor(w, wins, sessions, fv, now)
		if len(q.Items) != 1 || q.Items[0].Folder != it.Folder ||
			!reflect.DeepEqual(q.Items[0].Fleet.Demands, it.Fleet.Demands) {
			t.Fatalf("merged row flipped between rebuilds: %+v vs %+v", q.Items, p.Items)
		}
	}
}

// Herds on DIFFERENT windows stay separate rows, and the cap orders them worst-first
// with a deterministic tie-break — the property the old per-window test protected.
func TestHerdsOnDifferentWindowsStaySeparateAndOrderedWorstFirst(t *testing.T) {
	now := time.UnixMilli(1_000_000_000)
	fresh := now.Add(-time.Second)
	w := ws(3, "HDMI-A-1", 30, false)
	wins := []niri.Window{
		win(33, 30, "kitty", "herdr one"),
		win(44, 30, "kitty", "herdr two"),
		win(55, 30, "kitty", "herdr three"),
	}
	sessions := []db.Session{
		herdSession("aaa", 33, "w1", "w1:pA", "/home/e/a", "idle", fresh.UnixMilli()),
		herdSession("bbb", 44, "w9", "w9:pA", "/home/e/b", "idle", fresh.UnixMilli()),
		herdSession("ccc", 55, "wA", "wA:pA", "/home/e/c", "idle", fresh.UnixMilli()),
	}
	fv := FleetView{Herds: map[string]fleet.Group{
		"w1": herd("w1", "alpha", fresh, mem("a", fleet.DemandIdle, fresh)),
		"w9": herd("w9", "bravo", fresh, mem("b", fleet.DemandHuman, fresh)),
		"wA": herd("wA", "charlie", fresh, mem("c", fleet.DemandWorking, fresh)),
	}}

	p := PayloadFor(w, wins, sessions, fv, now)
	if len(p.Items) != maxItemsPerTile {
		t.Fatalf("want %d rows, got %d: %+v", maxItemsPerTile, len(p.Items), p.Items)
	}
	// Every single-agent herd labels "1 agent" now, so the repository is read off the
	// title instead.
	// Contains, not HasPrefix: an awaiting herd leads its title with the ask, so the
	// repository is not always first — see herdTitle.
	if !strings.Contains(p.Items[0].Title, "bravo") || !strings.Contains(p.Items[1].Title, "charlie") {
		t.Errorf("rows = [%q %q], want the awaiting herd then the working one",
			p.Items[0].Title, p.Items[1].Title)
	}
	for range 20 {
		q := PayloadFor(w, wins, sessions, fv, now)
		if q.Items[0].Title != p.Items[0].Title || q.Items[1].Title != p.Items[1].Title {
			t.Fatalf("row choice flipped between rebuilds: %v vs %v", q.Items, p.Items)
		}
	}
}

// GRACEFUL DEGRADATION, both ways round. A herdr window whose rows have no workspace
// id yet (the column is only written from the pane environment once a session has
// restarted under the integration) renders exactly as it does today; so does one
// whose workspace has no group, which is what happens with no daemon up.
func TestHerdrWindowWithoutTheColumnOrTheGroupRendersAsBefore(t *testing.T) {
	now := time.UnixMilli(1_000_000_000)
	w := ws(3, "HDMI-A-1", 30, false)
	wins := []niri.Window{win(33, 30, "kitty", "ML-PF603D2Z: project-manager")}
	withHerd := FleetView{Herds: map[string]fleet.Group{
		"w1": herd("w1", "project-manager", now, mem("keeper", fleet.DemandIdle, now)),
	}}

	// (a) no workspace id on the rows at all.
	noColumn := []db.Session{
		herdSession("aaa", 33, "", "", "/home/e/pm", "working", now.UnixMilli()),
		herdSession("bbb", 33, "", "", "/home/e/pm", "idle", now.UnixMilli()),
	}
	// (b) the column is there but no group answers for it.
	noGroup := []db.Session{
		herdSession("aaa", 33, "w7", "w7:pA", "/home/e/pm", "working", now.UnixMilli()),
		herdSession("bbb", 33, "w7", "w7:pB", "/home/e/pm", "idle", now.UnixMilli()),
	}
	// (c) the group exists but is empty — a row saying "0 agents" is worse than the
	// session row it would replace.
	emptyGroup := FleetView{Herds: map[string]fleet.Group{
		"w1": herd("w1", "project-manager", time.Time{}),
	}}
	withColumn := []db.Session{
		herdSession("aaa", 33, "w1", "w1:pA", "/home/e/pm", "working", now.UnixMilli()),
		herdSession("bbb", 33, "w1", "w1:pB", "/home/e/pm", "idle", now.UnixMilli()),
	}

	for _, tc := range []struct {
		name     string
		sessions []db.Session
		fv       FleetView
	}{
		{"no herdr column on the rows", noColumn, withHerd},
		{"column present, no group for it", noGroup, withHerd},
		{"group present but empty", withColumn, emptyGroup},
		{"no herd data at all", withColumn, FleetView{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := PayloadFor(w, wins, tc.sessions, tc.fv, now)
			for _, it := range p.Items {
				if it.Fleet != nil {
					t.Fatalf("a herd row appeared where it must not: %+v", it)
				}
			}
			if len(p.Items) != 2 || p.Items[0].State != "working" {
				t.Errorf("payload is not the pre-herd shape: %+v", p.Items)
			}
		})
	}
}

// A herd row frees the desktop's other row for whatever else is on it — the live
// case had a bare kitty and an unrelated claude both pushed off by the session-row
// lottery.
func TestHerdRowLeavesRoomForTheRestOfTheDesktop(t *testing.T) {
	now := time.UnixMilli(1_000_000_000)
	fresh := now.Add(-time.Second)
	w := ws(3, "HDMI-A-1", 30, false)
	wins := []niri.Window{
		win(33, 30, "kitty", "ML-PF603D2Z: project-manager"),
		win(40, 30, "firefox", "a browser"),
	}
	sessions := []db.Session{
		herdSession("aaa", 33, "w1", "w1:pA", "/home/e/pm", "idle", fresh.UnixMilli()),
		herdSession("bbb", 33, "w1", "w1:pB", "/home/e/pm", "idle", fresh.UnixMilli()),
		herdSession("ccc", 33, "w1", "w1:pC", "/home/e/pm", "idle", fresh.UnixMilli()),
	}
	fv := FleetView{Herds: map[string]fleet.Group{
		"w1": herd("w1", "project-manager", fresh,
			mem("a", fleet.DemandIdle, fresh), mem("b", fleet.DemandIdle, fresh),
			mem("c", fleet.DemandIdle, fresh)),
	}}

	p := PayloadFor(w, wins, sessions, fv, now)
	if len(p.Items) != 2 {
		t.Fatalf("want the herd row plus the browser, got %+v", p.Items)
	}
	if p.Items[0].Fleet == nil || p.Items[0].Fleet.Total != 3 {
		t.Errorf("first row should be the herd of three: %+v", p.Items[0])
	}
	if p.Items[1].Kind != KindApp || p.Items[1].App == "" {
		t.Errorf("the freed row should hold the ordinary window: %+v", p.Items[1])
	}
}

// A CONSOLE window still wins over the herd logic: it is running the agent view, not
// a herd, and its row is machine-wide. Nothing about the herd path may weaken the
// gate console.go just gained.
func TestConsoleWindowStillWinsOverAHerdRow(t *testing.T) {
	now := time.UnixMilli(1_000_000_000)
	w := ws(5, "HDMI-A-1", 11, false)
	wins := []niri.Window{win(16, 11, "kitty", "6 awaiting input · claude agents")}
	// The same session is both a console host and (implausibly) carries a herdr
	// column: the console reading must not be displaced.
	s := herdSession("aaa", 16, "w1", "w1:pA", "/home/e/pm", "idle", now.Add(-22*time.Hour).UnixMilli())
	fv := FleetView{
		Whole:    fleetSummary(4, 2, 0, 2, 3, "project-manager-7", "approve Bash: git mv …"),
		Consoles: map[int64]bool{16: true},
		Herds: map[string]fleet.Group{
			"w1": herd("w1", "project-manager", now, mem("keeper", fleet.DemandIdle, now)),
		},
	}
	p := PayloadFor(w, wins, []db.Session{s}, fv, now)
	if len(p.Items) != 1 || p.Items[0].Fleet == nil {
		t.Fatalf("want one fleet row, got %+v", p.Items)
	}
	if p.Items[0].Fleet.Scope != ScopeWhole {
		t.Errorf("scope = %q, want the console's whole-machine scope", p.Items[0].Fleet.Scope)
	}
}

// A herd pane that also owns background agents keeps that row: its own session row is
// replaced by the herd, but an ask from a fleet it spawned must not vanish with it.
func TestHerdPaneKeepsItsOwnedFleetRow(t *testing.T) {
	now := time.UnixMilli(1_000_000_000)
	w := ws(3, "HDMI-A-1", 30, false)
	wins := []niri.Window{win(33, 30, "kitty", "herdr")}
	sessions := []db.Session{
		herdSession("aaa", 33, "w1", "w1:pA", "/home/e/pm", "idle", now.UnixMilli()),
	}
	fv := FleetView{
		Herds: map[string]fleet.Group{
			"w1": herd("w1", "project-manager", now, mem("keeper", fleet.DemandIdle, now)),
		},
		ByOwner: map[string]fleet.Summary{
			"aaa": fleetSummary(1, 0, 0, 0, 0, "pm-3", "approve Bash: rm …"),
		},
	}
	p := PayloadFor(w, wins, sessions, fv, now)
	scopes := map[string]bool{}
	for _, it := range p.Items {
		if it.Fleet != nil {
			scopes[it.Fleet.Scope] = true
		}
	}
	if !scopes[ScopeHerd] || !scopes[ScopeOwned] {
		t.Errorf("want both a herd row and its owned fleet row, got %+v", p.Items)
	}
}

// herdr's `unknown` reaches the payload as its own bead and never as "done".
func TestUnknownAgentsAreTheirOwnBead(t *testing.T) {
	now := time.UnixMilli(1_000_000_000)
	w := ws(3, "HDMI-A-1", 30, false)
	wins := []niri.Window{win(33, 30, "kitty", "herdr")}
	sessions := []db.Session{herdSession("aaa", 33, "w1", "w1:pA", "/home/e/pm", "idle", now.UnixMilli())}
	fv := FleetView{Herds: map[string]fleet.Group{
		"w1": herd("w1", "pm", now,
			mem("a", fleet.DemandUnknown, now), mem("b", fleet.DemandIdle, now)),
	}}
	it := PayloadFor(w, wins, sessions, fv, now).Items[0]
	if it.Fleet.Unknown != 1 || it.Fleet.Done != 0 {
		t.Errorf("fleet = %+v, want one unknown and nothing done", it.Fleet)
	}
	if !reflect.DeepEqual(it.Fleet.Demands, []string{"idle", "unknown"}) {
		t.Errorf("beads = %v, want idle then unknown", it.Fleet.Demands)
	}
	if it.Fleet.Live != 2 {
		t.Errorf("live = %d, want 2: an unclassifiable agent is not a finished one", it.Fleet.Live)
	}
}

// Two herds of EQUAL urgency behind one window: the cap has to choose, and the choice
// must not depend on a session id. Session ids churn at ~30 an hour behind one herdr
// window, so tie-breaking on one puts the row lottery straight back — the two rows
// would swap the moment a tick recycled. The tie-break is the GROUP KEY.
// The row must not change when the sessions underneath it recycle. Same intent as
// before the merge landed — only now the herds of one window are one row, so the
// assertion is that the MERGED row is identical across two ticks whose session ids
// are all different and arrive in a different order, which is what a recycle does.
func TestMergedHerdRowIsIdenticalAcrossSessionRecycles(t *testing.T) {
	now := time.UnixMilli(1_000_000_000)
	fresh := now.Add(-time.Second)
	w := ws(3, "HDMI-A-1", 30, false)
	wins := []niri.Window{win(33, 30, "kitty", "herdr")}
	fv := FleetView{Herds: map[string]fleet.Group{
		"w1": herd("w1", "alpha", fresh, mem("a", fleet.DemandIdle, fresh)),
		"w9": herd("w9", "bravo", fresh, mem("b", fleet.DemandIdle, fresh)),
		"wA": herd("wA", "charlie", fresh, mem("c", fleet.DemandIdle, fresh)),
	}}
	tick1 := []db.Session{
		herdSession("f00d", 33, "w1", "w1:pA", "/home/e/a", "idle", fresh.UnixMilli()),
		herdSession("0001", 33, "w9", "w9:pA", "/home/e/b", "idle", fresh.UnixMilli()),
		herdSession("zzzz", 33, "wA", "wA:pA", "/home/e/c", "idle", fresh.UnixMilli()),
	}
	tick2 := []db.Session{
		herdSession("aaaa", 33, "wA", "wA:pB", "/home/e/c", "idle", fresh.UnixMilli()),
		herdSession("ffff", 33, "w9", "w9:pB", "/home/e/b", "idle", fresh.UnixMilli()),
		herdSession("5555", 33, "w1", "w1:pB", "/home/e/a", "idle", fresh.UnixMilli()),
	}
	render := func(sessions []db.Session) []Item {
		return PayloadFor(w, wins, sessions, fv, now).Items
	}
	first, second := render(tick1), render(tick2)

	if len(first) != 1 {
		t.Fatalf("want one merged row, got %+v", first)
	}
	if !reflect.DeepEqual(first, second) {
		t.Errorf("the row changed when the sessions recycled:\n  %+v\n  %+v", first, second)
	}
	// Herds sharing no prefix get no project name at all rather than a misleading
	// stub — the count is all that is claimed.
	if first[0].Folder != "herd ×3" {
		t.Errorf("label = %q, want an unnamed herd and the count", first[0].Folder)
	}
	if first[0].Fleet == nil || first[0].Fleet.Total != 3 {
		t.Errorf("merged fleet wrong: %+v", first[0].Fleet)
	}
}

// A herd row must outlive its panes' sessions. herdr panes are driven by whatever
// runs them, and a colony tick lives ~81s: when the last session carrying the
// workspace id ends, a binding DERIVED from the rows loses the herd entirely and the
// row blinks out. Observed on the live bar after deploying, which is the only place
// it could show up — every earlier test supplied a row.
func TestHerdRowSurvivesEveryMemberSessionEnding(t *testing.T) {
	now := time.UnixMilli(1_000_000_000)
	w := ws(3, "HDMI-A-1", 30, false)
	wins := []niri.Window{win(33, 30, "kitty", "herdr")}
	group := fleet.Group{
		Key:   fleet.GroupKey{Provider: fleet.ProviderHerdr, Key: "w1"},
		Label: "project-manager",
		Members: []fleet.Member{
			{Name: "keeper", Demand: fleet.DemandWorking},
			{Name: "helpdesk", Demand: fleet.DemandIdle},
		},
	}
	fv := FleetView{
		Herds:       map[string]fleet.Group{"w1": group},
		HerdWindows: map[string]int64{"w1": 33},
	}

	// NOT ONE SESSION ROW: every pane's tick has ended and been reaped.
	p := PayloadFor(w, wins, nil, fv, now)
	if len(p.Items) != 1 {
		t.Fatalf("want the herd row, got %d items: %+v", len(p.Items), p.Items)
	}
	it := p.Items[0]
	if it.Fleet == nil || it.Fleet.Scope != ScopeHerd {
		t.Fatalf("want a herd row, got %+v", it)
	}
	if it.Fleet.Total != 2 || it.Folder != "PM ×2" {
		t.Errorf("herd row wrong: folder=%q fleet=%+v", it.Folder, it.Fleet)
	}
	if !strings.Contains(it.Title, "project-manager") {
		t.Errorf("title = %q, want it to name the repository", it.Title)
	}
	if it.WinID != 33 {
		t.Errorf("win_id = %d, want the bound window 33", it.WinID)
	}
	// And the window must not ALSO surface as a bare app row.
	for _, i := range p.Items {
		if i.Kind == KindApp {
			t.Errorf("the herd's window also rendered as an app row: %+v", p.Items)
		}
	}
}

// The binding is per-desktop: a herd bound to a window elsewhere must not appear here.
func TestHerdRowOnlyOnItsOwnDesktop(t *testing.T) {
	now := time.UnixMilli(1_000_000_000)
	fv := FleetView{
		Herds: map[string]fleet.Group{"w1": {
			Key:     fleet.GroupKey{Provider: fleet.ProviderHerdr, Key: "w1"},
			Label:   "project-manager",
			Members: []fleet.Member{{Name: "keeper", Demand: fleet.DemandWorking}},
		}},
		HerdWindows: map[string]int64{"w1": 33},
	}
	other := ws(7, "HDMI-A-1", 70, false)
	p := PayloadFor(other, []niri.Window{win(50, 70, "kitty", "elsewhere")}, nil, fv, now)
	for _, it := range p.Items {
		if it.Fleet != nil {
			t.Errorf("a herd bound to window 33 leaked onto another desktop: %+v", it)
		}
	}
}

// The label line is the one place a herd row can be read at a glance, and it has
// about ten characters at 179px. Both halves have to survive in them.
func TestHerdLabelKeepsBothTheProjectAndTheCount(t *testing.T) {
	for _, c := range []struct{ label, want string }{
		// A multi-word project goes to its initials: "project-…" was the stub every
		// workspace of one checkout shared, and it named none of them.
		{"project-manager", "PM ×6"},
		// Distinct projects stay distinct, which a head-clip would not manage.
		{"agentic-colony", "AC ×6"},
		// Short enough to spell out: no reason to abbreviate.
		{"gitlab", "gitlab ×6"},
		{"ezsre", "ezsre ×6"},
		// One long word has no initials worth having — one letter names nothing — so
		// it is clipped instead.
		{"monobuilder", "monobu… ×6"},
		// Deep worktree labels stop at four letters rather than growing unbounded.
		{"a-b-c-d-e-f", "ABCD ×6"},
		// Nothing shared: the count is all that is claimed.
		{"", "herd ×6"},
	} {
		if got := herdLabel(c.label, 6); got != c.want {
			t.Errorf("herdLabel(%q) = %q, want %q", c.label, got, c.want)
		}
	}
}
