package daemon

// Background-agent tracking on the daemon's 1s tick.
//
// Two jobs, both deliberately narrow:
//
//  1. PERSIST IDENTITY. A background agent's kind, name, job id and owner come
//     from files only the daemon reads (the first-party status files, the job
//     records, /proc). The hook can learn kind/job/pid from its own environment,
//     but never the NAME — Claude Code does not export it to child processes — and
//     the name is what fleet notifications identify an agent by. So the daemon
//     fills the gaps, writing each value once, when it changes.
//
//  2. COMPUTE THE ROLL-UP. Who owns which agents, what they want, and how fresh a
//     session really is once its fleet counts. This feeds the TILE (see
//     tile.FleetViewFrom) — a desktop running the agent view shows its fleet instead
//     of its resting host session, and a desktop that spawned agents gains a second
//     row for them.
//
//  3. REMEMBER WHICH WINDOWS ARE CONSOLES. noteConsoles keeps a sticky set, because
//     the agent view retitles its terminal as you move through the list.
//
// The workspace-name glyphs are deliberately NOT fed: aggregate() is untouched and
// background rows, having no window, are still skipped there. (Those dots are dead
// configuration now — the tile is the live renderer.)
//
// All of it runs on the actor goroutine, on the same first-party read gc already
// does — no extra file reads per tick beyond one small mtime-gated JSON per live
// background agent.

import (
	"context"
	"fmt"
	"time"

	"github.com/mrzor/claude-status/internal/clauded"
	"github.com/mrzor/claude-status/internal/db"
	"github.com/mrzor/claude-status/internal/fleet"
	"github.com/mrzor/claude-status/internal/niri"
)

// shortID abbreviates a session id for log lines (the ids are UUIDs and the log is
// read by humans).
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// identityRecord is what we last persisted for a session, so a value is written
// once rather than on every tick. The DB would coalesce the writes anyway; this
// keeps them off the wire entirely.
type identityRecord struct {
	kind  string
	name  string
	jobID string
	owner string
}

// noteConsoles folds this tick's window titles into the sticky console set: a window
// seen running the agent view stays a console until the window itself goes away.
//
// Stickiness is the whole point. The view retitles its terminal as you move through
// the list, so a set derived from the current titles flips a console desktop back to
// describing its resting host session — "idle 22h" — the instant you scroll. Losing
// the marker also has to be cheap and unambiguous, which is why the only thing that
// clears it is the window closing.
//
// The price of that is a set of CANDIDATES, not of verdicts: a window keeps its
// marker after the console behind it dies, and kitty keeps the stale title to match.
// Whether a marker is still worth believing is a question about the host SESSION, not
// about the title, and it is answered where the console row is built — see
// tile.desktopRows and fleet.ConsoleHostLive. Deliberately not answered here: this
// function's contract is "has this window EVER been a console", and a marker that
// came back to life (the same window, a new console) must not have been forgotten in
// the meantime.
// noteHerdWindows remembers which niri window hosts each herd, so a herd row does
// not depend on one of its panes having a live session row at this instant.
//
// It has to be remembered rather than derived: a herdr pane's sessions are as short
// as whatever drives them, and when the last one with a workspace id ends, deriving
// the binding loses the herd entirely. Observed on the bar after deploying — the herd
// row appeared with a fresh pane's session and vanished when it ended, blinking every
// couple of minutes. Pruned only when the WINDOW goes away: a herd momentarily absent
// from herdr's roster simply has no group, which already yields no row, so forgetting
// the window as well would just make it rediscover the same binding a second later.
func (d *daemon) noteHerdWindows(windows []niri.Window) {
	live := make(map[int64]bool, len(windows))
	for _, w := range windows {
		live[int64(w.ID)] = true
	}
	for _, s := range d.sessions {
		if !s.HerdrWorkspaceID.Valid || s.HerdrWorkspaceID.String == "" || !s.WindowID.Valid {
			continue
		}
		if live[s.WindowID.Int64] {
			d.herdWins[s.HerdrWorkspaceID.String] = s.WindowID.Int64
		}
	}
	for ws, win := range d.herdWins {
		if !live[win] {
			delete(d.herdWins, ws)
		}
	}
}

func (d *daemon) noteConsoles(windows []niri.Window) {
	live := make(map[int64]bool, len(windows))
	for _, w := range windows {
		id := int64(w.ID)
		live[id] = true
		if fleet.IsConsoleTitle(w.Title) {
			d.consoleWins[id] = true
		}
	}
	for id := range d.consoleWins {
		if !live[id] {
			delete(d.consoleWins, id)
		}
	}
}

// tickFleet reads the background-job records, attributes them, persists the
// identity the hook could not see, and refreshes the roll-up. fp is the live
// first-party set gc already read this tick.
func (d *daemon) tickFleet(fp map[string]clauded.Session) {
	if d.fleetReader == nil || len(fp) == 0 {
		if len(d.fleetRollups) > 0 {
			d.fleetRollups = nil
			d.fleetChildren = nil
		}
		return
	}

	jobIDs := make([]string, 0, len(fp))
	livePIDs := make(map[int]bool, len(fp))
	for _, s := range fp {
		if s.JobID != "" {
			jobIDs = append(jobIDs, s.JobID)
		}
		if s.PID > 0 {
			livePIDs[s.PID] = true
		}
	}
	jobs := d.fleetReader.ReadJobs(jobIDs)

	now := db.Now()
	children := fleet.Attribute(d.sessions, fp, jobs, d.owners, now)
	rollups := fleet.Rollups(children)
	d.owners.Forget(livePIDs)

	d.logFleetChanges(rollups)
	d.fleetChildren = children
	d.fleetRollups = rollups
	d.persistIdentity(fp, children)
	d.pruneIdentity(fp)
}

// persistIdentity writes the identity fields the hook could not know. It is
// deliberately quiet: one narrow UPDATE per session per changed value, and nothing
// at all on a steady state.
func (d *daemon) persistIdentity(fp map[string]clauded.Session, children map[string]fleet.Child) {
	for id, s := range fp {
		want := identityRecord{
			kind:  string(s.Kind),
			name:  s.Name,
			jobID: s.JobID,
		}
		if c, ok := children[id]; ok {
			want.owner = c.OwnerSessionID
		}
		if have, seen := d.identified[id]; seen && have == want {
			continue
		}
		patch := db.Identity{}
		if want.kind != "" {
			k := want.kind
			patch.Kind = &k
		}
		if want.name != "" {
			n := want.name
			patch.SessionName = &n
		}
		if want.jobID != "" {
			j := want.jobID
			patch.JobID = &j
		}
		if want.owner != "" {
			o := want.owner
			patch.OwnerSessionID = &o
		}
		changed, err := d.db.SetIdentity(id, patch)
		if err != nil {
			logf("fleet identity %s: %v", shortID(id), err)
			continue
		}
		// Remember it either way: a row that does not exist yet (no hook has fired
		// for it) will be retried on the tick after its row appears, because the
		// first-party set is re-read every tick and `changed` stays false until
		// then.
		if changed {
			d.identified[id] = want
		}
	}
}

// pruneIdentity drops bookkeeping for sessions that are gone, so the map cannot
// grow without bound over a long-lived daemon.
func (d *daemon) pruneIdentity(fp map[string]clauded.Session) {
	for id := range d.identified {
		if _, ok := fp[id]; !ok {
			delete(d.identified, id)
		}
	}
}

// logFleetChanges records a line when what there is to say about a fleet actually
// changes, so the log reads as a history of "your agents started/stopped needing
// you" rather than a per-second dump.
func (d *daemon) logFleetChanges(next map[string]fleet.Rollup) {
	for id, r := range next {
		var line string
		if r.AwaitingHuman > 0 {
			// The name only, never the ask: a real `needs` is an entire
			// AskUserQuestion with its options (one live value ran ~600 characters),
			// and this fires per fleet on every change. The full text is not lost —
			// it is in the tile payload and in `claude-status fleet`.
			line = fmt.Sprintf("fleet %s: %d/%d agent(s) awaiting you (neediest: %s)",
				shortID(id), r.AwaitingHuman, r.Total, r.Neediest.Name)
		} else {
			line = fmt.Sprintf("fleet %s: %d live agent(s), worst=%s", shortID(id), r.Live, r.Worst)
		}
		// Dedupe on the LINE, not on the rollup: counts jitter between ticks in ways
		// this summary does not mention (a child flipping idle/working leaves the
		// sentence identical), and logging the same sentence every second buries the
		// changes that matter.
		if d.lastFleetLog[id] == line {
			continue
		}
		d.lastFleetLog[id] = line
		logf("%s", line)
	}
	for id := range d.lastFleetLog {
		if _, ok := next[id]; !ok {
			logf("fleet %s: no agents left", shortID(id))
			delete(d.lastFleetLog, id)
		}
	}
}

// herdrTickTimeout bounds the herdr roster call ON THE ACTOR GOROUTINE.
//
// Deliberately much shorter than herdr.CallTimeout (2s), which is the right bound
// for a CLI and the wrong one here: this goroutine also serves the niri event
// stream, the DB snapshots and the tile cache, so a wedged herdr socket must cost a
// fraction of one tick rather than two whole ones. A missed roster reading is
// invisible — the next tick asks again — while a stalled tick freezes every bar on
// the machine, which is the same trade herdr.CallTimeout documents, just with the
// tick's budget instead of a human's patience.
const herdrTickTimeout = 400 * time.Millisecond

// herdrMissThreshold is how many consecutive failed roster reads are tolerated
// before the herds are dropped. Mirrors firstPartyMissThreshold, and for the same
// reason: one bad read is noise, three in a row is a state worth believing.
const herdrMissThreshold = 3

// tickHerds refreshes the herd groups from herdr's live roster.
//
// SEPARATE FROM tickFleet, and not merged into it, because the two substrates fail
// independently: tickFleet returns early when there is no jobs directory or no live
// first-party session, and neither of those says anything about herdr. A herdr pane
// is an ORDINARY interactive session (no jobId, no CLAUDE_PID), so every one of
// them is invisible to the background-agent path by construction.
//
// This is the 1s tick and nothing else. The roster call is a process spawn, so it
// must never reach the 13ms DB poll — see the cost note in package herdr.
func (d *daemon) tickHerds(ctx context.Context, fp map[string]clauded.Session) {
	if d.herdr == nil {
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, herdrTickTimeout)
	defer cancel()

	groups, err := d.herdr.Groups(callCtx, fleet.Input{
		Rows: d.sessions,
		Live: fp,
		Now:  db.Now(),
	})
	if err != nil {
		// Benign by construction (see fleet.HerdrProvider.Groups): a herdr that is
		// asleep, or a socket that did not answer inside the tick's budget. Said
		// once per distinct message, because the alternative is one line a second
		// for as long as herdr is down.
		if msg := err.Error(); msg != d.herdrQuiet {
			d.herdrQuiet = msg
			logf("herdr roster: %s", msg)
		}
		// KEEP the last known herds for a few ticks rather than clearing on the
		// first failure. Clearing immediately made one flaky read equivalent to
		// "the herds went away": every herd row vanished from the bar, and the
		// per-workspace freshness ratchet was pruned with them — destroying exactly
		// the churn resistance this code exists to provide, for a read that a
		// second later succeeds again. A stale herd row is a far better answer than
		// no row, and the same 3-tick tolerance the first-party reap already uses
		// (firstPartyMissThreshold) bounds how long we will insist on it.
		d.herdrMiss++
		if d.herdrMiss >= herdrMissThreshold {
			d.herds = nil
		}
		return
	}
	d.herdrQuiet = ""
	d.herdrMiss = 0
	d.herds = groups
	d.persistHerdMembership(groups)
	d.logHerdChanges()
}

// persistHerdMembership writes a member's workspace back onto its session row.
//
// WHY IT IS NOT ENOUGH TO JOIN IN MEMORY. The hook records the pane from the pane's
// own environment, which only happens at SessionStart — so a pane whose session
// started before that hook shipped, or which simply runs for hours, never gets one.
// Observed on the bar: six live herdr panes on one window, five of them rendering as
// individual "active claude" rows instead of collapsing into their herds, three of
// them started that morning and therefore never going to restart on their own.
// Meanwhile herdr's roster names the session for two of the five outright.
//
// The tile keys its herd grouping on the ROW's workspace id, so a join the daemon
// makes only in memory cannot collapse anything. Writing it back is what closes that
// gap, and it also repairs the row over time: SetIdentity overwrites (unlike the
// hook's COALESCE), so a pane id that changed under a live session — they churn —
// gets corrected on the next tick instead of being wrong until the session ends.
func (d *daemon) persistHerdMembership(groups []fleet.Group) {
	if len(groups) == 0 {
		return
	}
	have := make(map[string]db.Session, len(d.sessions))
	for _, s := range d.sessions {
		have[s.SessionID] = s
	}
	// Windows already known to host a herd. A cwd join is only allowed to land in
	// one of these: it is what stops a match in some unrelated checkout elsewhere on
	// the machine from being adopted into a herd.
	herdWins := map[int64]bool{}
	for _, s := range d.sessions {
		if s.HerdrWorkspaceID.Valid && s.HerdrWorkspaceID.String != "" && s.WindowID.Valid {
			herdWins[s.WindowID.Int64] = true
		}
	}

	for _, g := range groups {
		if g.Key.Provider != fleet.ProviderHerdr {
			continue
		}
		ws := g.Key.Key
		for _, m := range joinByCwd(g, d.sessions, herdWins) {
			if _, err := d.db.SetIdentity(m.sessionID, m.patch); err != nil {
				logf("herd cwd join %s: %v", shortID(m.sessionID), err)
			}
		}
		for _, m := range g.Members {
			if m.SessionID == "" {
				continue
			}
			row, ok := have[m.SessionID]
			if !ok {
				continue // no row yet; the next tick will find it
			}
			patch := db.Identity{}
			if row.HerdrWorkspaceID.String != ws {
				patch.HerdrWorkspaceID = &ws
			}
			if m.Ref != "" && row.HerdrPaneID.String != m.Ref {
				ref := m.Ref
				patch.HerdrPaneID = &ref
			}
			if patch.HerdrWorkspaceID == nil && patch.HerdrPaneID == nil {
				continue // already agrees; the steady state writes nothing
			}
			if _, err := d.db.SetIdentity(m.SessionID, patch); err != nil {
				logf("herd membership %s: %v", shortID(m.SessionID), err)
			}
		}
	}
}

// logHerdChanges says something only when herdr's OWN change counters moved.
//
// That is the one place the roster fingerprint can be believed as "nothing happened":
// it is derived from herdr's `revision`/`state_change_seq` plus the cheap fields, so
// an unchanged fingerprint means no agent appeared, moved or changed status. It
// deliberately does NOT gate the group rebuild — the members' freshness comes from our
// own rows and advances while the roster is still — so this is the only work it skips.
func (d *daemon) logHerdChanges() {
	fp := d.herdr.Fingerprint()
	if fp == d.lastHerdFP {
		return
	}
	d.lastHerdFP = fp
	for _, g := range d.herds {
		sum := g.Summarize()
		logf("herd %s (%s): %d/%d agent(s) live, worst=%s", g.Label, g.Key.Key,
			sum.Live, sum.Total, sum.Worst)
	}
	if len(d.herds) == 0 {
		logf("herd: no herdr agents")
	}
}

// cwdJoin is one identity write the cwd fallback wants to make.
type cwdJoin struct {
	sessionID string
	patch     db.Identity
}

// joinByCwd claims the herd members that no exact key could reach.
//
// WHY A WEAK KEY IS STILL WORTH USING. herdr names a pane's Claude session only
// through its integration hook, which fires on SessionStart — so every pane whose
// occupant started before the integration was installed reports agent_session: null
// forever. Measured live 2026-08-25: window 33 hosted six herdr panes and only three
// rows carried a workspace id, so w9 and wA bound to no window at all and their two
// sessions leaked onto the bar as separate "active claude" rows beside the herd they
// belong to. Neither of the exact joins can ever fix those two, because neither key
// exists on either side.
//
// SO IT IS FENCED THREE WAYS, because cwd on its own is nearly meaningless — a dozen
// sessions share one checkout:
//
//  1. The candidate row must sit in a window that ALREADY hosts a herd. That is what
//     makes this a join within one herdr client rather than a machine-wide cwd search.
//  2. The row must not already carry a workspace id. Rows reached by a real key are
//     never overwritten by a guess.
//  3. The match must be unique on BOTH sides — exactly one nameless member with that
//     cwd, exactly one candidate row. w1 holds two agents in the same checkout, and
//     an ambiguous pair is left alone rather than assigned arbitrarily: a wrong herd
//     is worse than a stray row, since the stray at least tells the truth about
//     itself.
func joinByCwd(g fleet.Group, sessions []db.Session, herdWins map[int64]bool) []cwdJoin {
	if len(herdWins) == 0 {
		return nil // no herd is bound anywhere; there is nothing to join into
	}
	// Nameless members, counted per cwd so ambiguity is visible.
	want := map[string]int{}
	for _, m := range g.Members {
		if m.SessionID == "" && m.Cwd != "" {
			want[m.Cwd]++
		}
	}
	if len(want) == 0 {
		return nil
	}
	cand := map[string][]db.Session{}
	for _, s := range sessions {
		if s.HerdrWorkspaceID.Valid && s.HerdrWorkspaceID.String != "" {
			continue // already placed by a real key
		}
		if !s.WindowID.Valid || !herdWins[s.WindowID.Int64] {
			continue
		}
		if !s.Cwd.Valid || want[s.Cwd.String] == 0 {
			continue
		}
		cand[s.Cwd.String] = append(cand[s.Cwd.String], s)
	}

	ws := g.Key.Key
	out := []cwdJoin{}
	for _, m := range g.Members {
		if m.SessionID != "" || m.Cwd == "" {
			continue
		}
		if want[m.Cwd] != 1 || len(cand[m.Cwd]) != 1 {
			continue // ambiguous on one side or the other: leave it alone
		}
		row := cand[m.Cwd][0]
		patch := db.Identity{HerdrWorkspaceID: &ws}
		if m.Ref != "" {
			ref := m.Ref
			patch.HerdrPaneID = &ref
		}
		out = append(out, cwdJoin{sessionID: row.SessionID, patch: patch})
	}
	return out
}
