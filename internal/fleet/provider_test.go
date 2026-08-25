package fleet

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/mrzor/claude-status/internal/clauded"
)

func member(name string, d Demand, activity time.Time) Member {
	return Member{Name: name, Ref: "ref-" + name, Demand: d, Activity: activity}
}

// The bead strip is WORST FIRST — a user decision taken over stable-by-agent: the
// alarm bead is always leftmost and a bead's position does not encode which agent it
// is.
func TestGroupSummarizeIsWorstFirst(t *testing.T) {
	g := Group{Members: []Member{
		member("d", DemandDone, base),
		member("i", DemandIdle, base),
		member("u", DemandUnknown, base),
		member("h", DemandHuman, base),
		member("w", DemandWorking, base),
		member("b", DemandBlocked, base),
	}}
	sum := g.Summarize()
	want := []Demand{DemandHuman, DemandWorking, DemandBlocked, DemandIdle, DemandUnknown, DemandDone}
	if !reflect.DeepEqual(sum.Demands, want) {
		t.Errorf("bead order = %v, want %v", sum.Demands, want)
	}
	if sum.Worst != DemandHuman || sum.Neediest != "h" {
		t.Errorf("worst = %v via %q, want awaits-human via h", sum.Worst, sum.Neediest)
	}
	if sum.Total != 6 || sum.Live != 5 {
		t.Errorf("total/live = %d/%d, want 6/5 — only `done` leaves the live count", sum.Total, sum.Live)
	}
	if sum.Awaiting != 1 || sum.Working != 1 || sum.Blocked != 1 || sum.Idle != 1 || sum.Unknown != 1 || sum.Done != 1 {
		t.Errorf("counts do not add up: %+v", sum)
	}
}

// Two renders of one unchanged group must produce the same bytes: the members come
// out of a map, and the tiles learned the hard way that a non-deterministic pick
// blinks. The order must not depend on Ref, which churns under a live agent.
func TestSortMembersIsATotalOrderThatIgnoresRefChurn(t *testing.T) {
	older := base.Add(-time.Minute)
	build := func(refA, refB string) []Demand {
		g := Group{Members: []Member{
			{Name: "b", Ref: refB, Demand: DemandIdle, Activity: older},
			{Name: "a", Ref: refA, Demand: DemandIdle, Activity: base},
		}}
		out := make([]Demand, 0, 2)
		ms := make([]Member, len(g.Members))
		copy(ms, g.Members)
		SortMembers(ms)
		for _, m := range ms {
			out = append(out, m.Demand)
		}
		return out
	}
	if !reflect.DeepEqual(build("w1:pCD", "w1:pAA"), build("w1:pCJ", "w1:pZZ")) {
		t.Error("order changed when only the pane ids churned")
	}

	// Freshest first inside one demand rung.
	ms := []Member{
		{Name: "stale", Demand: DemandIdle, Activity: older},
		{Name: "fresh", Demand: DemandIdle, Activity: base},
	}
	SortMembers(ms)
	if ms[0].Name != "fresh" {
		t.Errorf("within a rung the freshest comes first, got %q", ms[0].Name)
	}
}

// An empty group is a zero Summary a caller can test with Empty().
func TestEmptyGroupSummarizesToNothing(t *testing.T) {
	sum := Group{Key: GroupKey{Provider: ProviderHerdr, Key: "w1"}}.Summarize()
	if !sum.Empty() || sum.NewestActivity != 0 || len(sum.Demands) != 0 {
		t.Errorf("empty group summarized to %+v", sum)
	}
}

// A group's Freshness floor wins over the members when it is newer — that is the
// ratchet the herdr provider maintains, and Summarize has to honour it.
func TestGroupFreshnessFloorWinsOverMembers(t *testing.T) {
	sum := Group{
		Freshness: base,
		Members:   []Member{member("a", DemandIdle, base.Add(-time.Hour))},
	}.Summarize()
	if got := time.UnixMilli(sum.NewestActivity); !got.Equal(base) {
		t.Errorf("freshness = %v, want the group's mark %v", got, base)
	}
}

// The other half: a member NEWER than the mark carries the group forward. Between
// this and the test above, the group's age is max(mark, newest member) — never one
// member's own age, which is noise at a median member lifetime of 81 seconds.
func TestGroupSummarizeTakesTheMaxOverMembers(t *testing.T) {
	fresh := base
	sum := Group{
		Freshness: base.Add(-time.Hour),
		Members: []Member{
			member("elder", DemandIdle, base.Add(-3*time.Hour)),
			member("tick", DemandWorking, fresh),
		},
	}.Summarize()
	if got := time.UnixMilli(sum.NewestActivity); !got.Equal(fresh) {
		t.Errorf("group age = %v, want the newest member %v", got, fresh)
	}
}

// The claude-code provider is an ADAPTER over Attribute+Rollups: one group per
// OWNER session. That key is a session id, which the layer forbids everywhere else —
// and it is right here because the owner is the interactive session a human sits at,
// not one of the churning agents.
func TestClaudeCodeProviderGroupsOneGroupPerOwner(t *testing.T) {
	live := map[string]clauded.Session{
		"owner": interactiveSession("owner", "console", 10, "/repo"),
		"kid1":  bgSession("kid1", "pm-3", "j1", 11, clauded.Waiting, "permission prompt"),
		"kid2":  bgSession("kid2", "pm-5", "j2", 12, clauded.Busy, ""),
	}
	stubEnviron(t, map[int][]string{
		11: {"CLAUDE_CODE_SESSION_KIND=bg", "CLAUDE_PID=10"},
		12: {"CLAUDE_CODE_SESSION_KIND=bg", "CLAUDE_PID=10"},
	})

	gs, err := NewClaudeCodeProvider().Groups(context.Background(), Input{
		Live: live, Spawn: NewOwnerResolver(), Now: base,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(gs) != 1 {
		t.Fatalf("want one group for the one owner, got %+v", gs)
	}
	g := gs[0]
	if g.Key.Provider != ProviderClaudeCode || g.Key.Key != "owner" {
		t.Errorf("key = %+v, want the owning session id under claude-code", g.Key)
	}
	if g.Label != "console" {
		t.Errorf("label = %q, want the owner's name", g.Label)
	}
	sum := g.Summarize()
	if sum.Total != 2 || sum.Awaiting != 1 {
		t.Errorf("summary = %+v, want 2 agents with one awaiting", sum)
	}
	if sum.Unknown != 0 {
		t.Error("the claude-code path has no unknown rung")
	}
	// The two substrates must be tellable apart by key alone.
	if _, ok := GroupsByKey(gs, ProviderHerdr)["owner"]; ok {
		t.Error("a claude-code group answered to a herdr lookup")
	}
	if _, ok := GroupsByKey(gs, ProviderClaudeCode)["owner"]; !ok {
		t.Error("GroupsByKey did not index the group under its own provider")
	}
}

// Groups from both providers land in one slice, so the order has to be total across
// providers too.
func TestSortGroupsOrdersByProviderThenKey(t *testing.T) {
	gs := []Group{
		{Key: GroupKey{ProviderHerdr, "w9"}},
		{Key: GroupKey{ProviderClaudeCode, "s2"}},
		{Key: GroupKey{ProviderHerdr, "w1"}},
		{Key: GroupKey{ProviderClaudeCode, "s1"}},
	}
	SortGroups(gs)
	var got []string
	for _, g := range gs {
		got = append(got, string(g.Key.Provider)+"/"+g.Key.Key)
	}
	want := []string{"claude-code/s1", "claude-code/s2", "herdr/w1", "herdr/w9"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}
