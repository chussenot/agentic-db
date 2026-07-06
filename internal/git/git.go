// Package git is a thin, best-effort wrapper over the `git` CLI for the recap's
// per-project metrics: the origin remote (in a short display form) and how many
// commits landed in a time window. It shells out (mirroring internal/niri) rather
// than linking a git library, and never fails hard — a directory that isn't a
// work tree, a repo with no origin, a detached HEAD, or a missing `git` binary
// all degrade to empty/zero values so a recap still renders.
package git

import (
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Info is the git context for one repository at recap time.
type Info struct {
	Remote  string // origin, normalized (e.g. "gh/owner/repo", "mm.tech/group/repo"); "" if none
	Branch  string // current branch; "" if detached HEAD
	Commits int    // commits in the window reachable from HEAD
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
	return i, true
}

// run executes `git -C dir <args...>` and returns its stdout. Errors (including a
// non-zero exit) are returned so callers can treat that field as unavailable.
func run(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
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
