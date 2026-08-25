package fleet

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mrzor/claude-status/internal/clauded"
	"github.com/mrzor/claude-status/internal/db"
	"github.com/mrzor/claude-status/internal/herdr"
)

// agent builds a roster entry. The zero AgentSession is the shape of a session that
// predates herdr's Claude integration — still live on this machine alongside the
// other one.
func agent(ws, pane, name string, st herdr.Status) herdr.Agent {
	return herdr.Agent{
		Kind: "claude", Status: st,
		WorkspaceID: ws, PaneID: pane, TabID: ws + ":t0", Name: name,
	}
}

func withSession(a herdr.Agent, sessionID string) herdr.Agent {
	a.HasSession = true
	a.Session = herdr.AgentSession{Source: "herdr:claude", Agent: "claude", Kind: "id", Value: sessionID}
	return a
}

// paneRow is one of our DB rows for a herdr-hosted session: the columns the hook
// captures from the pane environment.
func paneRow(sessionID, ws, pane string, lastTalk time.Time) db.Session {
	return db.Session{
		SessionID:        sessionID,
		HerdrWorkspaceID: sql.NullString{String: ws, Valid: true},
		HerdrPaneID:      sql.NullString{String: pane, Valid: true},
		LastTalkTS:       sql.NullInt64{Int64: lastTalk.UnixMilli(), Valid: true},
		LastSeenTS:       lastTalk.UnixMilli(),
	}
}

// stubProvider returns a provider whose roster is whatever the test hands it, and
// whose session.json is written under a temp dir (empty body -> no file at all).
func stubProvider(t *testing.T, sessionJSON string, rosters ...[]herdr.Agent) (*HerdrProvider, *int) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.json")
	if sessionJSON != "" {
		if err := os.WriteFile(path, []byte(sessionJSON), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p := NewHerdrProvider(path)
	calls := 0
	i := 0
	p.list = func(context.Context) ([]herdr.Agent, error) {
		calls++
		if i < len(rosters) {
			r := rosters[i]
			i++
			return r, nil
		}
		if len(rosters) == 0 {
			return nil, nil
		}
		return rosters[len(rosters)-1], nil
	}
	return p, &calls
}

func groupByKey(t *testing.T, gs []Group, key string) Group {
	t.Helper()
	for _, g := range gs {
		if g.Key.Key == key {
			return g
		}
	}
	t.Fatalf("no group keyed %q in %+v", key, gs)
	return Group{}
}

// The mapping herdr.Status documents but cannot implement (it would import this
// package and close a cycle). The two rows that matter are `done` and `unknown`.
func TestDemandOfHerdrStatus(t *testing.T) {
	cases := map[herdr.Status]Demand{
		herdr.StatusBlocked: DemandHuman,
		herdr.StatusWorking: DemandWorking,
		herdr.StatusIdle:    DemandIdle,
		// STICKY AND THEREFORE COMMON: only herdr's UI clears the flag and a CLI
		// read never marks anything seen, so agents park here. It is the same
		// at-rest state as idle and must stay in the live count.
		herdr.StatusDone: DemandIdle,
		// Present but unclassifiable. Emphatically not finished.
		herdr.StatusUnknown: DemandUnknown,
		// Anything herdr adds later is not guessed at.
		herdr.Status("compacting"): DemandUnknown,
	}
	for st, want := range cases {
		if got := DemandOfHerdrStatus(st); got != want {
			t.Errorf("DemandOfHerdrStatus(%q) = %v, want %v", st, got, want)
		}
	}
	if DemandOfHerdrStatus(herdr.StatusDone) == DemandDone {
		t.Error("herdr's `done` must not rank as finished — it is sticky and the agent is ready for a prompt")
	}
	if !(DemandIdle < DemandUnknown && DemandUnknown < DemandDone) {
		t.Errorf("unknown must sit between idle and done, got idle=%d unknown=%d done=%d",
			DemandIdle, DemandUnknown, DemandDone)
	}
}

// The live session.json shape: three workspaces on ONE repo, distinguished only by
// custom_name. Label-first would paint one herd as three identical rows.
const liveSessionJSON = `{
  "version": 3,
  "workspaces": [
    {"id": "w1", "custom_name": null, "identity_cwd": "/home/e/pm",
     "worktree_space": {"key": "/home/e/pm/.git", "label": "project-manager", "repo_root": "/home/e/pm", "checkout_path": "/home/e/pm", "is_linked_worktree": false},
     "tabs": [{"panes": {
        "7":   {"cwd": "/home/e/pm", "agent_name": "project-manager", "managed_agent_kind": "claude"},
        "8":   {"cwd": "/home/e/pm", "agent_name": "project-manager-keeper", "managed_agent_kind": "claude"},
        "9":   {"cwd": "/home/e/pm", "agent_name": null, "managed_agent_kind": null},
        "10":  {"cwd": "/home/e/pm", "agent_name": "project-manager-gone", "managed_agent_kind": "claude"}
     }}]},
    {"id": "w9", "custom_name": "project-manager-6", "identity_cwd": "/home/e/wt/6",
     "worktree_space": {"key": "/home/e/pm/.git", "label": "project-manager", "repo_root": "/home/e/pm", "checkout_path": "/home/e/wt/6", "is_linked_worktree": true},
     "tabs": [{"panes": {"1": {"cwd": "/home/e/wt/6", "agent_name": "helpdesk", "managed_agent_kind": "claude"}}}]}
  ]
}`

// GROUP IDENTITY IS (provider, workspace id) and the label comes from custom_name
// first — w1 and w9 share one repo label, so label-first collapses them.
func TestHerdrGroupsAreKeyedOnWorkspaceAndLabelledByCustomName(t *testing.T) {
	p, _ := stubProvider(t, liveSessionJSON, []herdr.Agent{
		agent("w1", "w1:pCD", "project-manager", herdr.StatusWorking),
		agent("w9", "w9:pA1", "helpdesk", herdr.StatusDone),
	})
	gs, err := p.Groups(context.Background(), Input{Now: base})
	if err != nil {
		t.Fatal(err)
	}
	if len(gs) != 2 {
		t.Fatalf("want one group per workspace, got %d: %+v", len(gs), gs)
	}
	for _, g := range gs {
		if g.Key.Provider != ProviderHerdr {
			t.Errorf("group %q claims provider %q", g.Key.Key, g.Key.Provider)
		}
	}
	if l := groupByKey(t, gs, "w1").Label; l != "project-manager" {
		t.Errorf("w1 label = %q, want the repo label (no custom name set)", l)
	}
	if l := groupByKey(t, gs, "w9").Label; l != "project-manager-6" {
		t.Errorf("w9 label = %q, want its custom_name — label-first would say %q for both",
			l, "project-manager")
	}
}

// COUNT AGENTS FROM THE ROSTER, NOT FROM session.json's PANES. Measured live: w1
// holds 4 pane records and 2 live agents. Counting panes inflates every bead strip.
func TestHerdrCountsLiveAgentsNotPaneRecords(t *testing.T) {
	p, _ := stubProvider(t, liveSessionJSON, []herdr.Agent{
		agent("w1", "w1:pCD", "project-manager", herdr.StatusIdle),
		agent("w1", "w1:pCJ", "project-manager-keeper", herdr.StatusDone),
	})
	gs, err := p.Groups(context.Background(), Input{Now: base})
	if err != nil {
		t.Fatal(err)
	}
	sum := groupByKey(t, gs, "w1").Summarize()
	if sum.Total != 2 {
		t.Fatalf("total = %d, want 2 live agents — session.json's 4 panes must not be counted", sum.Total)
	}
	if sum.Live != 2 {
		t.Errorf("live = %d, want 2: herdr's `done` is a flavour of idle, not a retirement", sum.Live)
	}
	if len(sum.Demands) != 2 {
		t.Errorf("bead strip has %d beads, want one per live agent", len(sum.Demands))
	}
}

// A pane BETWEEN TICKS hosts no Claude session at all and must still be an agent.
// This is why the member set comes from the provider and our rows only enrich it.
func TestHerdrPaneWithNoSessionIsStillAnAgent(t *testing.T) {
	p, _ := stubProvider(t, liveSessionJSON, []herdr.Agent{
		// Name cleared: herdr does that when a pane's occupant exits.
		agent("w1", "w1:pCD", "", herdr.StatusIdle),
		withSession(agent("w1", "w1:pCJ", "project-manager-keeper", herdr.StatusWorking), "sess-keeper"),
	})
	rows := []db.Session{paneRow("sess-keeper", "w1", "w1:pCJ", base)}
	gs, err := p.Groups(context.Background(), Input{Rows: rows, Now: base})
	if err != nil {
		t.Fatal(err)
	}
	g := groupByKey(t, gs, "w1")
	if len(g.Members) != 2 {
		t.Fatalf("want 2 members (one of them sessionless), got %+v", g.Members)
	}
	var orphan Member
	for _, m := range g.Members {
		if m.Ref == "w1:pCD" {
			orphan = m
		}
	}
	if orphan.SessionID != "" {
		t.Errorf("the empty pane joined to session %q; it has none", orphan.SessionID)
	}
	if !orphan.Activity.IsZero() {
		t.Errorf("a sessionless pane cannot have activity, got %v", orphan.Activity)
	}
	if orphan.Demand != DemandIdle {
		t.Errorf("orphan demand = %v, want the herdr status to still speak for it", orphan.Demand)
	}
}

// THE JOIN, in the decided precedence. All three shapes below are live on this
// machine at the same time.
func TestHerdrJoinPrefersAgentSessionThenThePaneColumn(t *testing.T) {
	talked := base.Add(-30 * time.Second)
	p, _ := stubProvider(t, liveSessionJSON, []herdr.Agent{
		// (1) herdr's own agent_session, resolving to one of our rows.
		withSession(agent("w1", "w1:pAA", "keeper", herdr.StatusIdle), "sess-a"),
		// (2) no agent_session (predates the integration) — the pane column answers.
		agent("w1", "w1:pBB", "pm", herdr.StatusIdle),
		// (1) names a session we cannot see at all, so it must FALL THROUGH to the
		// pane column rather than joining to nothing.
		withSession(agent("w1", "w1:pCC", "reaped", herdr.StatusIdle), "sess-vanished"),
	})
	rows := []db.Session{
		paneRow("sess-a", "w1", "w1:pAA", talked),
		paneRow("sess-b", "w1", "w1:pBB", talked),
		paneRow("sess-c", "w1", "w1:pCC", talked),
	}
	gs, err := p.Groups(context.Background(), Input{Rows: rows, Now: base})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, m := range groupByKey(t, gs, "w1").Members {
		got[m.Ref] = m.SessionID
	}
	want := map[string]string{"w1:pAA": "sess-a", "w1:pBB": "sess-b", "w1:pCC": "sess-c"}
	for ref, wantSID := range want {
		if got[ref] != wantSID {
			t.Errorf("pane %s joined to %q, want %q", ref, got[ref], wantSID)
		}
	}
}

// herdr's reported status is the primary signal, but a first-party PERMISSION PROMPT
// may raise a member to DemandHuman — it can never lower one.
func TestHerdrFirstPartyPromptOnlyRaisesDemand(t *testing.T) {
	p, _ := stubProvider(t, liveSessionJSON, []herdr.Agent{
		withSession(agent("w1", "w1:pAA", "keeper", herdr.StatusIdle), "sess-a"),
		withSession(agent("w1", "w1:pBB", "pm", herdr.StatusBlocked), "sess-b"),
	})
	live := map[string]clauded.Session{
		"sess-a": {SessionID: "sess-a", Status: clauded.Waiting, WaitingFor: "permission prompt", UpdatedAt: base},
		"sess-b": {SessionID: "sess-b", Status: clauded.Idle, UpdatedAt: base},
	}
	rows := []db.Session{paneRow("sess-a", "w1", "w1:pAA", base), paneRow("sess-b", "w1", "w1:pBB", base)}
	gs, err := p.Groups(context.Background(), Input{Rows: rows, Live: live, Now: base})
	if err != nil {
		t.Fatal(err)
	}
	byRef := map[string]Member{}
	for _, m := range groupByKey(t, gs, "w1").Members {
		byRef[m.Ref] = m
	}
	if byRef["w1:pAA"].Demand != DemandHuman {
		t.Errorf("a session sitting on a permission prompt must read awaits-human, got %v",
			byRef["w1:pAA"].Demand)
	}
	if byRef["w1:pAA"].Needs != "permission prompt" {
		t.Errorf("needs = %q; the first-party wait REASON is the only thing available — nothing is invented",
			byRef["w1:pAA"].Needs)
	}
	// herdr said blocked and the status file says nothing: the mapping still holds.
	if byRef["w1:pBB"].Demand != DemandHuman {
		t.Errorf("herdr `blocked` must map to awaits-human, got %v", byRef["w1:pBB"].Demand)
	}
	if byRef["w1:pBB"].Needs != "" {
		t.Errorf("no ask exists for a herdr-detected block; got %q", byRef["w1:pBB"].Needs)
	}
}

// PANE IDS CHURN MID-SESSION: measured w1:pCD -> w1:pCH -> w1:pCJ across ~20 minutes
// for one agent whose name never moved. The group and its membership must not notice.
func TestHerdrPaneChurnDoesNotDisturbTheGroup(t *testing.T) {
	p, _ := stubProvider(t, liveSessionJSON,
		[]herdr.Agent{agent("w1", "w1:pCD", "project-manager-keeper", herdr.StatusWorking)},
		[]herdr.Agent{agent("w1", "w1:pCH", "project-manager-keeper", herdr.StatusWorking)},
		[]herdr.Agent{agent("w1", "w1:pCJ", "project-manager-keeper", herdr.StatusWorking)},
	)
	var keys []GroupKey
	var names []string
	for i := range 3 {
		gs, err := p.Groups(context.Background(), Input{Now: base.Add(time.Duration(i) * time.Minute)})
		if err != nil {
			t.Fatal(err)
		}
		if len(gs) != 1 || len(gs[0].Members) != 1 {
			t.Fatalf("tick %d: want one group of one, got %+v", i, gs)
		}
		keys = append(keys, gs[0].Key)
		names = append(names, gs[0].Members[0].Name)
	}
	for i := range keys {
		if keys[i] != keys[0] {
			t.Errorf("group key moved with the pane id: %+v vs %+v", keys[i], keys[0])
		}
		if names[i] != "project-manager-keeper" {
			t.Errorf("agent name changed to %q while only its pane id moved", names[i])
		}
	}
}

// FRESHNESS IS THE MAX OVER MEMBERS. A 34-second tick dying must not reset the
// herd's idle age, and a tick starting must not make an old herd look new — the two
// halves of today's "idle 45m beside an actively working agent".
func TestHerdrGroupFreshnessIsTheMaxOverMembers(t *testing.T) {
	now := base
	stale := now.Add(-3 * time.Hour)
	fresh := now.Add(-2 * time.Second)

	p, _ := stubProvider(t, liveSessionJSON, []herdr.Agent{
		withSession(agent("w1", "w1:pOLD", "elder", herdr.StatusIdle), "sess-old"),
		withSession(agent("w1", "w1:pNEW", "tick", herdr.StatusWorking), "sess-new"),
	})
	rows := []db.Session{
		paneRow("sess-old", "w1", "w1:pOLD", stale),
		paneRow("sess-new", "w1", "w1:pNEW", fresh),
	}
	gs, err := p.Groups(context.Background(), Input{Rows: rows, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	sum := groupByKey(t, gs, "w1").Summarize()
	if got := time.UnixMilli(sum.NewestActivity); !got.Equal(fresh) {
		t.Errorf("group freshness = %v, want the NEWEST member (%v) — a three-hour-old member must not age the herd",
			got, fresh)
	}
}

// THE RATCHET. The tick that carried the herd's only recent activity ends and its
// row is reaped; a pure MAX would jump the herd's idle age BACKWARDS in time on the
// very tick something finished.
func TestHerdrGroupFreshnessNeverGoesBackwards(t *testing.T) {
	now := base
	fresh := now.Add(-2 * time.Second)
	stale := now.Add(-3 * time.Hour)

	p, _ := stubProvider(t, liveSessionJSON,
		[]herdr.Agent{
			withSession(agent("w1", "w1:pOLD", "elder", herdr.StatusIdle), "sess-old"),
			withSession(agent("w1", "w1:pNEW", "tick", herdr.StatusWorking), "sess-new"),
		},
		// Next tick: the 34-second colony tick is gone, pane and row with it.
		[]herdr.Agent{
			withSession(agent("w1", "w1:pOLD", "elder", herdr.StatusIdle), "sess-old"),
		},
	)
	first, err := p.Groups(context.Background(), Input{Now: now, Rows: []db.Session{
		paneRow("sess-old", "w1", "w1:pOLD", stale),
		paneRow("sess-new", "w1", "w1:pNEW", fresh),
	}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.Groups(context.Background(), Input{Now: now.Add(time.Second), Rows: []db.Session{
		paneRow("sess-old", "w1", "w1:pOLD", stale),
	}})
	if err != nil {
		t.Fatal(err)
	}
	a := groupByKey(t, first, "w1").Summarize().NewestActivity
	b := groupByKey(t, second, "w1").Summarize().NewestActivity
	if b < a {
		t.Errorf("freshness went backwards when a member died: %v -> %v",
			time.UnixMilli(a), time.UnixMilli(b))
	}
	if got := time.UnixMilli(b); !got.Equal(fresh) {
		t.Errorf("freshness after the tick died = %v, want the mark it left (%v)", got, fresh)
	}
}

// The mark is forgotten when the herd itself goes away, so a workspace that comes
// back does so with an honest history (and a long-lived daemon cannot accumulate one
// entry per workspace ever seen).
func TestHerdrFreshnessMarkIsDroppedWithItsGroup(t *testing.T) {
	fresh := base.Add(-2 * time.Second)
	p, _ := stubProvider(t, liveSessionJSON,
		[]herdr.Agent{withSession(agent("w1", "w1:pA", "a", herdr.StatusWorking), "s1")},
		[]herdr.Agent{}, // herd gone
		[]herdr.Agent{agent("w1", "w1:pZ", "z", herdr.StatusIdle)}, // and back, sessionless
	)
	in := Input{Now: base, Rows: []db.Session{paneRow("s1", "w1", "w1:pA", fresh)}}
	if _, err := p.Groups(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Groups(context.Background(), Input{Now: base.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	if n := len(p.freshest); n != 0 {
		t.Errorf("freshness marks left after the herd went away: %+v", p.freshest)
	}
	gs, err := p.Groups(context.Background(), Input{Now: base.Add(2 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if got := groupByKey(t, gs, "w1").Summarize().NewestActivity; got != 0 {
		t.Errorf("a returning herd inherited a stale mark: %v", time.UnixMilli(got))
	}
}

// No herdr on this machine says NOTHING and stops asking, or the daemon pays a PATH
// search a second forever. It asks again after the retry window, so installing herdr
// does not require a restart.
func TestHerdrNotInstalledIsSilentAndLatched(t *testing.T) {
	p := NewHerdrProvider(filepath.Join(t.TempDir(), "session.json"))
	p.retryAfter = time.Minute
	calls := 0
	p.list = func(context.Context) ([]herdr.Agent, error) {
		calls++
		return nil, herdr.ErrNotInstalled
	}
	for i := range 5 {
		gs, err := p.Groups(context.Background(), Input{Now: base.Add(time.Duration(i) * time.Second)})
		if err != nil || gs != nil {
			t.Fatalf("tick %d: got (%v, %v), want no groups and no error", i, gs, err)
		}
	}
	if calls != 1 {
		t.Errorf("probed %d times inside the retry window, want 1", calls)
	}
	if _, err := p.Groups(context.Background(), Input{Now: base.Add(2 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("after the retry window the probe should run again; calls = %d", calls)
	}
}

// A herdr that is merely ASLEEP is a different case: the code names something a human
// can act on, it surfaces, and it is asked again on the very next tick because
// starting herdr is one keystroke.
func TestHerdrServerRefusalSurfacesAndIsNotLatched(t *testing.T) {
	p := NewHerdrProvider(filepath.Join(t.TempDir(), "session.json"))
	calls := 0
	p.list = func(context.Context) ([]herdr.Agent, error) {
		calls++
		return nil, &herdr.ServerError{Code: "server_not_running"}
	}
	for i := range 3 {
		if _, err := p.Groups(context.Background(), Input{Now: base.Add(time.Duration(i) * time.Second)}); err == nil {
			t.Fatalf("tick %d: a herdr refusal must surface for doctor to print", i)
		}
	}
	if calls != 3 {
		t.Errorf("calls = %d, want one per tick — an asleep herdr can wake up", calls)
	}
}

// No session.json at all (or an unparseable one) degrades labels to the bare
// workspace id rather than taking a tick down.
func TestHerdrMissingSessionFileDegradesToWorkspaceIDs(t *testing.T) {
	p, _ := stubProvider(t, "", []herdr.Agent{agent("w1", "w1:pA", "a", herdr.StatusIdle)})
	gs, err := p.Groups(context.Background(), Input{Now: base})
	if err != nil {
		t.Fatal(err)
	}
	if l := groupByKey(t, gs, "w1").Label; l != "w1" {
		t.Errorf("label = %q, want the raw workspace id", l)
	}
}

// session.json is re-parsed only when the file changes — the same mtime/size gate
// Reader uses for job state files.
func TestHerdrSessionFileIsCachedUntilItChanges(t *testing.T) {
	p, _ := stubProvider(t, liveSessionJSON, []herdr.Agent{agent("w9", "w9:pA", "helpdesk", herdr.StatusIdle)})
	if _, err := p.Groups(context.Background(), Input{Now: base}); err != nil {
		t.Fatal(err)
	}
	first := p.workspaces
	if _, err := p.Groups(context.Background(), Input{Now: base.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	if len(p.workspaces) != len(first) {
		t.Fatal("cache was not reused")
	}
	renamed := `{"version":3,"workspaces":[{"id":"w9","custom_name":"renamed","identity_cwd":"/x","tabs":[]}]}`
	if err := os.WriteFile(p.sessionPath, []byte(renamed), 0o600); err != nil {
		t.Fatal(err)
	}
	// A same-second rewrite must still be seen: the gate compares size too.
	gs, err := p.Groups(context.Background(), Input{Now: base.Add(2 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if l := groupByKey(t, gs, "w9").Label; l != "renamed" {
		t.Errorf("label = %q after the file changed, want %q", l, "renamed")
	}
}

// The fingerprint is herdr's own change signal, and it must move when a status does —
// that is the only thing a caller may skip work on.
func TestHerdrFingerprintTracksTheRoster(t *testing.T) {
	a := agent("w1", "w1:pA", "a", herdr.StatusIdle)
	moved := a
	moved.Status = herdr.StatusWorking
	p, _ := stubProvider(t, liveSessionJSON, []herdr.Agent{a}, []herdr.Agent{moved})
	if _, err := p.Groups(context.Background(), Input{Now: base}); err != nil {
		t.Fatal(err)
	}
	before := p.Fingerprint()
	if before == "" {
		t.Fatal("fingerprint is empty for a non-empty roster")
	}
	if _, err := p.Groups(context.Background(), Input{Now: base.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	if p.Fingerprint() == before {
		t.Error("fingerprint did not move when an agent changed status")
	}
}

// An agent with no workspace id has nothing to group on and is dropped rather than
// collected under an empty key.
func TestHerdrAgentWithNoWorkspaceIsDropped(t *testing.T) {
	p, _ := stubProvider(t, liveSessionJSON, []herdr.Agent{
		agent("", "pX", "orphan", herdr.StatusIdle),
		agent("w1", "w1:pA", "a", herdr.StatusIdle),
	})
	gs, err := p.Groups(context.Background(), Input{Now: base})
	if err != nil {
		t.Fatal(err)
	}
	if len(gs) != 1 || gs[0].Key.Key != "w1" {
		t.Errorf("want only the w1 group, got %+v", gs)
	}
}
