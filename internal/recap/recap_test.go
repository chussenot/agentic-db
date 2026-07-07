package recap

import (
	"database/sql"
	"testing"
	"time"

	"github.com/mrzor/claude-status/internal/db"
)

const min = int64(60_000) // one minute in ms

func ev(ts int64, sid, event, state string) db.Event {
	return db.Event{TS: ts, SessionID: sid, Event: event, NewState: state}
}
func titleEv(ts int64, sid, title string) db.Event {
	e := ev(ts, sid, "TitleChanged", "unchanged")
	e.WindowTitle = sql.NullString{String: title, Valid: true}
	return e
}

func noLookup(string) (Meta, bool) { return Meta{}, false }

func TestRepoLookupGroupsBySharedRemote(t *testing.T) {
	from := time.UnixMilli(0)
	to := time.UnixMilli(1000 * min)
	// Two sessions in DIFFERENT cwds (different basenames) but the SAME remote —
	// two worktrees of one repo. The transcript heuristic would split them into
	// two projects ("wt-a", "wt-b"); the stored repo must collapse them into one.
	events := []db.Event{
		ev(0, "A", "UserPromptSubmit", "working"),
		ev(1*min, "A", "Stop", "idle"),
		ev(0, "B", "UserPromptSubmit", "working"),
		ev(1*min, "B", "Stop", "idle"),
	}
	lookup := func(sid string) (Meta, bool) {
		switch sid {
		case "A":
			return Meta{Cwd: "/home/zor/wt-a", Branch: "main"}, true
		case "B":
			return Meta{Cwd: "/home/zor/wt-b", Branch: "feature"}, true
		}
		return Meta{}, false
	}
	repoLookup := func(sid string) (db.RepoRef, bool) {
		return db.RepoRef{Remote: "gh/owner/proj", Root: "/home/zor/wt-" + sid, Branch: "b-" + sid}, true
	}
	d := Build(events, lookup, repoLookup, from, to, 10)

	if len(d.Projects) != 1 {
		t.Fatalf("projects = %d, want 1 (both sessions collapse into the shared repo)", len(d.Projects))
	}
	if d.Projects[0].Name != "proj" {
		t.Errorf("project name = %q, want %q (last segment of the remote)", d.Projects[0].Name, "proj")
	}
	if d.Projects[0].Sessions != 2 {
		t.Errorf("project sessions = %d, want 2", d.Projects[0].Sessions)
	}
	// Branch comes from the stored repo, not the transcript.
	for _, s := range d.Sessions {
		if s.Branch != "b-"+s.ID {
			t.Errorf("session %s branch = %q, want %q (from repoLookup)", s.ID, s.Branch, "b-"+s.ID)
		}
	}
}

func TestRepoLookupFallsBackToCwd(t *testing.T) {
	from := time.UnixMilli(0)
	to := time.UnixMilli(1000 * min)
	events := []db.Event{
		ev(0, "A", "UserPromptSubmit", "working"),
		ev(1*min, "A", "Stop", "idle"),
	}
	lookup := func(string) (Meta, bool) {
		return Meta{Cwd: "/home/zor/legacy-proj", Branch: "main"}, true
	}
	// repoLookup returns false (an un-backfilled session): must fall back to the
	// transcript cwd basename, exactly as before normalization.
	noRepo := func(string) (db.RepoRef, bool) { return db.RepoRef{}, false }
	d := Build(events, lookup, noRepo, from, to, 10)
	if len(d.Projects) != 1 || d.Projects[0].Name != "legacy-proj" {
		t.Fatalf("want fallback project 'legacy-proj', got %+v", d.Projects)
	}
}

func TestBuildEffortAndWindowing(t *testing.T) {
	from := time.UnixMilli(0)
	to := time.UnixMilli(1000 * min)
	events := []db.Event{
		// Session A: a 2-minute working turn ending in Stop.
		ev(0, "A", "UserPromptSubmit", "working"),
		ev(1*min, "A", "PostToolUse", "unchanged"), // still working (carry-forward)
		ev(2*min, "A", "Stop", "idle"),             // turn/pause boundary
		// Session A: a permission prompt, answered 3 min later.
		ev(10*min, "A", "Notification", "prompt"),
		ev(13*min, "A", "PostToolUse", "working"),
		ev(14*min, "A", "Stop", "idle"),
		// Out-of-window event must be ignored entirely.
		ev(5000*min, "A", "PostToolUse", "working"),
	}
	d := Build(events, noLookup, nil, from, to, 10)

	if d.Totals.Sessions != 1 {
		t.Fatalf("sessions = %d, want 1 (out-of-window excluded)", d.Totals.Sessions)
	}
	s := d.Sessions[0]
	if s.Turns != 2 {
		t.Errorf("turns = %d, want 2 (two Stops)", s.Turns)
	}
	if s.Questions != 1 {
		t.Errorf("questions = %d, want 1 (one prompt entry)", s.Questions)
	}
	if s.Prompts != 1 || d.Totals.Prompts != 1 {
		t.Errorf("prompts = %d / total %d, want 1 (one UserPromptSubmit)", s.Prompts, d.Totals.Prompts)
	}
	// Active: turn1 [0,2min] + turn2 working [13,14min] = 3min exactly (next-event
	// clipping, no heartbeat spill).
	if s.Active != 3*time.Minute {
		t.Errorf("active = %s, want 3m", s.Active)
	}
}

func TestFrozenWorkingIsBounded(t *testing.T) {
	from := time.UnixMilli(0)
	to := time.UnixMilli(10_000 * min)
	// Session goes working and never records a Stop; last event is 3h before the
	// window ends. Without the cap this would count as hours of work.
	events := []db.Event{
		ev(0, "Z", "UserPromptSubmit", "working"),
		ev(1*min, "Z", "PostToolUse", "unchanged"),
		// ...then silence until far later (frozen). No further events.
	}
	d := Build(events, noLookup, nil, from, to, 10)
	if got := d.Sessions[0].Active; got > 6*time.Minute {
		t.Errorf("frozen working active = %s, want bounded ~5m by heartbeat cap", got)
	}
}

func TestStreaksMergeAcrossSessions(t *testing.T) {
	from := time.UnixMilli(0)
	to := time.UnixMilli(1000 * min)
	// Two sessions working over overlapping wall-clock -> one merged streak.
	events := []db.Event{
		ev(0, "A", "UserPromptSubmit", "working"),
		ev(1*min, "A", "PostToolUse", "unchanged"),
		ev(2*min, "A", "PostToolUse", "unchanged"),
		ev(1*min, "B", "UserPromptSubmit", "working"),
		ev(3*min, "B", "PostToolUse", "unchanged"),
	}
	d := Build(events, noLookup, nil, from, to, 10)
	if len(d.Streaks) != 1 {
		t.Fatalf("streaks = %d, want 1 merged span", len(d.Streaks))
	}
	// A covers [0,2], B covers [1,3] -> union [0, 3+cap-clip]. B's last event at
	// 3min has no successor, so it extends by the cap: [0, 8min].
	if d.Streaks[0].Dur != 8*time.Minute {
		t.Errorf("streak dur = %s, want 8m", d.Streaks[0].Dur)
	}
}

func TestInsubstantialSessionsDropped(t *testing.T) {
	from := time.UnixMilli(0)
	to := time.UnixMilli(1000 * min)
	events := []db.Event{
		// A real session.
		ev(0, "A", "UserPromptSubmit", "working"),
		ev(1*min, "A", "Stop", "idle"),
		// A bare resume: SessionStart + a title change, no work.
		ev(2*min, "B", "SessionStart", "idle"),
		titleEv(2*min, "B", "resumed but untouched"),
	}
	d := Build(events, noLookup, nil, from, to, 10)
	if d.Totals.Sessions != 1 || d.Sessions[0].ID != "A" {
		t.Fatalf("insubstantial session B should be dropped, got %+v", d.Sessions)
	}
}

func TestTopicPrefersInWindowTitle(t *testing.T) {
	from := time.UnixMilli(0)
	to := time.UnixMilli(1000 * min)
	events := []db.Event{
		ev(0, "A", "UserPromptSubmit", "working"),
		titleEv(1*min, "A", "First title"),
		titleEv(2*min, "A", "Refined title"), // latest in-window title wins
	}
	lookup := func(string) (Meta, bool) {
		return Meta{Title: "transcript fallback", Ask: "the ask", Cwd: "/home/zor/proj", Branch: "main"}, true
	}
	d := Build(events, lookup, nil, from, to, 10)
	s := d.Sessions[0]
	if s.Topic != "Refined title" {
		t.Errorf("topic = %q, want the latest in-window TitleChanged", s.Topic)
	}
	if s.Ask != "the ask" || s.Project != "proj" || s.Branch != "main" {
		t.Errorf("transcript enrichment wrong: %+v", s)
	}
	// The project rollup carries the session cwd(s) so git enrichment can find
	// the repo later.
	if len(d.Projects) != 1 || len(d.Projects[0].Dirs) != 1 || d.Projects[0].Dirs[0] != "/home/zor/proj" {
		t.Errorf("Project.Dirs = %+v, want [/home/zor/proj]", d.Projects)
	}
}

func TestTopNCapAndProjectRollup(t *testing.T) {
	from := time.UnixMilli(0)
	to := time.UnixMilli(1000 * min)
	var events []db.Event
	// 4 sessions with descending active time, across 3 projects.
	specs := []struct {
		sid  string
		dur  int64
		proj string
	}{
		{"S1", 8, "alpha"}, {"S2", 6, "alpha"}, {"S3", 4, "beta"}, {"S4", 2, "gamma"},
	}
	cwds := map[string]string{}
	for _, sp := range specs {
		events = append(events,
			ev(0, sp.sid, "UserPromptSubmit", "working"),
			ev(sp.dur*min, sp.sid, "Stop", "idle"))
		cwds[sp.sid] = "/x/" + sp.proj
	}
	lookup := func(sid string) (Meta, bool) { return Meta{Cwd: cwds[sid]}, true }
	d := Build(events, lookup, nil, from, to, 2) // detail only top 2

	if len(d.Sessions) != 2 || d.Sessions[0].ID != "S1" {
		t.Fatalf("expected top-2 detailed sessions led by S1, got %+v", d.Sessions)
	}
	if d.MoreSessions != 2 {
		t.Errorf("MoreSessions = %d, want 2", d.MoreSessions)
	}
	if d.MoreProjects != 2 { // beta + gamma
		t.Errorf("MoreProjects = %d, want 2", d.MoreProjects)
	}
	if d.Totals.Sessions != 4 {
		t.Errorf("Totals.Sessions = %d, want 4 (cap is display-only)", d.Totals.Sessions)
	}
	if len(d.Projects) != 3 || d.Projects[0].Name != "alpha" {
		t.Errorf("project rollup wrong: %+v", d.Projects)
	}
}
