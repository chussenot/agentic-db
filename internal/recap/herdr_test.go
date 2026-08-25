package recap

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mrzor/claude-status/internal/db"
	"github.com/mrzor/claude-status/internal/herdr"
)

// colonySession is trimmed from the real ~/.config/herdr/session.json of
// 2026-08-25 and keeps the two shapes that matter:
//
//   - w1 is the MAIN workspace holding TWO named agents. Nothing but the
//     first-party agent_session bridge can tell its ticks apart, so a tick
//     without one must stay unnamed rather than pick a sibling at random.
//   - w9 is a colony chamber: one workspace, one named agent, custom_name set.
//     This is the shape that carries history — the pane number is long gone by
//     recap time, the workspace and the name are not.
//
// Every worktree_space.label is "project-manager" on purpose: that is the REPO
// name, shared by all three, which is why custom_name has to win.
const colonySession = `{
  "version": 3,
  "workspaces": [
    {
      "id": "w1",
      "custom_name": null,
      "identity_cwd": "/home/zor/taf/project-manager",
      "worktree_space": {
        "key": "/home/zor/taf/project-manager/.git",
        "label": "project-manager",
        "repo_root": "/home/zor/taf/project-manager",
        "checkout_path": "/home/zor/taf/project-manager",
        "is_linked_worktree": false
      },
      "public_pane_numbers": {"1": 1, "7": 283, "123": 397},
      "tabs": [
        {"panes": {"1": {"cwd": "/home/zor/taf/project-manager"}}},
        {"panes": {"7": {
          "cwd": "/home/zor/taf/project-manager",
          "agent_name": "project-manager-helpdesk",
          "managed_agent_kind": "claude"
        }}},
        {"panes": {"123": {
          "cwd": "/home/zor/taf/project-manager",
          "agent_name": "project-manager-integrator",
          "managed_agent_kind": "claude",
          "agent_session": {
            "source": "herdr:claude",
            "agent": "claude",
            "kind": "id",
            "value": "integrator-tick-2"
          }
        }}}
      ]
    },
    {
      "id": "w9",
      "custom_name": "project-manager-6",
      "identity_cwd": "/home/zor/taf/project-manager/.claude/worktrees/project-manager-6",
      "worktree_space": {
        "key": "/home/zor/taf/project-manager/.git",
        "label": "project-manager",
        "repo_root": "/home/zor/taf/project-manager",
        "checkout_path": "/home/zor/taf/project-manager/.claude/worktrees/project-manager-6",
        "is_linked_worktree": true
      },
      "tabs": [
        {"panes": {"9": {
          "cwd": "/home/zor/taf/project-manager/.claude/worktrees/project-manager-6",
          "agent_name": "project-manager-keeper",
          "managed_agent_kind": "claude"
        }}}
      ]
    }
  ]
}`

// herdrRow is a sessions row as the hook writes it for a pane of a herdr
// session: the herdr ids present, and the first-party session_name whatever
// Claude derived for that run.
func herdrRow(sid, workspace, pane, sessionName string) db.Session {
	s := db.Session{
		SessionID:        sid,
		HerdrWorkspaceID: sql.NullString{String: workspace, Valid: true},
		HerdrPaneID:      sql.NullString{String: pane, Valid: true},
	}
	if sessionName != "" {
		s.SessionName = sql.NullString{String: sessionName, Valid: true}
	}
	return s
}

func TestResolveAgentNameLadder(t *testing.T) {
	ws := herdr.ParseSession([]byte(colonySession))
	for _, tc := range []struct {
		name          string
		row           db.Session
		wantOK        bool
		wantName      string
		wantWorkspace string
		wantLabel     string
		why           string
	}{{
		name:          "first-party bridge names this very session id",
		row:           herdrRow("integrator-tick-2", "w1", "w1:pCD", "project-manager-9a"),
		wantOK:        true,
		wantName:      "project-manager-integrator",
		wantWorkspace: "project-manager",
		wantLabel:     "project-manager-integrator",
		why:           "agent_session.value is the exact join, and the only rung that can pick one of two siblings",
	}, {
		name:          "session_name happens to match a pane name",
		row:           herdrRow("t1", "w1", "w1:p8V", "project-manager-helpdesk"),
		wantOK:        true,
		wantName:      "project-manager-helpdesk",
		wantWorkspace: "project-manager",
		wantLabel:     "project-manager-helpdesk",
		why:           "Claude's per-run name is weak, but an exact match against a herdr pane name is still a match",
	}, {
		name:          "two named agents and nothing to tie the tick to one",
		row:           herdrRow("t2", "w1", "w1:p8V", "project-manager-9a"),
		wantOK:        true,
		wantName:      "",
		wantWorkspace: "project-manager",
		wantLabel:     "project-manager",
		why:           "guessing a sibling is worse than naming the workspace; the label still beats a window title",
	}, {
		name:          "sole named agent in a chamber",
		row:           herdrRow("t3", "w9", "w9:pAB", "project-manager-6b"),
		wantOK:        true,
		wantName:      "project-manager-keeper",
		wantWorkspace: "project-manager-6",
		wantLabel:     "project-manager-keeper",
		why:           "one workspace holding one agent is unambiguous, and this is the rung that survives the pane dying",
	}, {
		name:          "workspace session.json has never heard of",
		row:           herdrRow("t4", "wZ", "wZ:p1", ""),
		wantOK:        true,
		wantName:      "",
		wantWorkspace: "wZ",
		wantLabel:     "", // "wZ" is no better than a title, so the caller keeps what it had
		why:           "a workspace created since the last write degrades to the bare id",
	}, {
		name:   "no herdr ids at all",
		row:    db.Session{SessionID: "ordinary"},
		wantOK: false,
		why:    "the gate that keeps every ordinary session on the pre-herdr path",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			a, ok := ResolveAgent(tc.row, ws)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (%s)", ok, tc.wantOK, tc.why)
			}
			if !ok {
				return
			}
			if a.Name != tc.wantName {
				t.Errorf("Name = %q, want %q (%s)", a.Name, tc.wantName, tc.why)
			}
			if a.Workspace != tc.wantWorkspace {
				t.Errorf("Workspace = %q, want %q", a.Workspace, tc.wantWorkspace)
			}
			if got := a.Label(); got != tc.wantLabel {
				t.Errorf("Label() = %q, want %q (%s)", got, tc.wantLabel, tc.why)
			}
		})
	}
}

// TestSessionFileSuppliesTheLabel goes through ReadSession from a real file on
// disk, the path the subcommand takes, so a fixture that only ParseSession
// accepts cannot pass.
func TestSessionFileSuppliesTheLabel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.json")
	if err := os.WriteFile(path, []byte(colonySession), 0o600); err != nil {
		t.Fatal(err)
	}
	ws, err := herdr.ReadSession(path)
	if err != nil {
		t.Fatalf("ReadSession: %v", err)
	}
	lookup := AgentLookupFor([]db.Session{
		herdrRow("t3", "w9", "w9:pAB", ""),
		{SessionID: "ordinary"},
	}, ws)
	if lookup == nil {
		t.Fatal("lookup is nil, want one built from the herdr-hosted row")
	}
	a, ok := lookup("t3")
	if !ok || a.Name != "project-manager-keeper" || a.Workspace != "project-manager-6" {
		t.Errorf("lookup(t3) = %+v, %v; want the chamber's agent name and custom_name label", a, ok)
	}
	if a.PaneID != "w9:pAB" {
		t.Errorf("PaneID = %q, want it carried for diagnostics", a.PaneID)
	}
	if _, ok := lookup("ordinary"); ok {
		t.Error("a row with no herdr ids must not resolve")
	}
}

// TestAgentLookupForNilWithoutHerdr pins the cheap escape hatch: on a machine
// that never ran herdr there is no lookup at all, so Build's herdr branch is
// unreachable rather than merely inert.
func TestAgentLookupForNilWithoutHerdr(t *testing.T) {
	rows := []db.Session{{SessionID: "a"}, {SessionID: "b"}}
	if got := AgentLookupFor(rows, map[string]herdr.Workspace{}); got != nil {
		t.Error("no herdr-hosted rows must yield a nil AgentLookup")
	}
}

// tickEvents is one colony tick: a prompt, a tool call, a Stop, and the shared
// multiplexer title that is all a dead tick used to leave behind.
func tickEvents(sid string, startMin int64, title string) []db.Event {
	return []db.Event{
		ev(startMin*min, sid, "UserPromptSubmit", "working"),
		titleEv(startMin*min, sid, title),
		ev((startMin+1)*min, sid, "PostToolUse", "unchanged"),
		ev((startMin+2)*min, sid, "Stop", "idle"),
	}
}

// TestHerdrTicksAggregateUnderOneAgent is the bead's acceptance criterion: three
// short lives of one colony agent, each leaving a different herdr window title
// behind, must read as ONE row named after the agent — not as three mystery
// lines called "herdr server stop".
func TestHerdrTicksAggregateUnderOneAgent(t *testing.T) {
	from, to := time.UnixMilli(0), time.UnixMilli(1000*min)
	var events []db.Event
	events = append(events, tickEvents("k1", 0, "herdr server stop")...)
	events = append(events, tickEvents("k2", 10, "ML-PF603D2Z: project-manager")...)
	events = append(events, tickEvents("k3", 20, "~/taf/project-manager")...)
	// A plain interactive session alongside, to prove coalescing is scoped.
	events = append(events, tickEvents("plain", 30, "fixing the decay timeline")...)

	ws := herdr.ParseSession([]byte(colonySession))
	lookup := AgentLookupFor([]db.Session{
		herdrRow("k1", "w9", "w9:pCD", "project-manager-6a"),
		herdrRow("k2", "w9", "w9:pCH", "project-manager-6b"), // pane churned mid-window
		herdrRow("k3", "w9", "w9:pCH", "project-manager-6c"),
		{SessionID: "plain"},
	}, ws)

	d := Build(events, noLookup, nil, lookup, from, to, 10)

	if d.Totals.Sessions != 2 {
		t.Fatalf("rows = %d, want 2 (one agent + one plain session): %+v", d.Totals.Sessions, d.Sessions)
	}
	if d.Totals.Ticks != 4 {
		t.Errorf("Totals.Ticks = %d, want 4 (the raw session-id count is still reported)", d.Totals.Ticks)
	}
	var agent, plain *Session
	for i := range d.Sessions {
		if d.Sessions[i].Agent != nil {
			agent = &d.Sessions[i]
		} else {
			plain = &d.Sessions[i]
		}
	}
	if agent == nil || plain == nil {
		t.Fatalf("want one agent row and one plain row, got %+v", d.Sessions)
	}
	if agent.Topic != "project-manager-keeper" {
		t.Errorf("agent topic = %q, want the herdr agent name (not a multiplexer title)", agent.Topic)
	}
	if agent.Ticks != 3 {
		t.Errorf("agent ticks = %d, want 3", agent.Ticks)
	}
	if agent.ID != "k3" {
		t.Errorf("agent ID = %q, want the LATEST tick (the one a human could resume)", agent.ID)
	}
	if agent.Turns != 3 {
		t.Errorf("agent turns = %d, want 3 (one Stop per tick, summed)", agent.Turns)
	}
	if agent.Prompts != 3 {
		t.Errorf("agent prompts = %d, want 3", agent.Prompts)
	}
	// Each tick works [start, start+2m]; the ticks do not overlap, so the union is 6m.
	if agent.Active != 6*time.Minute {
		t.Errorf("agent active = %s, want 6m (union of the three ticks)", agent.Active)
	}
	if !agent.Started.Equal(time.UnixMilli(0)) || !agent.Ended.Equal(time.UnixMilli(22*min)) {
		t.Errorf("agent span = %s → %s, want the first tick's start to the last tick's end",
			agent.Started, agent.Ended)
	}
	// The plain session keeps its window title and its single-tick shape.
	if plain.Topic != "fixing the decay timeline" || plain.Ticks != 1 {
		t.Errorf("plain session altered: topic=%q ticks=%d", plain.Topic, plain.Ticks)
	}

	var b strings.Builder
	Markdown(&b, d)
	out := b.String()
	for _, want := range []string{
		"- **project-manager-keeper** — 6m across 3 ticks, 3 turns, 0 prompts",
		"**2 sessions (4 ticks) ·",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("markdown missing %q\n---\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"herdr server stop", "ML-PF603D2Z", "~/taf/project-manager"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("multiplexer title %q leaked into the recap\n---\n%s", unwanted, out)
		}
	}
}

// TestHerdrPaneChurnDoesNotSplitAnAgent isolates the trap that makes pane id
// unusable as an identity: an agent observed live moving w1:pCD → w1:pCH while
// its name stayed put. Grouping on the pane would have produced two rows.
func TestHerdrPaneChurnDoesNotSplitAnAgent(t *testing.T) {
	from, to := time.UnixMilli(0), time.UnixMilli(1000*min)
	events := append(tickEvents("c1", 0, "herdr"), tickEvents("c2", 5, "herdr")...)
	ws := herdr.ParseSession([]byte(colonySession))
	lookup := AgentLookupFor([]db.Session{
		herdrRow("c1", "w9", "w9:pCD", ""),
		herdrRow("c2", "w9", "w9:pCH", ""),
	}, ws)
	d := Build(events, noLookup, nil, lookup, from, to, 10)
	if len(d.Sessions) != 1 {
		t.Fatalf("rows = %d, want 1 despite the pane id changing: %+v", len(d.Sessions), d.Sessions)
	}
	if d.Sessions[0].Ticks != 2 || d.Sessions[0].Topic != "project-manager-keeper" {
		t.Errorf("row = %+v, want 2 ticks under the agent name", d.Sessions[0])
	}
}

// TestUnnamedHerdrTicksFallBackToTheWorkspace covers the ambiguous main
// workspace: no rung of the ladder names the ticks, so they aggregate under the
// chamber label instead. Still one row, still not a window title.
func TestUnnamedHerdrTicksFallBackToTheWorkspace(t *testing.T) {
	from, to := time.UnixMilli(0), time.UnixMilli(1000*min)
	events := append(tickEvents("u1", 0, "herdr server stop"), tickEvents("u2", 5, "herdr")...)
	ws := herdr.ParseSession([]byte(colonySession))
	lookup := AgentLookupFor([]db.Session{
		herdrRow("u1", "w1", "w1:p8V", "project-manager-9a"),
		herdrRow("u2", "w1", "w1:p8W", "project-manager-9b"),
	}, ws)
	d := Build(events, noLookup, nil, lookup, from, to, 10)
	if len(d.Sessions) != 1 || d.Sessions[0].Topic != "project-manager" {
		t.Fatalf("want one row labelled with the workspace, got %+v", d.Sessions)
	}
	if d.Sessions[0].Agent == nil || d.Sessions[0].Agent.Name != "" {
		t.Errorf("the agent must stay unnamed rather than borrow a sibling's name: %+v", d.Sessions[0].Agent)
	}
}

// TestNoHerdrIDsRendersUnchanged is the strict regression guard. Ordinary
// sessions — and every session that predates the herdr columns — must come out
// of Build and Markdown exactly as before, whether herdr attribution is absent
// (nil lookup) or merely resolves nothing.
func TestNoHerdrIDsRendersUnchanged(t *testing.T) {
	from, to := time.UnixMilli(0), time.UnixMilli(1000*min)
	events := append(tickEvents("A", 0, "wire the fleet provider"), tickEvents("B", 10, "fix the tile cap")...)
	lookup := func(sid string) (Meta, bool) {
		return Meta{Cwd: "/home/zor/claude-status", Branch: "master", Ask: "why is the tile lying"}, true
	}
	// Not nil, but never resolves — the shape a real machine with herdr installed
	// and no herdr-hosted sessions in the window produces.
	neverResolves := func(string) (Agent, bool) { return Agent{}, false }

	withoutHerdr := Build(events, lookup, nil, nil, from, to, 10)
	inert := Build(events, lookup, nil, neverResolves, from, to, 10)

	var a, b strings.Builder
	Markdown(&a, withoutHerdr)
	MetricsMarkdown(&a, withoutHerdr)
	Markdown(&b, inert)
	MetricsMarkdown(&b, inert)
	if a.String() != b.String() {
		t.Errorf("an unresolving AgentLookup changed the output:\n--- nil ---\n%s\n--- inert ---\n%s", a.String(), b.String())
	}

	// And the output itself is still the pre-herdr format: no tick counts anywhere,
	// no Agent on any row.
	out := a.String()
	if strings.Contains(out, "tick") {
		t.Errorf("a non-herdr recap must never mention ticks:\n%s", out)
	}
	for _, want := range []string{
		"**2 sessions · 4m active · 0 prompts · 2 turns · 1 project**",
		"### claude-status — 4m",
		"- **wire the fleet provider** (master) — 2m, 1 turns, 0 prompts",
		"- **fix the tile cap** (master) — 2m, 1 turns, 0 prompts",
		`  ↳ "why is the tile lying"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("pre-herdr format changed; missing %q\n---\n%s", want, out)
		}
	}
	for _, s := range withoutHerdr.Sessions {
		if s.Agent != nil {
			t.Errorf("session %s got an Agent it should not have: %+v", s.ID, s.Agent)
		}
		if s.Ticks != 1 {
			t.Errorf("session %s Ticks = %d, want 1", s.ID, s.Ticks)
		}
	}
	if withoutHerdr.Totals.Ticks != withoutHerdr.Totals.Sessions {
		t.Errorf("Totals.Ticks (%d) must equal Totals.Sessions (%d) when nothing coalesced",
			withoutHerdr.Totals.Ticks, withoutHerdr.Totals.Sessions)
	}
}

// TestHerdrAgentsWiring exercises the subcommand's own path end to end: a real
// sessions table carrying the ids the hook captured, a real session.json on
// disk, and the AgentLookup that Build consumes. The pure tests above prove the
// aggregation; this proves the plumbing that feeds it, which is the part the
// live database cannot demonstrate until the hook that writes those ids ships.
func TestHerdrAgentsWiring(t *testing.T) {
	dir := t.TempDir()
	database, err := db.Open(filepath.Join(dir, "claude.sqlite"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer database.Close()

	for _, s := range []db.Session{
		herdrRow("live-tick", "w9", "w9:pCH", "project-manager-6a"),
		{SessionID: "ordinary"},
	} {
		s.State, s.LastSeenTS, s.CreatedTS = "idle", 1, 1
		if err := database.Upsert(s); err != nil {
			t.Fatalf("upsert %s: %v", s.SessionID, err)
		}
	}
	path := filepath.Join(dir, "session.json")
	if err := os.WriteFile(path, []byte(colonySession), 0o600); err != nil {
		t.Fatal(err)
	}

	lookup := herdrAgents(database, path)
	if lookup == nil {
		t.Fatal("herdrAgents returned nil despite a herdr-hosted row")
	}
	a, ok := lookup("live-tick")
	if !ok || a.Name != "project-manager-keeper" {
		t.Errorf("lookup(live-tick) = %+v, %v; want the durable agent name, not the per-run session_name", a, ok)
	}
	if _, ok := lookup("ordinary"); ok {
		t.Error("the non-herdr row must not resolve")
	}

	// A missing session.json (herdr never ran here) must not break the lookup —
	// the ids still resolve to a workspace, just to its bare id.
	if got := herdrAgents(database, filepath.Join(dir, "absent.json")); got == nil {
		t.Error("a missing session.json should still yield workspace-level attribution")
	} else if a, _ := got("live-tick"); a.Label() != "" || a.WorkspaceID != "w9" {
		t.Errorf("degraded agent = %+v, want workspace w9 and no label", a)
	}
}

// TestOutOfOrderTicksKeepTheNewestContext pins the trap in folding: Started is
// MINIMIZED as ticks merge, so "did this tick start after the row's start?" is
// not the same question as "is this the newest tick?". Build groups sessions by
// first appearance in the event slice, and its caller is explicitly allowed to
// pass events in any order, so the newest tick can arrive in the middle.
func TestOutOfOrderTicksKeepTheNewestContext(t *testing.T) {
	from, to := time.UnixMilli(0), time.UnixMilli(1000*min)
	var events []db.Event
	events = append(events, tickEvents("oldest", 0, "herdr")...)
	events = append(events, tickEvents("newest", 20, "herdr")...) // newest seen SECOND
	events = append(events, tickEvents("middle", 10, "herdr")...) // then an older one
	ws := herdr.ParseSession([]byte(colonySession))
	lookup := AgentLookupFor([]db.Session{
		herdrRow("oldest", "w9", "w9:pA", ""),
		herdrRow("newest", "w9", "w9:pC", ""),
		herdrRow("middle", "w9", "w9:pB", ""),
	}, ws)
	d := Build(events, noLookup, nil, lookup, from, to, 10)
	if len(d.Sessions) != 1 {
		t.Fatalf("rows = %d, want 1: %+v", len(d.Sessions), d.Sessions)
	}
	s := d.Sessions[0]
	if s.ID != "newest" {
		t.Errorf("ID = %q, want %q — the row must name the newest tick regardless of arrival order", s.ID, "newest")
	}
	if s.Ticks != 3 || !s.Started.Equal(time.UnixMilli(0)) || !s.Ended.Equal(time.UnixMilli(22*min)) {
		t.Errorf("row = ticks %d, %s → %s; want 3 ticks spanning 0 → 22m", s.Ticks, s.Started, s.Ended)
	}
}
