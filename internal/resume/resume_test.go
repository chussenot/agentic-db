package resume

import (
	"strings"
	"testing"
	"time"

	"github.com/mrzor/claude-status/internal/db"
	"github.com/mrzor/claude-status/internal/transcript"
)

// fixedNow is a deterministic clock for relative-age assertions.
var fixedNow = time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)

func ms(t time.Time) int64 { return t.UnixMilli() }

// buildLookups turns simple maps into the injected lookups selectCandidates wants.
func lookups(meta map[string]transcript.Info, refs map[string]db.RepoRef) (metaLookup, repoRefLookup) {
	return func(sid string) (transcript.Info, bool) {
			i, ok := meta[sid]
			return i, ok
		}, func(sid string) (db.RepoRef, bool) {
			r, ok := refs[sid]
			return r, ok
		}
}

func TestSelectCandidates(t *testing.T) {
	recent := []db.RecentSession{
		{SessionID: "live", LastTS: ms(fixedNow.Add(-2 * time.Minute))},
		{SessionID: "ok", LastTS: ms(fixedNow.Add(-1 * time.Hour))},
		{SessionID: "nocwd", LastTS: ms(fixedNow.Add(-2 * time.Hour))},
		{SessionID: "rootonly", LastTS: ms(fixedNow.Add(-3 * time.Hour))},
	}
	meta := map[string]transcript.Info{
		"live": {Cwd: "/w/live", Title: "live one"},
		"ok":   {Cwd: "/w/ok", Title: "ok topic", Branch: "feat"},
		// "nocwd": no transcript, and no repo root below -> unresurrectable.
		// "rootonly": no transcript; cwd must fall back to repo root.
	}
	refs := map[string]db.RepoRef{
		"ok":       {Remote: "gh/o/ok", Root: "/w/ok", Branch: "main"},
		"rootonly": {Root: "/w/rootonly"},
	}
	m, r := lookups(meta, refs)
	live := map[string]bool{"live": true}

	got, d := selectCandidates(recent, m, r, live, selectConfig{limit: 20})

	if len(got) != 2 {
		t.Fatalf("kept %d candidates, want 2 (%+v)", len(got), got)
	}
	if d.live != 1 {
		t.Errorf("drops.live = %d, want 1", d.live)
	}
	if d.noCwd != 1 {
		t.Errorf("drops.noCwd = %d, want 1", d.noCwd)
	}
	// Order preserved (newest-first as given), live excluded.
	if got[0].SessionID != "ok" || got[1].SessionID != "rootonly" {
		t.Fatalf("order = [%s %s], want [ok rootonly]", got[0].SessionID, got[1].SessionID)
	}
	// "ok": repo label from remote, branch from ref (ref wins over transcript).
	if got[0].Repo != "gh/o/ok" || got[0].Branch != "main" {
		t.Errorf("ok repo/branch = %q/%q, want gh/o/ok/main", got[0].Repo, got[0].Branch)
	}
	// "rootonly": cwd falls back to the repo root; label is the root (no remote).
	if got[1].Cwd != "/w/rootonly" || got[1].Repo != "/w/rootonly" {
		t.Errorf("rootonly cwd/repo = %q/%q, want /w/rootonly", got[1].Cwd, got[1].Repo)
	}
}

func TestSelectCandidatesAllIncludesLive(t *testing.T) {
	recent := []db.RecentSession{{SessionID: "live", LastTS: ms(fixedNow)}}
	meta := map[string]transcript.Info{"live": {Cwd: "/w/live"}}
	m, r := lookups(meta, nil)
	got, d := selectCandidates(recent, m, r, map[string]bool{"live": true},
		selectConfig{limit: 20, all: true})
	if len(got) != 1 || d.live != 0 {
		t.Fatalf("--all should include live: got %d, drops.live=%d", len(got), d.live)
	}
}

func TestSelectCandidatesLimit(t *testing.T) {
	var recent []db.RecentSession
	meta := map[string]transcript.Info{}
	for _, id := range []string{"a", "b", "c"} {
		recent = append(recent, db.RecentSession{SessionID: id, LastTS: ms(fixedNow)})
		meta[id] = transcript.Info{Cwd: "/w/" + id}
	}
	m, r := lookups(meta, nil)
	got, _ := selectCandidates(recent, m, r, nil, selectConfig{limit: 2})
	if len(got) != 2 || got[0].SessionID != "a" || got[1].SessionID != "b" {
		t.Fatalf("limit 2 = %+v, want first two [a b]", got)
	}
}

func TestSelectCandidatesHereFilter(t *testing.T) {
	recent := []db.RecentSession{
		{SessionID: "mine", LastTS: ms(fixedNow)},
		{SessionID: "other", LastTS: ms(fixedNow)},
	}
	meta := map[string]transcript.Info{
		"mine":  {Cwd: "/w/mine"},
		"other": {Cwd: "/w/other"},
	}
	refs := map[string]db.RepoRef{
		"mine":  {Remote: "gh/o/here"},
		"other": {Remote: "gh/o/there"},
	}
	m, r := lookups(meta, refs)
	got, d := selectCandidates(recent, m, r, nil, selectConfig{limit: 20, hereKey: "gh/o/here"})
	if len(got) != 1 || got[0].SessionID != "mine" {
		t.Fatalf("here filter = %+v, want just [mine]", got)
	}
	if d.elsewhere != 1 {
		t.Errorf("drops.elsewhere = %d, want 1", d.elsewhere)
	}
}

func TestSelectCandidatesTopicFallsBackToAsk(t *testing.T) {
	recent := []db.RecentSession{{SessionID: "x", LastTS: ms(fixedNow)}}
	meta := map[string]transcript.Info{"x": {Cwd: "/w/x", Ask: "first line\nsecond line"}}
	m, r := lookups(meta, nil)
	got, _ := selectCandidates(recent, m, r, nil, selectConfig{limit: 20})
	if len(got) != 1 || got[0].Topic != "first line" {
		t.Fatalf("topic = %q, want the ask's first line", got[0].Topic)
	}
}

func TestDisplayLine(t *testing.T) {
	c := Candidate{Repo: "gh/o/r", Branch: "main", Topic: "do a thing", LastTS: ms(fixedNow.Add(-3 * time.Hour))}
	got := displayLine(c, fixedNow)
	for _, want := range []string{"3h", "gh/o/r@main", "do a thing"} {
		if !strings.Contains(got, want) {
			t.Errorf("displayLine = %q, missing %q", got, want)
		}
	}
	// No repo -> "(no repo)"; no topic -> "(untitled)".
	empty := displayLine(Candidate{LastTS: ms(fixedNow)}, fixedNow)
	if !strings.Contains(empty, "(no repo)") || !strings.Contains(empty, "(untitled)") {
		t.Errorf("empty displayLine = %q, want (no repo)/(untitled)", empty)
	}
}

func TestRelAge(t *testing.T) {
	cases := []struct {
		back time.Duration
		want string
	}{
		{30 * time.Second, "now"},
		{5 * time.Minute, "5m"},
		{3 * time.Hour, "3h"},
		{2 * 24 * time.Hour, "2d"},
		{3 * 7 * 24 * time.Hour, "3w"},
	}
	for _, c := range cases {
		if got := relAge(ms(fixedNow.Add(-c.back)), fixedNow); got != c.want {
			t.Errorf("relAge(-%s) = %q, want %q", c.back, got, c.want)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("hello", 10); got != "hello" {
		t.Errorf("no-op truncate = %q", got)
	}
	got := truncate("hello world", 5)
	if r := []rune(got); len(r) != 5 || !strings.HasSuffix(got, "…") {
		t.Errorf("truncate len/suffix = %q", got)
	}
}

func TestRunInit(t *testing.T) {
	var b strings.Builder
	if err := runInit(&b, "zsh"); err != nil {
		t.Fatalf("runInit zsh: %v", err)
	}
	out := b.String()
	for _, want := range []string{"claude-resume()", "cd -- \"$cwd\"", "claude --resume", "alias cr="} {
		if !strings.Contains(out, want) {
			t.Errorf("zsh init missing %q", want)
		}
	}
	if err := runInit(&strings.Builder{}, "fish"); err == nil {
		t.Error("runInit fish should error")
	}
}

func TestWriteRowsTabShape(t *testing.T) {
	var b strings.Builder
	writeRows(&b, []Candidate{{SessionID: "sid1", Cwd: "/w/x", Repo: "r", Topic: "t", LastTS: ms(fixedNow)}}, fixedNow)
	line := strings.TrimRight(b.String(), "\n")
	parts := strings.Split(line, "\t")
	if len(parts) != 3 || parts[0] != "sid1" || parts[1] != "/w/x" {
		t.Fatalf("row shape = %q (%d fields), want id\\tcwd\\tdisplay", line, len(parts))
	}
}
