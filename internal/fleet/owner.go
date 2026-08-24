package fleet

import (
	"os"
	"strconv"
	"strings"
)

// Environment variables Claude Code sets on background sessions and on the
// children it spawns. Their meaning DEPENDS ON WHERE YOU READ THEM, which is the
// single most important thing to know about this file:
//
//   - In a background Claude process's OWN environment, CLAUDE_PID is the pid of
//     the session that SPAWNED it. That is the only parent link Claude Code
//     exposes anywhere on disk, and it is what OwnerResolver reads.
//   - In a CHILD of any Claude process (a Bash-tool shell, a hook), CLAUDE_PID is
//     that child's own Claude pid instead. Useful — it is how a hook learns its
//     own pid without guessing — but it is NOT a parent link. Reading it in the
//     wrong place makes every session its own owner.
//
// An interactive Claude process has none of these in its own environment, so their
// presence is itself the background marker.
const (
	envClaudePID  = "CLAUDE_PID"
	envSessionKnd = "CLAUDE_CODE_SESSION_KIND"
	envJobDir     = "CLAUDE_JOB_DIR"
)

// readEnviron returns the environment of pid as a map. Package var so tests can
// stub it. Reading /proc/<pid>/environ requires the same uid, which holds for
// every process this program cares about.
var readEnviron = func(pid int) (map[string]string, bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ")
	if err != nil {
		return nil, false
	}
	return parseEnviron(string(data)), true
}

// parseEnviron splits a NUL-delimited /proc environ blob into a map. Only the
// CLAUDE_* keys this package names are kept: an environment can hold credentials,
// and there is no reason for a status tracker to hold a copy of one.
func parseEnviron(blob string) map[string]string {
	out := map[string]string{}
	for entry := range strings.SplitSeq(blob, "\x00") {
		key, val, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		switch key {
		case envClaudePID, envSessionKnd, envJobDir:
			out[key] = val
		}
	}
	return out
}

// Spawn is what a background process's environment says about its origin.
type Spawn struct {
	// SpawnerPID is the pid named by CLAUDE_PID — the session that created this
	// one. Zero when absent or unparseable.
	SpawnerPID int
	// IsBackground reflects CLAUDE_CODE_SESSION_KIND=bg.
	IsBackground bool
	// JobID is the basename of CLAUDE_JOB_DIR, empty when unset.
	JobID string
}

// OwnerResolver reads spawn information from /proc, caching by (pid, procStart) so
// a process is inspected once rather than on every tick. procStart is part of the
// key because pids are recycled quickly here: background sessions are claimed from
// a pre-forked spare pool, so a pid alone is not a stable identity.
type OwnerResolver struct {
	cache map[string]Spawn
}

// NewOwnerResolver returns an empty resolver.
func NewOwnerResolver() *OwnerResolver {
	return &OwnerResolver{cache: map[string]Spawn{}}
}

// Spawn returns what pid's environment says about who started it. procStart may be
// empty, in which case the cache degrades to keying on the pid alone. ok is false
// when the environment could not be read (the process is gone, most likely).
//
// CAVEAT, and it is a real one: background sessions are claimed from a pre-forked
// spare pool, and a spare inherits the environment of whoever caused the pool to be
// created. So CLAUDE_PID names the fleet's ROOT session, not the immediate parent —
// verified on a live colony where seven agents all reported the same spawner even
// though the dispatcher (itself a background agent) had spawned four of them. That
// makes this the right signal for "whose fleet is this" and the wrong one for
// "which agent spawned which".
func (r *OwnerResolver) Spawn(pid int, procStart string) (Spawn, bool) {
	if pid <= 0 {
		return Spawn{}, false
	}
	key := strconv.Itoa(pid) + ":" + procStart
	if s, ok := r.cache[key]; ok {
		return s, true
	}
	env, ok := readEnviron(pid)
	if !ok {
		return Spawn{}, false
	}
	s := Spawn{
		IsBackground: env[envSessionKnd] == "bg",
		JobID:        jobIDFromDir(env[envJobDir]),
	}
	if v := env[envClaudePID]; v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			s.SpawnerPID = n
		}
	}
	if s.JobID != "" {
		s.IsBackground = true
	}
	r.cache[key] = s
	return s, true
}

// Forget drops cached entries for pids that are no longer of interest, so a
// long-lived resolver does not accumulate one entry per background agent ever run.
func (r *OwnerResolver) Forget(keep map[int]bool) {
	for key := range r.cache {
		pidPart, _, _ := strings.Cut(key, ":")
		pid, err := strconv.Atoi(pidPart)
		if err != nil || !keep[pid] {
			delete(r.cache, key)
		}
	}
}

// jobIDFromDir extracts the job id from a CLAUDE_JOB_DIR path
// (~/.claude/jobs/<jobId>), tolerating a trailing separator.
func jobIDFromDir(dir string) string {
	dir = strings.TrimRight(dir, "/")
	if dir == "" {
		return ""
	}
	if i := strings.LastIndexByte(dir, '/'); i >= 0 {
		return dir[i+1:]
	}
	return dir
}
