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
	// Log carries the actual in-window commits, newest first, with full subjects.
	if len(info.Log) != 2 {
		t.Fatalf("Log = %d commits, want 2: %+v", len(info.Log), info.Log)
	}
	if info.Log[0].Subject != "in window 2" || info.Log[1].Subject != "in window 1" {
		t.Errorf("Log subjects = %q, %q; want newest-first 'in window 2', 'in window 1'",
			info.Log[0].Subject, info.Log[1].Subject)
	}
	if info.Log[0].Hash == "" || info.Log[0].At.IsZero() {
		t.Errorf("Log[0] should carry a hash and a parsed time: %+v", info.Log[0])
	}

	// A non-repo directory degrades to ok=false.
	if _, ok := Describe(filepath.Join(dir, "does-not-exist"), since, until); ok {
		t.Error("Describe should return ok=false for a missing directory")
	}
}

// TestResolve builds a throwaway repo and checks Resolve reads back the toplevel,
// normalized remote, and branch — and degrades on the edge cases.
func TestResolve(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = baseGitEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	git("init", "-b", "work")
	git("remote", "add", "origin", "git@github.com:you/proj.git")
	git("commit", "--allow-empty", "-m", "c1")

	remote, root, branch, ok := Resolve(dir)
	if !ok {
		t.Fatal("Resolve returned ok=false for a real work tree")
	}
	// root is the worktree toplevel; resolve symlinks (macOS /tmp, etc.) both sides.
	wantRoot, _ := filepath.EvalSymlinks(dir)
	gotRoot, _ := filepath.EvalSymlinks(root)
	if gotRoot != wantRoot {
		t.Errorf("root = %q, want %q", root, dir)
	}
	if remote != "gh/you/proj" {
		t.Errorf("remote = %q, want gh/you/proj", remote)
	}
	if branch != "work" {
		t.Errorf("branch = %q, want work", branch)
	}

	// Detached HEAD -> empty branch, still ok.
	git("checkout", "--detach", "HEAD")
	if _, _, b, ok := Resolve(dir); !ok || b != "" {
		t.Errorf("detached HEAD: ok=%v branch=%q, want ok=true branch=\"\"", ok, b)
	}

	// A repo with no origin -> empty remote, still ok.
	bare := t.TempDir()
	noRemote := exec.Command("git", "-C", bare, "init")
	noRemote.Env = baseGitEnv()
	if out, err := noRemote.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if rem, _, _, ok := Resolve(bare); !ok || rem != "" {
		t.Errorf("no-origin repo: ok=%v remote=%q, want ok=true remote=\"\"", ok, rem)
	}

	// Non-repo dir and empty dir -> ok=false.
	if _, _, _, ok := Resolve(t.TempDir()); ok {
		t.Error("Resolve should return ok=false for a non-repo dir")
	}
	if _, _, _, ok := Resolve(""); ok {
		t.Error("Resolve should return ok=false for an empty dir")
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

// TestParseLog checks the unit-separated decode skips blank and malformed lines
// and keeps subjects verbatim.
func TestParseLog(t *testing.T) {
	out := "abc123\x1ffeat: do the thing\x1f2026-07-07T10:00:00Z\n" +
		"\n" + // blank line — skipped
		"def456\x1ffix: the bug\x1f2026-07-06T09:00:00Z\n" +
		"garbage-without-separators\n" // < 3 fields — skipped
	log := parseLog(out)
	if len(log) != 2 {
		t.Fatalf("commits = %d, want 2: %+v", len(log), log)
	}
	if log[0].Hash != "abc123" || log[0].Subject != "feat: do the thing" {
		t.Errorf("commit[0] = %+v", log[0])
	}
	if log[0].At.IsZero() {
		t.Error("expected a parsed commit time")
	}
}
