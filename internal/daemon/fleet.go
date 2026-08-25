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
	"fmt"

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
