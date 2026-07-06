package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestNormalizeRemote(t *testing.T) {
	cases := map[string]string{
		"git@github.com:mrzor/claude-status.git":       "gh/mrzor/claude-status",
		"https://github.com/mrzor/claude-status.git":   "gh/mrzor/claude-status",
		"https://github.com/mrzor/claude-status":       "gh/mrzor/claude-status",
		"ssh://git@github.com/mrzor/claude-status.git": "gh/mrzor/claude-status",
		"git@mm.tech:obs/datadog-ci.git":               "mm.tech/obs/datadog-ci",
		"https://mm.tech/obs/group/datadog-ci.git":     "mm.tech/obs/group/datadog-ci",
		"https://gitlab.com:8443/team/app.git":         "gitlab.com/team/app",
		"":                                             "",
		"git@github.com:owner/repo":                    "gh/owner/repo",
	}
	for in, want := range cases {
		if got := normalizeRemote(in); got != want {
			t.Errorf("normalizeRemote(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestDescribe builds a throwaway repo with commits at known dates and checks
// that Describe reads back the remote, branch, and window commit count.
func TestDescribe(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}
	dir := t.TempDir()

	git := func(env []string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(baseGitEnv(), env...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	commitAt := func(msg, iso string) {
		date := []string{"GIT_AUTHOR_DATE=" + iso, "GIT_COMMITTER_DATE=" + iso}
		git(date, "commit", "--allow-empty", "-m", msg)
	}

	git(nil, "init", "-b", "main")
	git(nil, "remote", "add", "origin", "git@github.com:you/proj.git")
	commitAt("before window", "2026-06-30T12:00:00")
	commitAt("in window 1", "2026-07-03T10:00:00")
	commitAt("in window 2", "2026-07-04T10:00:00")
	commitAt("after window", "2026-07-10T10:00:00")

	since := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	info, ok := Describe(dir, since, until)
	if !ok {
		t.Fatal("Describe returned ok=false for a real work tree")
	}
	if info.Remote != "gh/you/proj" {
		t.Errorf("Remote = %q, want gh/you/proj", info.Remote)
	}
	if info.Branch != "main" {
		t.Errorf("Branch = %q, want main", info.Branch)
	}
	if info.Commits != 2 {
		t.Errorf("Commits = %d, want 2 (only the two in-window commits)", info.Commits)
	}

	// A non-repo directory degrades to ok=false.
	if _, ok := Describe(filepath.Join(dir, "does-not-exist"), since, until); ok {
		t.Error("Describe should return ok=false for a missing directory")
	}
}

// baseGitEnv gives the throwaway repo a self-contained identity/config so the
// test doesn't depend on (or touch) the developer's global git config.
func baseGitEnv() []string {
	return []string{
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t",
		"GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t",
		"GIT_COMMITTER_EMAIL=t@example.com",
		"HOME=/tmp",
		"PATH=" + os.Getenv("PATH"),
	}
}
