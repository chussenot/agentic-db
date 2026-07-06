package recap

import (
	"strings"
	"testing"
	"time"
)

func TestMetricsMarkdown(t *testing.T) {
	var b strings.Builder
	MetricsMarkdown(&b, Digest{Totals: Totals{
		Sessions:  4,
		Active:    64 * time.Minute,
		MinActive: 2 * time.Minute,
		AvgActive: 16 * time.Minute,
		MaxActive: time.Hour,
		Prompts:   78,
		Questions: 51,
	}})
	out := b.String()
	for _, want := range []string{
		"## Metrics\n\n", // blank line after the heading
		"Total time clauding: 1h04m",
		"Per session: min 2m · avg 16m · max 1h00m (4 sessions)",
		"Prompts sent: 78",
		"Permission prompts: 51",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics output missing %q\n---\n%s", want, out)
		}
	}
}

func TestProjectMetricsMarkdown(t *testing.T) {
	var b strings.Builder
	MetricsMarkdown(&b, Digest{
		Totals: Totals{Sessions: 3, Active: time.Hour},
		Projects: []Project{
			{Name: "ezmm", HasRepo: true, Remote: "gh/you/ezmm", Branch: "feat/prefs", Commits: 8},
			{Name: "datadog-ci", HasRepo: true, Remote: "mm.tech/obs/datadog-ci", Branch: "main", Commits: 1},
			{Name: "scratch", HasRepo: true, Branch: "master", Commits: 0}, // repo, no remote
			// A removed temp worktree / non-repo cwd: unresolved, must be dropped.
			{Name: "ezmm-fyre-systray", HasRepo: false},
		},
	})
	out := b.String()
	for _, want := range []string{
		"## Project metrics\n\n",
		"- ezmm — gh/you/ezmm · 8 commits (feat/prefs)",    // feature branch surfaced
		"- datadog-ci — mm.tech/obs/datadog-ci · 1 commit", // singular; main hidden
		"- scratch — (no remote) · 0 commits",              // repo without a remote is kept
	} {
		if !strings.Contains(out, want) {
			t.Errorf("project metrics missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "ezmm-fyre-systray") {
		t.Errorf("unresolvable project (gone worktree) should be dropped, got:\n%s", out)
	}
	if strings.Contains(out, "more project") {
		t.Errorf("no cap should remain; every git-resolved project is listed:\n%s", out)
	}
}

func TestProjectMetricsAllReposShown(t *testing.T) {
	var projects []Project
	for range 20 {
		projects = append(projects, Project{Name: "p", HasRepo: true, Remote: "gh/x/p", Commits: 1})
	}
	var b strings.Builder
	MetricsMarkdown(&b, Digest{Totals: Totals{Sessions: 1, Active: time.Hour}, Projects: projects})
	if got := strings.Count(b.String(), "\n- p — "); got != 20 {
		t.Errorf("all 20 resolved projects should be listed, got %d", got)
	}
}

func TestMetricsMarkdownZeroSessions(t *testing.T) {
	var b strings.Builder
	MetricsMarkdown(&b, Digest{}) // Sessions == 0 must not divide by zero
	if !strings.Contains(b.String(), "No activity") {
		t.Errorf("zero-session metrics should report no activity, got:\n%s", b.String())
	}
}
