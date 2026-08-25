package fleet

// Fleet CONSOLES: a terminal running `claude agents` (the agent view) rather than a
// conversation.
//
// Why this needs its own signal. A console desktop is where you go to answer your
// agents, so it is the desktop that most needs to show the fleet — and it is exactly
// the desktop the tracker described worst. The session hosting the view is a real
// session that happens to be resting: its own last turn can be a day old and its
// first-party status file freezes, so the tile read "idle 22h" on a desktop actively
// displaying eleven working agents.
//
// Neither ownership edge catches it. The console is usually NOT the session that
// spawned the fleet (verified: a fleet spawned from one desktop was being watched
// from another), and the notification edge moves — Claude Code delivers a fleet
// notification to whichever session is frontmost, so over one afternoon the same
// fleet's notifications landed on two different sessions and on the agents
// themselves.
//
// The window TITLE is the stable signal, because the agent view publishes its own
// state there: it sets the terminal title to "<N> awaiting input · claude agents"
// and, while you move through the list, to the focused agent's name. Across an
// afternoon that title appeared 106 times and on exactly the two sessions that had
// ever opened the view. The daemon already tracks per-window titles, so this costs
// nothing new.
//
// The view is machine-wide (it lists every session, like `claude agents --json`), so
// a console desktop shows the WHOLE fleet, not one session's children.

import (
	"strings"
	"time"
	"unicode/utf8"
)

// consoleTitleMark is what the agent view puts in the terminal title. Matching the
// suffix rather than the count keeps it stable as agents come and go.
const consoleTitleMark = "claude agents"

// IsConsoleTitle reports whether a window title says the window is running the agent
// view. ONLY the view's aggregate line counts, e.g. "6 awaiting input · claude
// agents" — a string no conversation produces.
//
// A second form was tried and REVERTED: matching the focused agent's name, since the
// view retitles the terminal as you move through the list. It fails twice over.
// It flaps — observed live, two windows flipping between "console" and "ordinary
// owner" between one tile rebuild and the next, because the title had moved on. And
// it false-positives: a conversation whose ai-title happens to match an agent's name
// would be misread as a dashboard. The flicker that the name form was meant to cover
// (the title leaves the aggregate form while you navigate) is handled properly by
// remembering which windows have BEEN a console — see the daemon's sticky set —
// rather than by widening what counts as one.
func IsConsoleTitle(title string) bool {
	t := normalizeTitle(title)
	if t == "" {
		return false
	}
	return strings.Contains(strings.ToLower(t), consoleTitleMark)
}

// consoleHostSilence is how long a console window's host session may go without
// talking before its "claude agents" title stops being believed.
//
// A NUMBER, BECAUSE NEITHER SIDE OF THE QUESTION HAS A CRISPER SIGNAL. The title is
// set by the agent view and never unset, and the daemon's marker is sticky per
// window, so both outlive the session that earned them. Process liveness does NOT
// separate the two cases and was REJECTED as the gate: measured 2026-08-25, the two
// fossil windows (niri ids 12 and 34, both workspace 1) still had LIVE `claude
// agents` processes — pids 1916662 and 2554350, up 7d21h — whose sessions had never
// taken a turn since starting 188h and 187h earlier. Their kitty windows, their
// claude processes and their titles were all present; only the point of them was
// gone.
//
// So the limit is bounded by measurement on both sides. A genuine console's own
// session goes quiet WHILE the view is open — 22h observed live, and that reading is
// the entire reason this mechanism exists — so the limit cannot be tight. The
// fossils sat at 187-188h. 72h is ~3x above the observed genuine case and ~2.5x
// below the fossils; a console nobody has typed into for three days is not a
// dashboard anybody is reading.
const consoleHostSilence = 72 * time.Hour

// ConsoleHostLive reports whether the session hosting a console window is still live
// enough for that window's title to mean anything. lastTalk is the host session's
// last_talk_ts; a zero value is BELIEVED (a session that has not talked yet is not a
// fossil — absence of evidence is not evidence of death, the same way the daemon's
// gc treats an unrecorded pid).
//
// This is deliberately NOT about the title: a live console retitles itself as you
// scroll and must keep its marker across that (see the daemon's noteConsoles). The
// question asked here is whether anything is still alive behind the marker.
func ConsoleHostLive(lastTalk, now time.Time) bool {
	if lastTalk.IsZero() {
		return true
	}
	return now.Sub(lastTalk) < consoleHostSilence
}

// normalizeTitle strips Claude Code's leading status glyph (the Braille spinner, ✳,
// ⠐ and friends) and surrounding space, so a title is compared by its text. It
// mirrors the daemon's own title normalization — a spinner frame must not change what
// a title means.
func normalizeTitle(title string) string {
	t := strings.TrimSpace(title)
	for t != "" {
		r, size := utf8.DecodeRuneInString(t)
		if !isStatusGlyph(r) {
			break
		}
		t = strings.TrimSpace(t[size:])
	}
	return t
}

// isStatusGlyph reports whether r is one of the decorative markers Claude Code
// prefixes a title with: Braille spinner frames and the ✳/✻ family.
func isStatusGlyph(r rune) bool {
	switch {
	case r >= 0x2800 && r <= 0x28FF: // Braille patterns (spinner frames)
		return true
	case r >= 0x2731 && r <= 0x273F: // ✱ ✳ ✻ heavy asterisks
		return true
	}
	return false
}

// Summary is a fleet reduced to what a renderer needs: the counts, one demand per
// agent for the bead strip, and the neediest agent's name and verbatim ask.
//
// Deliberately NOT the Child values themselves: a renderer has no business with
// pids, job paths or cwds, and this shape is what crosses into the tile payload.
type Summary struct {
	Total    int
	Live     int
	Awaiting int
	Working  int
	Blocked  int
	Idle     int
	// Unknown counts agents present but unclassifiable — see DemandUnknown. Always
	// zero on the claude-code path, which has no such rung.
	Unknown int
	Done    int
	// Demands is one entry per agent, worst first — the bead strip, in order.
	Demands []Demand
	// Neediest names the agent that set the worst demand; Needs is its ask,
	// verbatim, empty when nothing is waiting on a human.
	Neediest string
	Needs    string
	// Worst is the fleet's highest-precedence demand.
	Worst Demand
	// NewestActivity is the freshest evidence any live agent did anything — the
	// number a console desktop should age on, instead of its resting session's
	// last turn.
	NewestActivity int64 // unix ms, 0 when unknown
}

// Summarize reduces a Rollup for a renderer. The zero Rollup yields a zero Summary,
// which callers can test with Empty.
func Summarize(r Rollup) Summary {
	s := Summary{
		Total: r.Total, Live: r.Live,
		Awaiting: r.AwaitingHuman, Working: r.Working, Blocked: r.Blocked,
		Idle: r.Idle, Done: r.Done,
		Neediest: r.Neediest.Name, Needs: r.Neediest.Needs,
		Worst: r.Worst,
	}
	if !r.NewestActivity.IsZero() {
		s.NewestActivity = r.NewestActivity.UnixMilli()
	}
	// Children are already sorted worst-first by Rollups.
	for _, c := range r.Children {
		s.Demands = append(s.Demands, c.Demand)
	}
	return s
}

// Empty reports whether there is no fleet to show.
func (s Summary) Empty() bool { return s.Total == 0 }

// WholeFleet summarizes EVERY live agent on the machine, which is what a console
// desktop displays (the agent view is machine-wide, not per-owner).
func WholeFleet(children map[string]Child) Summary {
	all := make([]Child, 0, len(children))
	for _, c := range children {
		all = append(all, c)
	}
	return Summarize(summarize("", all))
}
