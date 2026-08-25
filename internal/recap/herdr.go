package recap

// Herdr attribution: naming a stretch of work after the AGENT that did it
// rather than after the terminal title that happened to be up when it died.
//
// herdr is a multiplexer: N Claude agents share one terminal window, so every
// pane in a herdr session reports the same window title (the FOCUSED pane's,
// fanned out to its siblings). Its agents are also short-lived — a colony tick
// runs 34-90s, median 81s, ~30 new session ids an hour on one window — and when
// a tick exits the only thing left on the row is that shared title. A day's
// recap therefore listed "herdr server stop", "~/taf/project-manager" and
// "ML-PF603D2Z: project-manager" as if they were topics, seven times out of
// twenty lines, and the same agent appeared as N unrelated mystery rows.
//
// IDENTITY. The stable key is (herdr workspace, agent name), which is what herdr
// itself treats as stable and what internal/herdr documents. Everything else on
// offer is worse:
//
//   - The public pane id (w1:p8V) CHURNS MID-SESSION — observed live, an agent
//     moved w1:pCD -> w1:pCH while its name stayed put — so it is not an
//     identity. It is also not a usable join key into session.json: the panes
//     there are keyed by herdr's INTERNAL number ("7") and the public number
//     lives in a separate public_pane_numbers map ("7" -> 283, rendered "8V"),
//     which internal/herdr deliberately does not expose. We carry the pane id on
//     the ref for diagnostics and never group by it.
//   - Claude Code's own session_name is per-run and churny: herdr called one
//     live session project-manager-keeper while Claude called it
//     project-manager-9a, and a different suffix after the next restart. It is
//     accepted only as a confirming match against a herdr pane name, never as
//     the name itself.
//   - ~/.claude/sessions/<pid>.json carries an `agent` field ("colony-keeper").
//     Evaluated and NOT used: it is the agent's ROLE, so it cannot tell two
//     keepers apart, and it is keyed by a live pid in a file that dies with the
//     process — exactly as perishable as the sessions row we already read, so it
//     buys no history. herdr's agent_name is both durable and more specific.
//
// GRACEFUL DEGRADATION IS A HARD REQUIREMENT. A session with no herdr ids — an
// ordinary interactive session, and every session that predates the columns —
// must produce byte-identical output. Resolve returns false for those, Build
// leaves them alone, and nothing here runs.

import (
	"sort"
	"time"

	"github.com/mrzor/claude-status/internal/db"
	"github.com/mrzor/claude-status/internal/herdr"
)

// Agent is the durable herdr identity behind a session. A nil *Agent on a
// Session means "not herdr-hosted", which is the overwhelmingly common case.
type Agent struct {
	// WorkspaceID is herdr's public workspace id ("w1", "wA"). Stable, and the
	// coarse half of the identity.
	WorkspaceID string
	// Workspace is the human-facing label for WorkspaceID (custom_name, else the
	// repo label, else the bare id — see herdr.Workspace.DisplayLabel).
	Workspace string
	// Name is herdr's durable agent name ("project-manager-keeper"). Empty when
	// herdr had no name for the pane (it clears the name when an occupant exits)
	// or when the workspace held several named agents and nothing tied this
	// session to one of them.
	Name string
	// PaneID is the public pane id the hook captured ("w1:p8V"), kept for
	// diagnostics only. It is NEVER part of the identity: it churns mid-session.
	PaneID string
}

// key is the aggregation key: ticks sharing it are one agent's work. Falling
// back to the workspace alone when Name is empty is deliberate — a workspace is
// the stable half, so an unnamed tick lands on its chamber rather than on a
// pane id that may already have been renumbered.
func (a Agent) key() string { return a.WorkspaceID + "\x00" + a.Name }

// Label is what to render for this agent, or "" when herdr told us nothing
// nameable (no name, and no label beyond the raw workspace id) — in which case
// the caller keeps whatever it had, since "w1" is no better than a title.
func (a Agent) Label() string {
	if a.Name != "" {
		return a.Name
	}
	if a.Workspace != "" && a.Workspace != a.WorkspaceID {
		return a.Workspace
	}
	return ""
}

// AgentLookup resolves a session id to the herdr agent that ran it. ok is false
// for any session not hosted by herdr, which makes the whole feature inert for
// ordinary sessions. A nil AgentLookup disables herdr attribution entirely.
type AgentLookup func(sessionID string) (Agent, bool)

// ResolveAgent derives the herdr identity of one session row from herdr's own
// session file. ok is false when the row carries no herdr workspace — the
// gate that keeps ordinary sessions on the pre-herdr path.
//
// The name ladder, strongest first:
//
//  1. The pane whose first-party bridge names THIS session id
//     (agent_session.kind == "id"). Exact, and the only rung that can name one
//     of several agents sharing a workspace.
//  2. The pane whose agent_name equals the session's own session_name. Weak on
//     its own (Claude's name is per-run) but an exact match is still a match.
//  3. The workspace's ONLY named agent. Unambiguous by construction, and the
//     rung that carries history: a colony chamber is one workspace holding one
//     agent, so a tick that died hours ago is still attributable even though
//     its pane number is long gone.
//
// Falling off the ladder is fine: the agent then identifies the workspace, and
// the row reads as its chamber instead of as a multiplexer title.
func ResolveAgent(s db.Session, ws map[string]herdr.Workspace) (Agent, bool) {
	if !s.HerdrWorkspaceID.Valid || s.HerdrWorkspaceID.String == "" {
		return Agent{}, false
	}
	a := Agent{WorkspaceID: s.HerdrWorkspaceID.String}
	if s.HerdrPaneID.Valid {
		a.PaneID = s.HerdrPaneID.String
	}
	a.Workspace = herdr.Label(ws, a.WorkspaceID)

	w, ok := ws[a.WorkspaceID]
	if !ok {
		return a, true // workspace unknown to session.json; the id is all we have
	}
	for _, p := range w.Panes {
		if p.AgentName != "" && p.HasSession && p.Session.Kind == "id" &&
			p.Session.Value == s.SessionID {
			a.Name = p.AgentName
			return a, true
		}
	}
	if s.SessionName.Valid {
		if p, ok := w.PaneByAgentName(s.SessionName.String); ok {
			a.Name = p.AgentName
			return a, true
		}
	}
	if name, only := soleAgentName(w); only {
		a.Name = name
	}
	return a, true
}

// soleAgentName returns the workspace's single named agent, and false when it
// has none or more than one. "More than one" must stay unnamed rather than
// guess: naming a tick after the wrong sibling is worse than naming it after
// its chamber.
func soleAgentName(w herdr.Workspace) (string, bool) {
	name := ""
	for _, p := range w.Panes {
		if p.AgentName == "" {
			continue // a plain shell pane
		}
		if name != "" && p.AgentName != name {
			return "", false
		}
		name = p.AgentName
	}
	return name, name != ""
}

// AgentLookupFor builds an AgentLookup over the current session rows and herdr's
// session file. Both inputs are best-effort: an empty rows slice or an empty
// workspace map yields a lookup that never resolves, which is precisely the
// pre-herdr behavior.
//
// KNOWN LIMIT, and it bounds what this can do today: a clean SessionEnd DELETES
// the sessions row (internal/hook), so a tick that exited tidily has no herdr
// ids left to read by the time recap runs. Attribution therefore covers the
// ticks whose rows survive — live agents, and anything that died without a
// SessionEnd. Retaining the ids for reaped sessions is a db/hook change and is
// deliberately out of this change's blast radius.
func AgentLookupFor(rows []db.Session, ws map[string]herdr.Workspace) AgentLookup {
	byID := make(map[string]Agent, len(rows))
	for _, s := range rows {
		if a, ok := ResolveAgent(s, ws); ok {
			byID[s.SessionID] = a
		}
	}
	if len(byID) == 0 {
		return nil // nothing herdr-hosted: keep Build on the untouched path
	}
	return func(sid string) (Agent, bool) {
		a, ok := byID[sid]
		return a, ok
	}
}

// tick is one session id's contribution plus the heartbeat windows behind its
// active time. The windows have to survive the per-session loop because a
// coalesced agent's active time is the UNION of its ticks' windows, not the sum
// of their durations — ticks of one agent can overlap (a pane handing over) and
// double-counting the overlap would inflate the agent's day.
type tick struct {
	s     Session
	beats []interval
	// last is the start of the latest tick folded in so far. Session.Started is
	// MINIMIZED as ticks merge, so it cannot answer "is this one newer?" — and
	// ticks reach coalesceAgents in event order, which is not guaranteed to be
	// chronological. Without this, an out-of-order middle tick would overwrite
	// the newest tick's id and branch.
	last time.Time
}

// coalesceAgents folds every tick of one herdr agent into a single Session and
// leaves every other tick exactly as it was, in its original order. A digest
// with no herdr sessions therefore comes out of here identical to its input —
// the property the regression guard pins.
func coalesceAgents(ticks []tick) []Session {
	out := make([]tick, 0, len(ticks))
	at := map[string]int{} // agent key -> index in out
	for _, t := range ticks {
		if t.s.Agent == nil {
			out = append(out, t)
			continue
		}
		k := t.s.Agent.key()
		if i, seen := at[k]; seen {
			out[i] = mergeTick(out[i], t)
			continue
		}
		at[k] = len(out)
		t.last = t.s.Started
		out = append(out, t)
	}
	sessions := make([]Session, len(out))
	for i, t := range out {
		sessions[i] = t.s
	}
	return sessions
}

// mergeTick absorbs a later tick of the same agent into the tick accumulated so
// far, keeping the union of their heartbeat windows so Active stays honest.
//
// The choices that are not obvious:
//   - ID becomes the LATEST tick's session id: that is the one a human could
//     still resume, and a dead tick's id is useless.
//   - Project/Dir/Branch take the latest tick's non-empty values — an agent that
//     moved onto a new branch mid-window should read as the branch it is on.
//   - Ask keeps the EARLIEST tick's opening ask: it is what the agent was set
//     going on, which is the thing worth reporting.
//   - Timeline is concatenated and re-sorted rather than coalesced across ticks.
//     Adjacent runs from different ticks are genuinely different sessions, and
//     merging them would invent a continuity that did not exist.
func mergeTick(acc, t tick) tick {
	a, n := acc.s, t.s
	later := !n.Started.Before(acc.last) // ties keep the incoming tick as "later"
	a.Ticks += n.Ticks
	if n.Started.Before(a.Started) {
		a.Started = n.Started
	}
	if n.Ended.After(a.Ended) {
		a.Ended = n.Ended
	}
	if later {
		acc.last = n.Started
		a.ID = n.ID
		if n.Project != "" && n.Project != "(unknown)" {
			a.Project = n.Project
		}
		if n.Dir != "" {
			a.Dir = n.Dir
		}
		if n.Branch != "" {
			a.Branch = n.Branch
		}
	}
	if a.Ask == "" {
		a.Ask = n.Ask
	}
	a.Arc = append(a.Arc, n.Arc...)
	a.Turns += n.Turns
	a.Prompts += n.Prompts
	a.Questions += n.Questions
	a.Working += n.Working
	a.WaitingUser += n.WaitingUser
	a.WaitingPermission += n.WaitingPermission
	a.Timeline = append(a.Timeline, n.Timeline...)
	sort.SliceStable(a.Timeline, func(i, j int) bool {
		return a.Timeline[i].Start.Before(a.Timeline[j].Start)
	})
	merged := make([]interval, 0, len(acc.beats)+len(t.beats))
	merged = append(append(merged, acc.beats...), t.beats...)
	a.Active = unionDur(merged)
	return tick{s: a, beats: merged, last: acc.last}
}
