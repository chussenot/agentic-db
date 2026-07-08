// Package recap turns the event log (effort: when Claude was working, how many
// turns, how many prompts) and the session transcripts (intent: topic, opening
// ask, project, branch) into a windowed digest suitable for a standup. Build is
// a pure function over injected events + a transcript lookup, so the whole
// aggregation is unit-testable without a DB or the filesystem; the subcommand
// wires the real sources in.
//
// Active time uses a heartbeat model: each event that occurs while a session is
// in the working state implies a bounded window of activity (heartbeatCap), and
// active time is the union of those windows. This is deliberately robust to the
// daemon's known "frozen working" glitch — a session that goes working and never
// records a Stop (missed-Stop / stalled turn) would otherwise look like hours of
// continuous work; here it contributes a single cap-length window.
package recap

import (
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/mrzor/claude-status/internal/db"
)

// heartbeatCap bounds how much active time a single working-state event implies.
// PostToolUse fires often during real work, so 5 minutes bridges thinking gaps
// while capping a frozen/stalled working state at one window rather than hours.
const heartbeatCap = 5 * time.Minute

// Meta is the transcript-derived content for a session (see internal/transcript).
type Meta struct {
	Title, Ask, Cwd, Branch string
	Arc                     []Turn // full conversational arc; untruncated
}

// Turn is one message in a session's arc: a genuine user prompt (Role "user")
// or the assistant's turn-ending message that preceded the next prompt (Role
// "assistant"). Text is untruncated. It mirrors transcript.Turn, kept local so
// recap stays free of the transcript dependency (Build is pure over injected
// data).
type Turn struct {
	Role string
	Text string
	At   time.Time
}

// Lookup resolves a session id to its transcript content. ok is false when the
// session has no transcript (still counted for effort, just without content).
type Lookup func(sessionID string) (Meta, bool)

// RepoLookup resolves a session id to its primary repo, captured at hook time and
// stored normalized (internal/db). ok is false for a session with no stored repo
// (an old/un-backfilled session, or a cwd that never resolved to a work tree), in
// which case Build falls back to the transcript-cwd heuristic. Returning the zero
// RepoLookup (nil) disables the DB path entirely — every session takes the
// fallback, which is the pre-normalization behavior.
type RepoLookup func(sessionID string) (db.RepoRef, bool)

// Digest is the full windowed recap. Sessions holds the top-N by active time in
// detail; sessions beyond that are summarized by MoreSessions / MoreProjects and
// still fold into Totals and Projects.
type Digest struct {
	From, To     time.Time
	Totals       Totals
	Streaks      []Streak
	Sessions     []Session
	MoreSessions int
	MoreProjects int
	Projects     []Project
}

// Totals are the window-wide roll-ups.
type Totals struct {
	Sessions int
	Active   time.Duration // total wall-clock (union across sessions; overlaps merged)
	// Per-session active-time distribution (each session's own active time; the
	// average is the mean of those, so Avg*Sessions can exceed Active when
	// sessions ran in parallel).
	MinActive time.Duration
	AvgActive time.Duration
	MaxActive time.Duration
	Turns     int
	Prompts   int // prompts I sent (UserPromptSubmit)
	Questions int // permission prompts Claude raised (new_state=='prompt')
	Projects  []string
	// Window-wide work-vs-wait sums (see Session's fields). Summed per session,
	// so unlike Active these do not merge overlaps across concurrent sessions.
	Working           time.Duration
	WaitingUser       time.Duration
	WaitingPermission time.Duration
}

// Session is one session's contribution within the window.
type Session struct {
	ID        string
	Topic     string
	Project   string
	Dir       string // working directory (used to locate the project's git repo)
	Branch    string
	Ask       string
	Started   time.Time
	Ended     time.Time
	Active    time.Duration
	Turns     int
	Prompts   int // prompts I sent (UserPromptSubmit)
	Questions int // permission prompts Claude raised
	// Arc is the full conversational arc (my prompts + Claude's turn-ending
	// summaries), untruncated; empty when the transcript is unavailable.
	Arc []Turn
	// Work-vs-wait breakdown, derived from the event log's gaps (see summarize):
	// Working = my prompt → Stop; WaitingUser = Stop → my next prompt; and
	// WaitingPermission = a permission prompt → the next activity. Gaps into a
	// SessionEnd are not attributed (the session ended, it wasn't waiting).
	Working           time.Duration
	WaitingUser       time.Duration
	WaitingPermission time.Duration
	// Timeline is the ordered span sequence the durations above sum from, with
	// runs of the same kind coalesced — the intra-day picture of the session.
	Timeline []Span
}

// SpanKind labels a stretch of a session's timeline.
type SpanKind string

const (
	SpanWorking  SpanKind = "working"            // Claude taking a turn
	SpanWaitUser SpanKind = "waiting_user"       // done, waiting on me to reply
	SpanWaitPerm SpanKind = "waiting_permission" // blocked on a permission prompt
)

// Span is one contiguous stretch of a session's timeline of a single kind.
type Span struct {
	Kind       SpanKind
	Start, End time.Time
}

// Streak is a contiguous span of cross-session activity within the window.
type Streak struct {
	Start, End time.Time
	Dur        time.Duration
}

// Project is a per-project roll-up over all in-window sessions. The git fields
// (Remote/Branch/Commits) are left zero by Build — it stays pure — and filled by
// the recap command via internal/git for the projects it displays.
type Project struct {
	Name string
	// Dirs are the distinct session cwds seen for this project (first-seen order).
	// Enrichment tries each until one resolves to a git repo, so a project worked
	// from a since-removed temp worktree still resolves via its surviving checkout.
	Dirs     []string
	HasRepo  bool // some Dir resolved to a live git work tree (set during enrichment)
	Remote   string
	Branch   string
	Commits  int
	Log      []Commit // the in-window commits behind Commits (newest first); "shipped X"
	Active   time.Duration
	Sessions int
}

// Commit mirrors git.Commit, kept local so recap's types don't leak the git
// dependency into consumers of the digest. Subject is untruncated.
type Commit struct {
	Hash    string
	Subject string
	At      time.Time
}

type interval struct{ start, end int64 } // unix ms

// Build aggregates events (any order; grouped internally) into a Digest for
// [from, to]. lookup supplies transcript content; repoLookup supplies the stored
// repo (nil disables it, falling back to the cwd heuristic for every session);
// topN caps the detailed session list. Effort stats are computed only from events
// already inside the window.
func Build(events []db.Event, lookup Lookup, repoLookup RepoLookup, from, to time.Time, topN int) Digest {
	fromMS, toMS := from.UnixMilli(), to.UnixMilli()

	// Group events per session, preserving ascending ts.
	order := []string{}
	bySession := map[string][]db.Event{}
	for _, e := range events {
		if e.TS < fromMS || e.TS > toMS {
			continue
		}
		if _, seen := bySession[e.SessionID]; !seen {
			order = append(order, e.SessionID)
		}
		bySession[e.SessionID] = append(bySession[e.SessionID], e)
	}
	// Ascending ts within each session (EventsBetween already orders, but Build
	// must not assume its caller does).
	for _, evs := range bySession {
		sort.SliceStable(evs, func(i, j int) bool { return evs[i].TS < evs[j].TS })
	}

	var allBeats []interval
	sessions := make([]Session, 0, len(order))
	for _, sid := range order {
		s, beats := summarize(bySession[sid], sid, toMS)
		if m, ok := lookup(sid); ok {
			if s.Topic == "" {
				s.Topic = m.Title
			}
			s.Ask = m.Ask
			s.Arc = m.Arc
			s.Branch = m.Branch
			s.Dir = m.Cwd
			s.Project = projectName(m.Cwd)
		}
		// Prefer the normalized repo stored at hook time: it groups sessions by the
		// actual repository (so two cwds sharing a remote collapse into one project),
		// where the transcript heuristic groups by cwd basename. Fall back to the
		// heuristic above when no repo was captured.
		if repoLookup != nil {
			if r, ok := repoLookup(sid); ok {
				s.Dir = r.Root // the git toplevel; recap runs git rev-list here
				s.Project = repoDisplayName(r)
				if r.Branch != "" {
					s.Branch = r.Branch
				}
			}
		}
		if s.Project == "" {
			s.Project = "(unknown)"
		}
		s.Active = unionDur(beats)
		// Skip sessions with no substance in the window — a bare resume ping or a
		// lone title change is not work and would only pad the counts.
		if s.Active == 0 && s.Turns == 0 && s.Questions == 0 && s.Prompts == 0 {
			continue
		}
		sessions = append(sessions, s)
		allBeats = append(allBeats, beats...)
	}

	// Totals + project roll-ups over every in-window session.
	tot := Totals{Sessions: len(sessions), Active: unionDur(allBeats)}
	projActive := map[string]time.Duration{}
	projCount := map[string]int{}
	projDirs := map[string][]string{}
	var sumActive time.Duration
	for i, s := range sessions {
		tot.Turns += s.Turns
		tot.Prompts += s.Prompts
		tot.Questions += s.Questions
		tot.Working += s.Working
		tot.WaitingUser += s.WaitingUser
		tot.WaitingPermission += s.WaitingPermission
		sumActive += s.Active
		if i == 0 || s.Active < tot.MinActive {
			tot.MinActive = s.Active
		}
		if s.Active > tot.MaxActive {
			tot.MaxActive = s.Active
		}
		projActive[s.Project] += s.Active
		projCount[s.Project]++
		if s.Dir != "" && !slices.Contains(projDirs[s.Project], s.Dir) {
			projDirs[s.Project] = append(projDirs[s.Project], s.Dir)
		}
	}
	if len(sessions) > 0 {
		tot.AvgActive = sumActive / time.Duration(len(sessions))
	}
	projects := make([]Project, 0, len(projActive))
	for name, act := range projActive {
		projects = append(projects, Project{Name: name, Dirs: projDirs[name], Active: act, Sessions: projCount[name]})
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].Active > projects[j].Active })
	for _, p := range projects {
		tot.Projects = append(tot.Projects, p.Name)
	}

	// Rank sessions by active time; detail the top-N, summarize the rest.
	sort.SliceStable(sessions, func(i, j int) bool { return sessions[i].Active > sessions[j].Active })
	more, moreProjects := 0, 0
	if topN > 0 && len(sessions) > topN {
		seen := map[string]bool{}
		for _, s := range sessions[topN:] {
			if !seen[s.Project] {
				seen[s.Project] = true
				moreProjects++
			}
		}
		more = len(sessions) - topN
		sessions = sessions[:topN]
	}

	return Digest{
		From: from, To: to, Totals: tot,
		Streaks:      topStreaks(allBeats, fromMS, toMS, 5),
		Sessions:     sessions,
		MoreSessions: more,
		MoreProjects: moreProjects,
		Projects:     projects,
	}
}

// summarize walks one session's (ascending) events, reconstructing the effective
// state to collect working heartbeats, turn (Stop) and question (prompt) counts,
// the in-window topic (latest TitleChanged), and the span. Heartbeat windows are
// clipped to toMS so a working state near the window edge can't spill past it.
func summarize(evs []db.Event, sid string, toMS int64) (Session, []interval) {
	s := Session{ID: sid}
	var beats []interval
	cur := ""
	for i, e := range evs {
		if i == 0 {
			s.Started = time.UnixMilli(e.TS)
		}
		s.Ended = time.UnixMilli(e.TS)
		switch e.NewState {
		case "working", "idle", "prompt", "shell":
			cur = e.NewState
		}
		if e.NewState == "prompt" {
			s.Questions++
		}
		if e.Event == "UserPromptSubmit" {
			s.Prompts++
		}
		if e.Event == "Stop" && e.NewState == "idle" {
			s.Turns++
		}
		if e.WindowTitle.Valid && e.WindowTitle.String != "" {
			s.Topic = e.WindowTitle.String // latest in-window title wins
		}
		if cur == "working" {
			// A working event covers until the next event, capped at heartbeatCap.
			// Clipping to the next event keeps a short turn (working -> Stop 2min
			// later) accurate; the cap bounds the LAST working event before a gap
			// (a real pause, or a frozen/missed-Stop turn) to one window instead of
			// letting it run to the session's end hours later.
			end := e.TS + heartbeatCap.Milliseconds()
			if i+1 < len(evs) && evs[i+1].TS < end {
				end = evs[i+1].TS
			}
			if end > toMS {
				end = toMS
			}
			if end > e.TS {
				beats = append(beats, interval{e.TS, end})
			}
		}
	}
	buildTimeline(&s, evs, toMS)
	return s, beats
}

// buildTimeline classifies the gap between each pair of consecutive events into
// a work-vs-wait Span and coalesces same-kind runs, then sums the per-kind
// totals onto s. The mode entered by event i governs the gap that follows it, so
// UserPromptSubmit→...→Stop is "working", Stop→next prompt is "waiting_user",
// and a permission prompt→next activity is "waiting_permission". A gap whose
// end is a SessionEnd is left unattributed: the session ended, it wasn't
// waiting. Sessions with no SessionEnd simply have no trailing event, so their
// dangling final gap is likewise never invented.
func buildTimeline(s *Session, evs []db.Event, toMS int64) {
	mode := SpanWaitUser // before the first prompt we're waiting on the user to type
	for i := range evs {
		mode = nextMode(evs[i], mode)
		if i+1 >= len(evs) || evs[i+1].Event == "SessionEnd" {
			continue
		}
		start, end := evs[i].TS, evs[i+1].TS
		if end > toMS {
			end = toMS
		}
		if end <= start {
			continue
		}
		if n := len(s.Timeline); n > 0 && s.Timeline[n-1].Kind == mode &&
			s.Timeline[n-1].End.UnixMilli() == start {
			s.Timeline[n-1].End = time.UnixMilli(end) // extend the current run
		} else {
			s.Timeline = append(s.Timeline, Span{Kind: mode, Start: time.UnixMilli(start), End: time.UnixMilli(end)})
		}
	}
	for _, sp := range s.Timeline {
		d := sp.End.Sub(sp.Start)
		switch sp.Kind {
		case SpanWorking:
			s.Working += d
		case SpanWaitUser:
			s.WaitingUser += d
		case SpanWaitPerm:
			s.WaitingPermission += d
		}
	}
}

// nextMode is the timeline state machine: the span kind in effect after event e,
// given the current kind. Neutral events (TitleChanged, SessionEnd, unknown)
// keep the current kind; a non-permission Notification (e.g. the idle nudge)
// likewise doesn't change what we're waiting on.
func nextMode(e db.Event, cur SpanKind) SpanKind {
	switch e.Event {
	case "UserPromptSubmit", "PostToolUse", "SubagentStop":
		return SpanWorking
	case "Stop", "SessionStart":
		return SpanWaitUser
	case "Notification":
		if e.NewState == "prompt" {
			return SpanWaitPerm
		}
		return cur
	default:
		return cur
	}
}

// unionDur returns the total duration covered by the union of intervals.
func unionDur(iv []interval) time.Duration {
	var ms int64
	for _, m := range merge(iv) {
		ms += m.end - m.start
	}
	return time.Duration(ms) * time.Millisecond
}

// merge coalesces overlapping/adjacent intervals; input need not be sorted.
func merge(iv []interval) []interval {
	if len(iv) == 0 {
		return nil
	}
	cp := append([]interval(nil), iv...)
	sort.Slice(cp, func(i, j int) bool { return cp[i].start < cp[j].start })
	out := []interval{cp[0]}
	for _, m := range cp[1:] {
		last := &out[len(out)-1]
		if m.start <= last.end {
			if m.end > last.end {
				last.end = m.end
			}
		} else {
			out = append(out, m)
		}
	}
	return out
}

// topStreaks merges all heartbeats into contiguous active spans, clips them to
// the window, and returns the n longest (descending).
func topStreaks(iv []interval, fromMS, toMS int64, n int) []Streak {
	var out []Streak
	for _, m := range merge(iv) {
		if m.start < fromMS {
			m.start = fromMS
		}
		if m.end > toMS {
			m.end = toMS
		}
		if m.end <= m.start {
			continue
		}
		out = append(out, Streak{
			Start: time.UnixMilli(m.start),
			End:   time.UnixMilli(m.end),
			Dur:   time.Duration(m.end-m.start) * time.Millisecond,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Dur > out[j].Dur })
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// projectName reduces a cwd to a short project label (its basename).
func projectName(cwd string) string {
	if cwd == "" {
		return ""
	}
	return filepath.Base(cwd)
}

// repoDisplayName is the short label for a stored repo: the last segment of the
// normalized remote (e.g. "gh/owner/repo" -> "repo"), else the basename of the
// git toplevel for a local-only repo, else "(unknown)".
func repoDisplayName(r db.RepoRef) string {
	if r.Remote != "" {
		return filepath.Base(r.Remote)
	}
	if r.Root != "" {
		return filepath.Base(r.Root)
	}
	return ""
}
