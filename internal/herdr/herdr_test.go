package herdr

import (
	"context"
	"errors"
	"testing"
)

// liveRoster is a verbatim `herdr agent list` payload, captured 2026-08-25 from
// herdr 0.8.2 (socket protocol 20). Kept byte-for-byte rather than prettified so
// these tests fail if the real envelope ever stops looking like this. It happens to
// carry every case that matters at once:
//   - the double envelope (.result.agents, not .agents)
//   - a STICKY `done` status (the helpdesk agent had been sitting there for minutes)
//   - one agent WITH agent_session and three WITHOUT, live at the same time,
//     because only the first had restarted since the Claude integration was
//     installed
//   - workspaces w1/w9/wA, i.e. several agents behind one herdr session
const liveRoster = `{"id":"cli:agent:list","result":{"agents":[{"agent":"claude","agent_status":"done","cwd":"/home/eliebleton/taf/project-manager","focused":true,"foreground_cwd":"/home/eliebleton/taf/project-manager","interactive_ready":true,"name":"project-manager-helpdesk","pane_id":"w1:p8V","revision":4,"state_change_seq":339,"tab_id":"w1:t8V","terminal_id":"term_659da53280c417","terminal_title":"✳ Workers 6 and 7 integration","terminal_title_stripped":"Workers 6 and 7 integration","workspace_id":"w1"},{"agent":"claude","agent_session":{"agent":"claude","kind":"id","source":"herdr:claude","value":"c90b0c55-9e66-4a95-9c9a-5bf052a77338"},"agent_status":"working","cwd":"/home/eliebleton/taf/project-manager","focused":false,"foreground_cwd":"/home/eliebleton/taf/project-manager","interactive_ready":true,"name":"project-manager-integrator","pane_id":"w1:pCD","revision":3,"state_change_seq":342,"tab_id":"w1:tCD","terminal_id":"term_659ddf8a5e2937b","terminal_title":"◑ colony-integrator","terminal_title_stripped":"colony-integrator","workspace_id":"w1"},{"agent":"claude","agent_status":"idle","cwd":"/home/eliebleton/taf/project-manager/.claude/worktrees/project-manager-6","focused":false,"foreground_cwd":"/home/eliebleton/taf/project-manager/.claude/worktrees/project-manager-6","interactive_ready":true,"name":"project-manager-6","pane_id":"w9:p1","revision":3,"state_change_seq":21,"tab_id":"w9:t1","terminal_id":"term_659da7b12436d9","terminal_title":"✳ Claude Code","terminal_title_stripped":"Claude Code","workspace_id":"w9"},{"agent":"claude","agent_status":"idle","cwd":"/home/eliebleton/taf/project-manager/.claude/worktrees/project-manager-7","focused":false,"foreground_cwd":"/home/eliebleton/taf/project-manager/.claude/worktrees/project-manager-7","interactive_ready":true,"name":"project-manager-7","pane_id":"wA:p1","revision":3,"state_change_seq":284,"tab_id":"wA:t1","terminal_id":"term_659dc98af9d4b51","terminal_title":"✳ Claude Code","terminal_title_stripped":"Claude Code","workspace_id":"wA"}],"type":"agent_list"}}`

// byPane indexes a parsed roster for assertions.
func byPane(agents []Agent) map[string]Agent {
	out := make(map[string]Agent, len(agents))
	for _, a := range agents {
		out[a.PaneID] = a
	}
	return out
}

func TestParseAgentListLivePayload(t *testing.T) {
	got, err := ParseAgentList([]byte(liveRoster))
	if err != nil {
		t.Fatalf("ParseAgentList: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d agents, want 4 (are we still reading .result.agents?)", len(got))
	}

	idx := byPane(got)
	for _, tc := range []struct {
		pane        string
		wantName    string
		wantStatus  Status
		wantWS      string
		wantTab     string
		wantCwd     string
		wantSession string // "" means: no agent_session key in the payload
	}{
		{
			pane: "w1:p8V", wantName: "project-manager-helpdesk", wantStatus: StatusDone,
			wantWS: "w1", wantTab: "w1:t8V", wantCwd: "/home/eliebleton/taf/project-manager",
		},
		{
			pane: "w1:pCD", wantName: "project-manager-integrator", wantStatus: StatusWorking,
			wantWS: "w1", wantTab: "w1:tCD", wantCwd: "/home/eliebleton/taf/project-manager",
			wantSession: "c90b0c55-9e66-4a95-9c9a-5bf052a77338",
		},
		{
			pane: "w9:p1", wantName: "project-manager-6", wantStatus: StatusIdle,
			wantWS: "w9", wantTab: "w9:t1",
			wantCwd: "/home/eliebleton/taf/project-manager/.claude/worktrees/project-manager-6",
		},
		{
			pane: "wA:p1", wantName: "project-manager-7", wantStatus: StatusIdle,
			wantWS: "wA", wantTab: "wA:t1",
			wantCwd: "/home/eliebleton/taf/project-manager/.claude/worktrees/project-manager-7",
		},
	} {
		a, ok := idx[tc.pane]
		if !ok {
			t.Errorf("pane %s missing from roster", tc.pane)
			continue
		}
		if a.Name != tc.wantName || a.Status != tc.wantStatus {
			t.Errorf("%s: name/status = %q/%q, want %q/%q", tc.pane, a.Name, a.Status, tc.wantName, tc.wantStatus)
		}
		if a.WorkspaceID != tc.wantWS || a.TabID != tc.wantTab || a.Cwd != tc.wantCwd {
			t.Errorf("%s: ids/cwd = %q/%q/%q", tc.pane, a.WorkspaceID, a.TabID, a.Cwd)
		}
		if a.Kind != "claude" {
			t.Errorf("%s: kind = %q, want claude", tc.pane, a.Kind)
		}
		if tc.wantSession == "" {
			if a.HasSession {
				t.Errorf("%s: HasSession true, but the payload has no agent_session key", tc.pane)
			}
			if a.Session != (AgentSession{}) {
				t.Errorf("%s: session should be zero when absent, got %+v", tc.pane, a.Session)
			}
			continue
		}
		if !a.HasSession {
			t.Errorf("%s: HasSession false despite a present agent_session", tc.pane)
		}
		if a.Session.Value != tc.wantSession || a.Session.Kind != "id" || a.Session.Source != "herdr:claude" {
			t.Errorf("%s: session = %+v", tc.pane, a.Session)
		}
	}

	// Only the pane the human is looking at is focused, and it is not a property
	// of the work — assert it decodes so nobody "fixes" it into a status.
	if !idx["w1:p8V"].Focused || idx["w1:pCD"].Focused {
		t.Errorf("focused decoded wrong: %v / %v", idx["w1:p8V"].Focused, idx["w1:pCD"].Focused)
	}
	if idx["w1:p8V"].Revision != 4 || idx["w1:p8V"].StateChangeSeq != 339 {
		t.Errorf("change counters decoded wrong: %+v", idx["w1:p8V"])
	}
	if idx["w1:pCD"].TitleStripped != "colony-integrator" || idx["w1:pCD"].TerminalTitle != "◑ colony-integrator" {
		t.Errorf("titles decoded wrong: %+v", idx["w1:pCD"])
	}
}

func TestParseAgentListSortsDeterministically(t *testing.T) {
	// herdr promises no order. Feed it reversed and expect (workspace, pane).
	payload := `{"id":"cli:agent:list","result":{"agents":[
		{"agent":"claude","agent_status":"idle","name":"c","pane_id":"wA:p1","workspace_id":"wA"},
		{"agent":"claude","agent_status":"idle","name":"b","pane_id":"w1:pCD","workspace_id":"w1"},
		{"agent":"claude","agent_status":"idle","name":"a","pane_id":"w1:p8V","workspace_id":"w1"}]}}`
	got, err := ParseAgentList([]byte(payload))
	if err != nil {
		t.Fatalf("ParseAgentList: %v", err)
	}
	want := []string{"w1:p8V", "w1:pCD", "wA:p1"}
	for i, w := range want {
		if got[i].PaneID != w {
			t.Fatalf("order = %v, want %v", []Agent{got[0], got[1], got[2]}, want)
		}
	}
}

func TestParseAgentListNullName(t *testing.T) {
	// herdr clears `name` when a pane's occupant exits, so a null name is a normal
	// live reading. Both null and an omitted key must decode to "" without losing
	// the agent — the pane is still there.
	for _, tc := range []struct{ name, payload string }{
		{"explicit null", `{"id":"cli:agent:list","result":{"agents":[{"agent":"claude","agent_status":"idle","name":null,"pane_id":"w1:p2","tab_id":"w1:t2","workspace_id":"w1"}]}}`},
		{"key omitted", `{"id":"cli:agent:list","result":{"agents":[{"agent":"claude","agent_status":"idle","pane_id":"w1:p2","tab_id":"w1:t2","workspace_id":"w1"}]}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseAgentList([]byte(tc.payload))
			if err != nil {
				t.Fatalf("ParseAgentList: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("an unnamed pane must still be reported, got %d agents", len(got))
			}
			if got[0].Name != "" {
				t.Errorf("name = %q, want empty", got[0].Name)
			}
			if _, ok := got[0].Key(); ok {
				t.Error("Key must report not-ok for an unnamed agent: there is nothing durable to key on")
			}
		})
	}
}

func TestKeyIsWorkspacePlusName(t *testing.T) {
	a := Agent{WorkspaceID: "w9", Name: "project-manager-6", PaneID: "w9:p1"}
	k, ok := a.Key()
	if !ok || k != (Key{WorkspaceID: "w9", Name: "project-manager-6"}) {
		t.Errorf("Key = %+v (ok=%v)", k, ok)
	}
}

func TestStatusKnown(t *testing.T) {
	// The vocabulary is closed at five values; anything else is preserved verbatim
	// so it is visible rather than mapped on a guess.
	for _, tc := range []struct {
		in    Status
		known bool
	}{
		{StatusIdle, true},
		{StatusWorking, true},
		{StatusBlocked, true},
		{StatusDone, true},
		{StatusUnknown, true},
		{Status("compacting"), false},
		{Status(""), false},
	} {
		if got := tc.in.Known(); got != tc.known {
			t.Errorf("Status(%q).Known() = %v, want %v", tc.in, got, tc.known)
		}
	}
}

func TestParseAgentListStatusVocabulary(t *testing.T) {
	// Every status herdr can report must survive the decode verbatim. `unknown` in
	// particular must stay `unknown` — it is not a synonym for finished, and a
	// decoder that folded it into idle would erase the distinction before any
	// consumer got to see it. `done` likewise stays `done` here; the sticky-idle
	// reading of it belongs to whoever ranks demand, not to this decode.
	for _, tc := range []struct {
		status Status
		known  bool
	}{
		{StatusIdle, true},
		{StatusWorking, true},
		{StatusBlocked, true},
		{StatusDone, true},
		{StatusUnknown, true},
		{Status("something-new"), false},
	} {
		t.Run(string(tc.status), func(t *testing.T) {
			payload := `{"id":"cli:agent:list","result":{"agents":[{"agent":"claude","agent_status":"` +
				string(tc.status) + `","name":"a","pane_id":"w1:p1","workspace_id":"w1"}]}}`
			got, err := ParseAgentList([]byte(payload))
			if err != nil {
				t.Fatalf("ParseAgentList: %v", err)
			}
			if len(got) != 1 || got[0].Status != tc.status {
				t.Fatalf("status = %+v, want %q", got, tc.status)
			}
			if got[0].Status.Known() != tc.known {
				t.Errorf("Known() = %v, want %v", got[0].Status.Known(), tc.known)
			}
		})
	}
}

func TestParseAgentListDegradesToNoData(t *testing.T) {
	// A payload that IS a payload but reports nothing usable: an empty roster and
	// NOT an error. This runs once a second, and an error here would only teach
	// callers to ignore errors.
	for _, tc := range []struct{ name, payload string }{
		{"no result", `{"id":"cli:agent:list"}`},
		{"result without agents", `{"id":"cli:agent:list","result":{"type":"agent_list"}}`},
		{"agents at the top level", `{"agents":[{"agent":"claude","pane_id":"w1:p1"}]}`},
		{"agent with no pane id", `{"id":"cli:agent:list","result":{"agents":[{"agent":"claude","agent_status":"idle"}]}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseAgentList([]byte(tc.payload))
			if err != nil {
				t.Fatalf("want no error, got %v", err)
			}
			if len(got) != 0 {
				t.Errorf("want no agents, got %+v", got)
			}
		})
	}
}

// Bytes that are NOT a payload must be an ERROR, not an empty roster — the
// distinction the original table missed, and it is destructive rather than
// cosmetic. "Zero agents" is a meaningful answer here: it wipes every herd row off
// the bar and prunes the per-workspace freshness ratchet that the churn resistance
// depends on. A truncated or killed read must therefore look like a failure so the
// caller keeps the state it already had, rather than confidently reporting that the
// herds went away.
func TestParseAgentListRejectsNonPayloads(t *testing.T) {
	for _, tc := range []struct{ name, payload string }{
		{"garbage", `not json at all`},
		{"truncated mid-write", `{"id":"cli:agent:list","result":{"agents":[{"agent":"cl`},
		{"empty", ``},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseAgentList([]byte(tc.payload))
			if err == nil {
				t.Fatalf("want an error for a non-payload, got none (agents=%+v)", got)
			}
			if len(got) != 0 {
				t.Errorf("want no agents alongside the error, got %+v", got)
			}
		})
	}
}

func TestParseAgentListUnknownFieldsTolerated(t *testing.T) {
	// herdr documents state_labels{} and tokens{} that this 0.8.2 payload did not
	// carry; a future one adding them must not break the read.
	payload := `{"id":"cli:agent:list","result":{"agents":[{"agent":"claude","agent_status":"working",
		"name":"a","pane_id":"w1:p1","workspace_id":"w1","state_labels":{"x":"y"},"tokens":{"in":12},
		"future":[1,2,3]}],"type":"agent_list","extra":true},"meta":{"protocol":21}}`
	got, err := ParseAgentList([]byte(payload))
	if err != nil {
		t.Fatalf("ParseAgentList: %v", err)
	}
	if len(got) != 1 || got[0].Status != StatusWorking {
		t.Fatalf("got %+v", got)
	}
}

func TestParseAgentListServerError(t *testing.T) {
	// Verbatim from the live CLI with the socket pointed at nothing. herdr exits 1
	// AND writes this on stdout, which is why the exec seam keeps stdout.
	payload := `{"id":"cli:agent:list","error":{"code":"server_not_running","message":"no herdr server is running at /nonexistent/herdr.sock; run ` + "`herdr`" + ` to start or attach it"}}`
	got, err := ParseAgentList([]byte(payload))
	if len(got) != 0 {
		t.Errorf("want no agents, got %+v", got)
	}
	var se *ServerError
	if !errors.As(err, &se) {
		t.Fatalf("want a *ServerError, got %v", err)
	}
	if !se.NotRunning() {
		t.Errorf("code = %q, want it recognized as not-running", se.Code)
	}
	if se.Error() == "" {
		t.Error("ServerError must render something a human can read")
	}
}

func TestFingerprint(t *testing.T) {
	base, err := ParseAgentList([]byte(liveRoster))
	if err != nil {
		t.Fatalf("ParseAgentList: %v", err)
	}
	again, _ := ParseAgentList([]byte(liveRoster))
	if Fingerprint(base) != Fingerprint(again) {
		t.Fatal("an unchanged roster must fingerprint identically, or the tick rebuilds every second")
	}

	// Each of these is a change a caller must not miss. state_change_seq and
	// revision are herdr's own counters and carry most of it; the rest are folded
	// in against a change herdr makes without bumping a counter we watch.
	for _, tc := range []struct {
		name   string
		mutate func(a *Agent)
	}{
		{"status", func(a *Agent) { a.Status = StatusBlocked }},
		{"state_change_seq", func(a *Agent) { a.StateChangeSeq++ }},
		{"revision", func(a *Agent) { a.Revision++ }},
		{"name cleared", func(a *Agent) { a.Name = "" }},
		{"focus moved", func(a *Agent) { a.Focused = !a.Focused }},
		{"agent_session arrived", func(a *Agent) {
			a.HasSession = true
			a.Session = AgentSession{Source: "herdr:claude", Kind: "id", Value: "0b431acc-5ddd-4907-9701-e71f0613625c"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mutated := make([]Agent, len(base))
			copy(mutated, base)
			tc.mutate(&mutated[0])
			if Fingerprint(mutated) == Fingerprint(base) {
				t.Errorf("%s must change the fingerprint", tc.name)
			}
		})
	}

	// A pane leaving is a change too, and an empty roster must not fingerprint
	// like a one-agent one.
	if Fingerprint(base[:3]) == Fingerprint(base) {
		t.Error("losing an agent must change the fingerprint")
	}
	if Fingerprint(nil) != "" {
		t.Error("an empty roster should fingerprint empty")
	}
}

// stubRoster replaces the exec seam so no test ever spawns herdr.
func stubRoster(t *testing.T, out string, err error) {
	t.Helper()
	orig := runAgentList
	runAgentList = func(ctx context.Context) ([]byte, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("List must impose a deadline: a wedged socket cannot be allowed to stall the tick")
		}
		if err != nil {
			return nil, err
		}
		return []byte(out), nil
	}
	t.Cleanup(func() { runAgentList = orig })
}

func TestListUsesTheExecSeam(t *testing.T) {
	stubRoster(t, liveRoster, nil)
	got, err := List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d agents, want 4", len(got))
	}
}

func TestListPropagatesNotInstalled(t *testing.T) {
	// "no herdr on this machine" is a different reading from "herdr is asleep",
	// and a caller wants to tell them apart: one means never show herdr rows.
	stubRoster(t, "", ErrNotInstalled)
	got, err := List(context.Background())
	if !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("err = %v, want ErrNotInstalled", err)
	}
	if len(got) != 0 {
		t.Errorf("want no agents, got %+v", got)
	}
}
