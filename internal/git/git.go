// Package git is a thin, best-effort wrapper over the `git` CLI for the recap's
// per-project metrics: the origin remote (in a short display form) and how many
// commits landed in a time window. It shells out (mirroring internal/niri) rather
// than linking a git library, and never fails hard — a directory that isn't a
// work tree, a repo with no origin, a detached HEAD, or a missing `git` binary
// all degrade to empty/zero values so a recap still renders.
package git

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// runTimeout bounds any single git invocation. Describe runs off the recap path
// (not latency-critical) and Resolve runs on the hook hot path (must never
// block); a hung git — a network remote, a corrupt repo, an unresponsive
// filesystem — degrades to an unavailable field rather than stalling either.
const runTimeout = 500 * time.Millisecond

// Info is the git context for one repository at recap time.
type Info struct {
	Remote  string   // origin, normalized (e.g. "gh/owner/repo", "mm.tech/group/repo"); "" if none
	Branch  string   // current branch; "" if detached HEAD
	Commits int      // commits in the window reachable from HEAD
	Log     []Commit // those commits (newest first), for the "shipped X" digest
}

// Commit is one commit that landed in the recap window. Subject is the full
// commit subject, untruncated (the consumer decides what to elide).
type Commit struct {
	Hash    string    // abbreviated hash
	Subject string    // full subject line
	At      time.Time // commit time
}

// Describe returns the git Info for the repository containing dir, over the
// window [since, until]. ok is false when dir is empty, git is unavailable, or
// dir is not inside a work tree; individual fields still degrade independently
// (e.g. a repo with no origin yields Remote=="" but ok==true).
func Describe(dir string, since, until time.Time) (Info, bool) {
	if dir == "" {
		return Info{}, false
	}
	if _, err := exec.LookPath("git"); err != nil {
		return Info{}, false
	}
	// Gate on "is this a work tree?" — the cheap check that decides ok.
	if out, err := run(dir, "rev-parse", "--is-inside-work-tree"); err != nil ||
		strings.TrimSpace(out) != "true" {
		return Info{}, false
	}

	var i Info
	if out, err := run(dir, "remote", "get-url", "origin"); err == nil {
		i.Remote = normalizeRemote(out)
	}
	if out, err := run(dir, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		if b := strings.TrimSpace(out); b != "HEAD" { // "HEAD" == detached
			i.Branch = b
		}
	}
	if out, err := run(dir, "rev-list", "--count", "HEAD",
		"--since="+since.Format(time.RFC3339), "--until="+until.Format(time.RFC3339)); err == nil {
		if n, cerr := strconv.Atoi(strings.TrimSpace(out)); cerr == nil {
			i.Commits = n
		}
	}
	// The commits themselves (newest first) — the "shipped X" content behind the
	// count. Fields are unit-separated (%x1f) so a subject can hold any character;
	// no --max-count, so the full window is captured untruncated.
	if out, err := run(dir, "log", "HEAD", "--no-merges",
		"--since="+since.Format(time.RFC3339), "--until="+until.Format(time.RFC3339),
		"--format=%h%x1f%s%x1f%cI"); err == nil {
		i.Log = parseLog(out)
	}
	return i, true
}

// parseLog decodes the unit-separated `git log` output into Commits, skipping
// malformed lines. Commit time is best-effort — an unparseable date yields a
// zero time, never a dropped commit.
func parseLog(out string) []Commit {
	var log []Commit
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Split(line, "\x1f")
		if len(f) < 3 {
			continue
		}
		at, _ := time.Parse(time.RFC3339, strings.TrimSpace(f[2]))
		log = append(log, Commit{Hash: f[0], Subject: f[1], At: at})
	}
	return log
}

// Resolve returns the identity of the repo containing dir — its git worktree
// toplevel (root), normalized origin remote, and current branch — for capture on
// the hook hot path. It is the cheap cousin of Describe: it OMITS the commit
// count (a rev-list walk), which is window-dependent and belongs at recap time,
// not on every session's first hook. ok is false when dir is empty, git is
// unavailable, or dir is not inside a work tree; remote/branch degrade to "" on
// their own (a repo with no origin or a detached HEAD still yields ok==true).
func Resolve(dir string) (remote, root, branch string, ok bool) {
	if dir == "" {
		return "", "", "", false
	}
	if _, err := exec.LookPath("git"); err != nil {
		return "", "", "", false
	}
	out, err := run(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", "", "", false // not a work tree (or git failed)
	}
	root = strings.TrimSpace(out)
	if root == "" {
		return "", "", "", false
	}
	if out, err := run(dir, "remote", "get-url", "origin"); err == nil {
		remote = normalizeRemote(out)
	}
	if out, err := run(dir, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		if b := strings.TrimSpace(out); b != "HEAD" { // "HEAD" == detached
			branch = b
		}
	}
	return remote, root, branch, true
}

// run executes `git -C dir <args...>` and returns its stdout, under runTimeout.
// Errors (including a non-zero exit or a timeout) are returned so callers can
// treat that field as unavailable.
func run(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).Output()
	return string(out), err
}

// normalizeRemote reduces a git remote URL to a compact "host/path" label,
// mapping github.com to "gh". It handles both scp-like (git@host:path) and URL
// (scheme://[user@]host[:port]/path) forms and strips a trailing ".git".
func normalizeRemote(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	s = strings.TrimSuffix(s, ".git")

	if i := strings.Index(s, "://"); i >= 0 {
		// scheme://[user@]host[:port]/path
		s = s[i+3:]
		if at := strings.Index(s, "@"); at >= 0 {
			s = s[at+1:]
		}
	} else if at := strings.LastIndex(s, "@"); at >= 0 {
		// scp-like: [user@]host:path -> host/path
		s = s[at+1:]
		s = strings.Replace(s, ":", "/", 1)
	}

	host, path, _ := strings.Cut(s, "/")
	if h, _, ok := strings.Cut(host, ":"); ok { // drop any :port
		host = h
	}
	if host == "github.com" {
		host = "gh"
	}
	if path == "" {
		return host
	}
	return host + "/" + path
}
