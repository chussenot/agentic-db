package fleet

// The PROVIDER layer: what a substrate that hosts agents in GROUPS has to answer,
// so that everything downstream — the demand ladder, the bead strip, the tile row —
// is written once and fed twice.
//
// There are two substrates on this machine at the same time, and that is the
// permanent state, not a migration:
//
//   - claude-code: background sessions (`claude --bg`), the jobs/roster/CLAUDE_PID
//     path this package was originally written for. Still a Claude Code feature,
//     so it is kept rather than deleted.
//   - herdr: a terminal multiplexer hosting many INTERACTIVE Claude agents inside
//     one window. Every one of its agents is kind:interactive with no jobId and no
//     CLAUDE_PID, so the claude-code path drops all of them at its first line.
//
// WHY GROUPS AND NOT AGENTS. The renderers need one ROW per set of agents, and the
// two substrates disagree about what a set is: claude-code's set is "the agents one
// session spawned" (a session id), herdr's is "the agents in one workspace" (a
// workspace id). A provider therefore returns groups already formed, and nothing
// downstream has to know how the grouping was decided.
//
// GROUP IDENTITY IS (provider, key), AND THE KEY IS NEVER A SESSION ID OR A PANE ID.
// Both churn far faster than the thing a human is looking at: ~30 new Claude session
// ids an hour behind one herdr window, median tick lifetime 81s, 61 of 98 ticks under
// two minutes; and one live agent's PANE id walked w1:pCD -> w1:pCH -> w1:pCJ across
// ~20 minutes while its name never moved. See Member.Ref.
//
// DIRECTION OF DEPENDENCY: fleet imports herdr, never the reverse. internal/herdr
// deliberately depends on nothing of ours precisely so this compiles, which is why
// the herdr-status-to-Demand mapping is implemented HERE (DemandOfHerdrStatus) and
// only documented there.

import (
	"context"
	"sort"
	"time"

	"github.com/mrzor/claude-status/internal/clauded"
	"github.com/mrzor/claude-status/internal/db"
)

// ProviderID names a substrate. It is half of every group's identity, so that two
// providers can key a group on the same string without colliding.
type ProviderID string

const (
	// ProviderClaudeCode is Claude Code's own background agents.
	ProviderClaudeCode ProviderID = "claude-code"
	// ProviderHerdr is the herdr multiplexer.
	ProviderHerdr ProviderID = "herdr"
)

// GroupKey identifies one group of agents across ticks.
type GroupKey struct {
	Provider ProviderID
	// Key is the provider's own stable grouping key: a herdr workspace id ("w1"),
	// or the owning session id on the claude-code path. NEVER a pane id — see the
	// package note above.
	Key string
}

// Member is one agent inside a group, in provider-neutral terms. It is what the
// bead strip counts and what the third line names.
type Member struct {
	// Name is the agent's name within its group, and the only thing a human reads.
	// For herdr this is the `name` field, which is stable for the agent's whole
	// life; for claude-code it is the first-party session name, which is per-run.
	// May be EMPTY: herdr clears a pane's name when its occupant exits, and that
	// is a normal live reading, not corruption.
	Name string
	// Ref is the provider's own LOOKUP handle — a herdr pane id, a claude-code job
	// id. It is explicitly NOT identity: measured live 2026-08-25, one agent's pane
	// id moved w1:pCD -> w1:pCH -> w1:pCJ over ~20 minutes while its name stayed
	// "project-manager-keeper" throughout. Carried for diagnosis and for joins that
	// are valid at this instant, and for nothing that has to survive a tick.
	Ref string
	// SessionID is our session row for this agent, when the join found one. EMPTY
	// IS NORMAL AND EXPECTED: a herdr pane between two colony ticks hosts no Claude
	// session at all, and it is still an agent that must be counted. This is why
	// the member set comes from the provider and our rows only enrich it.
	SessionID string
	// Demand is what this agent wants, on the one ladder both providers share.
	Demand Demand
	// Needs is the verbatim ask when a human is being waited on, empty otherwise.
	// The claude-code path has a real one (a job's `needs`); herdr has NO
	// equivalent, so there it is at best a first-party wait REASON and usually
	// empty. Nothing here invents one.
	Needs string
	// Detail is the agent's own one-line summary, when its substrate publishes one.
	Detail string
	// Activity is the freshest evidence this agent did anything; the zero value
	// means "no evidence", which a group's freshness simply ignores.
	Activity time.Time
}

// Group is a set of agents that renders as ONE row.
type Group struct {
	Key GroupKey
	// Label is the name to show for the group: a herdr workspace's DisplayLabel, or
	// the owning session's name.
	Label string
	// Members is every agent in the group. Order is not promised here — Summarize
	// imposes the worst-first order the bead strip needs.
	Members []Member
	// Freshness is when ANYTHING in this group last moved, and it is the
	// churn-resistant number this whole layer exists to produce. See
	// Group.Summarize and HerdrProvider for how it is computed and why the MAX is
	// not enough on its own.
	Freshness time.Time
}

// Input is everything a provider may need that the CALLER has already read this
// tick. Passing it in rather than letting each provider read for itself is what
// keeps the 1s tick to one first-party read and one roster call.
type Input struct {
	// Rows is our DB's live sessions. They carry the persisted joins (the console
	// edge, the herdr pane/workspace columns) that cannot be derived.
	Rows []db.Session
	// Live is the first-party status set, already filtered to live processes.
	Live map[string]clauded.Session
	// Jobs is the background-job records, keyed by job id. claude-code only.
	Jobs map[string]Job
	// Spawn resolves a background process's spawner from /proc. claude-code only.
	Spawn *OwnerResolver
	// Now is the tick's clock, passed explicitly so every provider is a pure
	// function of its inputs.
	Now time.Time
}

// Provider is one substrate that hosts agents in groups.
//
// Groups returns what the substrate can see right now. An empty slice with a nil
// error is the NORMAL answer on a machine that does not use this substrate, and
// providers must not report that as an error: a per-second call that returns errors
// only teaches its caller to ignore them. The errors that ARE returned name
// something a human could act on, and are for `claude-status fleet` to print — the
// daemon ignores them by design.
type Provider interface {
	ID() ProviderID
	Groups(ctx context.Context, in Input) ([]Group, error)
}

// Summarize reduces a group to the renderer's shape — the same Summary the
// claude-code path produces through Rollups + Summarize, so the bead strip, the
// counts and the third line are written once.
//
// FRESHNESS IS THE MAX OVER MEMBERS, and that is the point of the bead. A herd's
// row must age on the newest thing in it, because its members churn underneath at a
// rate that has nothing to do with the herd: ~30 new session ids an hour on one
// window, median member lifetime 81s. Take any single member's age and a 34-second
// tick dying resets the herd's idle age, while a tick starting makes a three-hour-old
// herd look brand new — which is exactly today's bug, where a tile reads "idle 45m"
// for a herd with an agent actively working in it.
//
// The MAX alone is still not enough, because a member DISAPPEARING would make the
// herd look older than it is; Group.Freshness carries a per-group high-water mark
// across ticks and wins here when it is newer. See HerdrProvider.
func (g Group) Summarize() Summary {
	ms := make([]Member, len(g.Members))
	copy(ms, g.Members)
	SortMembers(ms)

	s := Summary{Total: len(ms)}
	newest := g.Freshness
	for _, m := range ms {
		switch m.Demand {
		case DemandHuman:
			s.Awaiting++
		case DemandWorking:
			s.Working++
		case DemandBlocked:
			s.Blocked++
		case DemandIdle:
			s.Idle++
		case DemandUnknown:
			s.Unknown++
		case DemandDone:
			s.Done++
		}
		if m.Demand != DemandDone {
			s.Live++
		}
		if m.Activity.After(newest) {
			newest = m.Activity
		}
		s.Demands = append(s.Demands, m.Demand)
	}
	if len(ms) > 0 {
		s.Worst = ms[0].Demand
		s.Neediest = ms[0].Name
		s.Needs = ms[0].Needs
	}
	if !newest.IsZero() {
		s.NewestActivity = newest.UnixMilli()
	}
	return s
}

// SortMembers imposes the total order the bead strip is drawn in: WORST FIRST, then
// freshest, then by name and finally by ref.
//
// Worst-first is a user decision, taken over "stable by agent": the alarm bead is
// always leftmost, and a bead's POSITION deliberately does not encode which agent it
// is. The remaining tie-breaks exist because the members are assembled out of maps
// and the tiles learned the hard way that a non-deterministic pick blinks — two
// renders of one unchanged group must produce the same bytes.
//
// Name before Ref, and never Ref alone: a pane id changes under a live agent (see
// Member.Ref), so ordering on it would reshuffle the strip mid-session.
func SortMembers(ms []Member) {
	sort.Slice(ms, func(i, j int) bool {
		if ms[i].Demand != ms[j].Demand {
			return ms[i].Demand < ms[j].Demand
		}
		if !ms[i].Activity.Equal(ms[j].Activity) {
			return ms[i].Activity.After(ms[j].Activity)
		}
		if ms[i].Name != ms[j].Name {
			return ms[i].Name < ms[j].Name
		}
		return ms[i].Ref < ms[j].Ref
	})
}

// GroupsByKey indexes groups from one provider by their key, so a renderer can ask
// "is there a herd for workspace w1" without scanning.
func GroupsByKey(groups []Group, id ProviderID) map[string]Group {
	out := make(map[string]Group, len(groups))
	for _, g := range groups {
		if g.Key.Provider == id {
			out[g.Key.Key] = g
		}
	}
	return out
}

// SortGroups orders groups deterministically: provider, then key. Same reason as
// everything else here — a stable order is what keeps a rebuild from blinking.
func SortGroups(gs []Group) {
	sort.Slice(gs, func(i, j int) bool {
		if gs[i].Key.Provider != gs[j].Key.Provider {
			return gs[i].Key.Provider < gs[j].Key.Provider
		}
		return gs[i].Key.Key < gs[j].Key.Key
	})
}
