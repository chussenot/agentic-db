package fleet

// The herdr provider: one group per herdr WORKSPACE.
//
// This is the direction of dependency the whole layering exists for — fleet imports
// herdr, never the reverse. internal/herdr deliberately knows nothing of ours so
// that this file can exist; the status-to-Demand mapping it documents is therefore
// implemented here (DemandOfHerdrStatus).
//
// THREE MEASURED RULES SHAPE EVERY DECISION BELOW, all from live probes on
// 2026-08-25. They are stated here because each one has an obvious wrong answer that
// a reader will otherwise reach for.
//
//  1. THE AGENT SET COMES FROM THE ROSTER, NOT FROM session.json's PANES. Workspace
//     w1 reported 4 pane records in session.json but only 2 LIVE agents. Panes
//     outlive their occupants, so counting them inflates every bead strip. The
//     roster is the authority on which agents exist; session.json is for labels.
//
//  2. AN AGENT WITH NO CLAUDE SESSION IS STILL AN AGENT. A herdr pane between two
//     colony ticks hosts nothing at all. So our rows only ENRICH the roster's
//     members — they never decide who is a member.
//
//  3. A PANE ID IS NOT AN IDENTITY. One live agent's pane id moved w1:pCD ->
//     w1:pCH -> w1:pCJ across ~20 minutes while its name stayed
//     "project-manager-keeper". Group by workspace id (stable), identify an agent by
//     (workspace, name), and use the pane id only as a lookup key valid at this
//     instant.

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/mrzor/claude-status/internal/db"
	"github.com/mrzor/claude-status/internal/herdr"
)

// DemandOfHerdrStatus maps a herdr status onto the demand ladder. The mapping is
// specified on herdr.Status and implemented here because that package cannot import
// this one without closing an import cycle.
//
//	blocked -> DemandHuman    herdr's attention signal: a detected prompt
//	working -> DemandWorking
//	idle    -> DemandIdle
//	done    -> DemandIdle     see below
//	unknown -> DemandUnknown  its own rung, never "finished"
//
// `done` MAPPING TO IDLE IS THE NORMAL CASE, NOT AN EDGE CASE. herdr's `done` means
// "background work finished and nobody has LOOKED at it yet", and the flag is
// cleared only by the UI's seen-bookkeeping — a CLI read explicitly marks nothing
// seen. So a headless reader like this one sees agents park at `done` and STAY there:
// measured live, one helpdesk agent sat at `done` for many minutes with nothing
// happening, and that is the steady state for any agent we watch without opening
// herdr's own UI. It is the same at-rest state as idle, and an agent sitting there
// is ready for its next prompt — ranking it DemandDone would drop it out of the live
// count and invite a caller to retire it.
//
// Anything herdr adds later lands on DemandUnknown rather than being guessed at.
func DemandOfHerdrStatus(s herdr.Status) Demand {
	switch s {
	case herdr.StatusBlocked:
		return DemandHuman
	case herdr.StatusWorking:
		return DemandWorking
	case herdr.StatusIdle:
		return DemandIdle
	case herdr.StatusDone:
		return DemandIdle
	}
	return DemandUnknown
}

// herdrRetryAfter is how long the provider stays quiet after finding no herdr
// executable at all. Without a latch a daemon on a machine that does not use herdr
// would pay a PATH search every second forever; without the retry, installing herdr
// would require restarting the daemon. Only ErrNotInstalled latches — a herdr that
// is merely asleep is asked again next tick, because starting it is a keystroke.
const herdrRetryAfter = 5 * time.Minute

// HerdrProvider reads herdr's live roster and forms one group per workspace.
//
// It holds three pieces of state and each earns its keep:
//
//   - a mtime/size cache over session.json, so a steady session costs a stat
//     instead of a parse (the same shape Reader uses for job state files);
//   - a "no herdr here" latch with a retry, see herdrRetryAfter;
//   - the freshness HIGH-WATER MARK per group, which is the churn resistance this
//     bead is about. See freshness.
type HerdrProvider struct {
	// list is the roster call, a package seam so a test never spawns a process.
	list func(context.Context) ([]herdr.Agent, error)
	// sessionPath is herdr's session.json (labels and the durable pane records).
	sessionPath string

	// workspaces caches the parsed session.json, keyed on the file's mtime/size.
	workspaces map[string]herdr.Workspace
	wsMod      time.Time
	wsSize     int64
	wsRead     bool

	// quietUntil latches "no herdr on this machine" (see herdrRetryAfter).
	quietUntil time.Time

	// fingerprint is herdr's own change signal for the last roster read. See
	// Fingerprint for what it may and may not gate.
	fingerprint string

	// freshness is the per-group high-water mark of activity. See freshness.
	freshest map[string]time.Time
	// retryAfter is the latch duration; a field so a test does not have to wait
	// five minutes to observe the retry.
	retryAfter time.Duration
}

// NewHerdrProvider returns a provider reading the roster through herdr.List and
// labels from sessionPath (herdr.DefaultSessionPath when empty).
func NewHerdrProvider(sessionPath string) *HerdrProvider {
	if sessionPath == "" {
		sessionPath = herdr.DefaultSessionPath()
	}
	return &HerdrProvider{
		list:        herdr.List,
		sessionPath: sessionPath,
		freshest:    map[string]time.Time{},
		retryAfter:  herdrRetryAfter,
	}
}

// NewHerdrProviderFrom returns a provider whose roster comes from list instead of
// from a herdr process. The seam exists so that the daemon's wiring can be tested
// without a live herdr session on the machine running the test, and so that a
// payload captured from a bug report can be rendered through the real code path —
// the same reason ParseAgentList is exported.
func NewHerdrProviderFrom(list func(context.Context) ([]herdr.Agent, error), sessionPath string) *HerdrProvider {
	p := NewHerdrProvider(sessionPath)
	p.list = list
	return p
}

// ID reports the substrate.
func (p *HerdrProvider) ID() ProviderID { return ProviderHerdr }

// Fingerprint is herdr's own change signal for the last roster this provider read —
// its `revision`/`state_change_seq` counters folded together (see herdr.Fingerprint).
// An unchanged fingerprint means nothing herdr can see moved, which is what lets a
// caller skip talking about the herd at all.
//
// IT DELIBERATELY DOES NOT GATE THE GROUP BUILD, and that is worth stating because
// gating it is the obvious optimisation and it would be a bug: a member's Activity
// comes from OUR rows and the first-party status files, which advance while the
// roster sits perfectly still. Reusing last tick's groups on an unchanged
// fingerprint would freeze every herd's idle age at whatever it was when herdr last
// moved. The build is pure and costs a few dozen struct copies; the expensive parts
// — the process spawn and the session.json parse — are gated where they can be
// gated correctly.
func (p *HerdrProvider) Fingerprint() string { return p.fingerprint }

// Groups returns one group per herdr workspace that has at least one live agent.
//
// Errors are narrow and benign by construction: no herdr on PATH yields no groups
// and NO error (a machine that does not use herdr must never produce a diagnostic),
// while a herdr-side refusal yields *herdr.ServerError so that
// `claude-status fleet` can print a code a human can act on ("server_not_running" ->
// start herdr). The daemon ignores the error and shows no herd, which is the correct
// rendering in every one of those cases.
func (p *HerdrProvider) Groups(ctx context.Context, in Input) ([]Group, error) {
	if !p.quietUntil.IsZero() && in.Now.Before(p.quietUntil) {
		return nil, nil
	}
	agents, err := p.list(ctx)
	if err != nil {
		if errors.Is(err, herdr.ErrNotInstalled) {
			p.quietUntil = in.Now.Add(p.retryAfter)
			return nil, nil
		}
		return nil, err
	}
	p.quietUntil = time.Time{}
	p.fingerprint = herdr.Fingerprint(agents)
	ws := p.readWorkspaces()

	rowBySession, rowByPane := indexRows(in.Rows)

	byWorkspace := map[string][]Member{}
	for _, a := range agents {
		if a.WorkspaceID == "" {
			continue // nothing to group on; a roster entry this thin is not one we understand
		}
		byWorkspace[a.WorkspaceID] = append(byWorkspace[a.WorkspaceID],
			p.member(a, in, rowBySession, rowByPane))
	}

	out := make([]Group, 0, len(byWorkspace))
	for id, members := range byWorkspace {
		g := Group{
			Key:     GroupKey{Provider: ProviderHerdr, Key: id},
			Label:   herdr.Label(ws, id),
			Members: members,
		}
		g.Freshness = p.freshness(id, members)
		out = append(out, g)
	}
	p.pruneFreshness(byWorkspace)
	SortGroups(out)
	return out, nil
}

// member joins one roster entry to whatever we know about it.
//
// THE JOIN PRECEDENCE, and both rungs are live on this machine at the same time:
//
//  1. herdr's own `agent_session` — a real first-party session id, verified live to
//     resolve to one of our rows. Zero-hop, and the strongest signal there is. It is
//     ABSENT for any session that predates installing herdr's Claude integration,
//     which is not a migration to wait out: three of four live agents lacked it
//     while the fourth had it.
//  2. the herdr_pane_id column our hook writes from the pane's environment. Works
//     with no integration at all. The pane id is a LOOKUP key only — it churns under
//     a live agent (see rule 3 at the top of this file) — so it is read fresh from
//     this tick's roster entry and never remembered.
//
// Rung 1 is only taken when it names something we can actually see; otherwise it
// would join an agent to nothing and hide the pane column that would have worked.
// Deliberately NOT a third rung: session.json's persisted copy of agent_session.
// That record is DURABLE and keyed by pane, so it can still name a pane's PREVIOUS
// occupant, and joining a live agent's activity to a dead session's row is worse
// than not joining at all. Also deliberately not built: herdr's documented 2-hop
// `pane process-info -> pid -> ~/.claude/sessions/<pid>.json` bridge. Two rungs that
// cost nothing already cover both live shapes.
func (p *HerdrProvider) member(a herdr.Agent, in Input, rowBySession map[string]db.Session, rowByPane map[string]db.Session) Member {
	m := Member{Name: a.Name, Ref: a.PaneID, Cwd: a.Cwd, Demand: DemandOfHerdrStatus(a.Status)}

	sid := ""
	if a.HasSession && a.Session.Kind == "id" && a.Session.Value != "" {
		if _, ok := rowBySession[a.Session.Value]; ok {
			sid = a.Session.Value
		} else if _, ok := in.Live[a.Session.Value]; ok {
			sid = a.Session.Value
		}
	}
	if sid == "" {
		if row, ok := rowByPane[a.PaneID]; ok {
			sid = row.SessionID
		}
	}
	if sid == "" {
		// A pane with no Claude session of its own: normal between ticks, and still
		// a member. It contributes its herdr status and no activity at all.
		return m
	}
	m.SessionID = sid
	fp := in.Live[sid]
	m.Activity = SessionActivity(fp, rowBySession[sid])

	// herdr's reported status is the primary signal, per the epic's decision to
	// build on what herdr reports. The first-party file may only RAISE the demand to
	// DemandHuman, never lower it: a session whose own status file says it is
	// sitting on a permission prompt IS waiting on a person, whatever herdr's screen
	// detector concluded — the same precedence demandOf already applies to a
	// background agent.
	if fp.IsUserPrompt() {
		m.Demand = DemandHuman
		// The nearest thing herdr has to an ask. It is a wait REASON ("permission
		// prompt"), not a question, and NO `needs` string is invented here: herdr
		// has no equivalent of a background job's `needs`, and the one surface that
		// would answer it (`herdr agent read --source detection`) returns kilobytes
		// of screen scrape and must never be on a tick.
		m.Needs = fp.WaitingFor
	}
	return m
}

// indexRows builds the two lookups the join needs. Rows with no herdr pane are
// absent from the pane index, which is most of them.
//
// The pane index is keyed on the pane id ALONE because herdr's public pane ids carry
// their workspace ("w1:p8V"), so they are already workspace-qualified and cannot
// collide across herds.
func indexRows(rows []db.Session) (bySession, byPane map[string]db.Session) {
	bySession = make(map[string]db.Session, len(rows))
	byPane = map[string]db.Session{}
	for _, r := range rows {
		bySession[r.SessionID] = r
		if r.HerdrPaneID.Valid && r.HerdrPaneID.String != "" {
			byPane[r.HerdrPaneID.String] = r
		}
	}
	return bySession, byPane
}

// freshness is the group's churn-resistant "when did anything in this herd last
// move", and it is the point of the bead.
//
// It is the MAX over the group's members, RATCHETED so it can never go backwards
// while the group exists. Both halves are needed and each fixes a different lie:
//
//   - the MAX stops one member from speaking for the herd. Members churn at a rate
//     that has nothing to do with the herd — ~30 new session ids an hour behind one
//     window, median member lifetime 81s, 61 of 98 under two minutes — so a single
//     member's age is noise. Today's tile reads "idle 45m" for a herd with an agent
//     actively working in it, because it picked a row by lexicographic session uuid.
//   - the RATCHET stops a member DISAPPEARING from ageing the herd. A 34-second
//     colony tick ends, its row is reaped, and with it the only recent activity in
//     the group: a pure MAX would jump the herd's idle age backwards in time on the
//     very tick something finished. "When did anything here last move" is
//     monotonically non-decreasing for as long as the herd exists, so the mark is
//     kept until the group itself goes away (pruneFreshness).
func (p *HerdrProvider) freshness(workspaceID string, members []Member) time.Time {
	newest := p.freshest[workspaceID]
	for _, m := range members {
		if m.Activity.After(newest) {
			newest = m.Activity
		}
	}
	if !newest.IsZero() {
		p.freshest[workspaceID] = newest
	}
	return newest
}

// pruneFreshness forgets the marks of workspaces that no longer have agents, so a
// long-lived daemon cannot accumulate one entry per workspace ever seen — and so a
// workspace that comes back does so with an honest, empty history.
func (p *HerdrProvider) pruneFreshness(live map[string][]Member) {
	for id := range p.freshest {
		if _, ok := live[id]; !ok {
			delete(p.freshest, id)
		}
	}
}

// readWorkspaces returns the parsed session.json, re-parsing only when the file
// changed. Labels are the only thing taken from it — see rule 1 at the top of this
// file for why its pane records are NOT the agent set.
//
// A missing or unparseable file yields an empty map, which degrades labels to bare
// workspace ids ("w1") rather than taking a tick down. herdr rewrites this file in
// place, so a read can legitimately catch it mid-write.
func (p *HerdrProvider) readWorkspaces() map[string]herdr.Workspace {
	fi, err := os.Stat(p.sessionPath)
	if err != nil {
		p.workspaces, p.wsRead = nil, false
		return nil
	}
	if p.wsRead && p.wsMod.Equal(fi.ModTime()) && p.wsSize == fi.Size() {
		return p.workspaces
	}
	ws, err := herdr.ReadSession(p.sessionPath)
	if err != nil {
		p.workspaces, p.wsRead = nil, false
		return nil
	}
	p.workspaces, p.wsMod, p.wsSize, p.wsRead = ws, fi.ModTime(), fi.Size(), true
	return ws
}
