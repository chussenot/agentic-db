package fleet

// The claude-code provider: Claude Code's OWN background agents, expressed as
// groups.
//
// This is an ADAPTER, not a second implementation. Everything it needs already
// exists and is well covered by tests — Attribute walks the three sources, Rollups
// groups the result by the session that owns it — so this file only restates that
// answer in the provider vocabulary. Nothing about the ladder, the attribution or
// the roll-up changed to make it fit.
//
// The substrate is kept rather than deleted on an explicit call: `claude --bg` is
// still a Claude Code feature, so a machine can have both substrates running, and on
// this one both are live at once.
//
// A GROUP HERE IS ONE OWNER SESSION'S CHILDREN, keyed on that owner's session id.
// That is a session id, which the provider layer forbids as a group key everywhere
// else — and it is the right key here for the reason the ban exists: the owner is an
// INTERACTIVE session a human is sitting at, not one of the churning agents. It is
// the same identity the tile already keys its owned fleet row on.
//
// The daemon still calls Attribute and Rollups directly rather than going through
// this type, because it needs the per-Child detail those return (pids, owner edges,
// job records) to persist identity — this view is deliberately the RENDERING view,
// which drops all of it. `claude-status fleet` uses the provider so that the two
// substrates print through one code path.

import "context"

// ClaudeCodeProvider turns background-job attribution into groups.
type ClaudeCodeProvider struct{}

// NewClaudeCodeProvider returns the provider. Stateless: everything it reads comes
// in through Input, and the caching that matters (job state files, /proc environs)
// already lives in Reader and OwnerResolver.
func NewClaudeCodeProvider() *ClaudeCodeProvider { return &ClaudeCodeProvider{} }

// ID reports the substrate.
func (p *ClaudeCodeProvider) ID() ProviderID { return ProviderClaudeCode }

// Groups returns one group per session that owns background agents.
//
// Never errors: every read behind it degrades to "no data" rather than failing, and
// a machine that has never run a background agent legitimately has nothing here.
func (p *ClaudeCodeProvider) Groups(_ context.Context, in Input) ([]Group, error) {
	children := Attribute(in.Rows, in.Live, in.Jobs, in.Spawn, in.Now)
	rollups := Rollups(children)
	out := make([]Group, 0, len(rollups))
	for owner, r := range rollups {
		g := Group{
			Key:     GroupKey{Provider: ProviderClaudeCode, Key: owner},
			Label:   in.Live[owner].Name,
			Members: make([]Member, 0, len(r.Children)),
		}
		if g.Label == "" {
			g.Label = "agents"
		}
		for _, c := range r.Children {
			g.Members = append(g.Members, Member{
				Name:      c.Name,
				Ref:       c.JobID,
				SessionID: c.SessionID,
				Demand:    c.Demand,
				Needs:     c.Needs,
				Detail:    c.Detail,
				Activity:  c.Activity,
			})
		}
		// A high-water mark is not needed here: Rollup already reports the newest
		// activity over the same children, and a background agent's record is not
		// recycled every 81 seconds the way a herdr pane's occupant is.
		g.Freshness = r.NewestActivity
		out = append(out, g)
	}
	SortGroups(out)
	return out, nil
}

// compile-time assertions that both providers satisfy the interface. Cheap, and it
// puts the failure at the definition rather than at a call site.
var (
	_ Provider = (*ClaudeCodeProvider)(nil)
	_ Provider = (*HerdrProvider)(nil)
)
