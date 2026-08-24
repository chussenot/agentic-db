package fleet

import (
	"strings"
	"testing"
)

// stubEnviron makes readEnviron serve the given per-pid NUL-delimited blobs.
// A pid absent from the map reads as "process gone".
func stubEnviron(t *testing.T, blobs map[int][]string) *int {
	t.Helper()
	calls := 0
	orig := readEnviron
	readEnviron = func(pid int) (map[string]string, bool) {
		calls++
		entries, ok := blobs[pid]
		if !ok {
			return nil, false
		}
		return parseEnviron(strings.Join(entries, "\x00")), true
	}
	t.Cleanup(func() { readEnviron = orig })
	return &calls
}

func TestSpawnReadsBackgroundOrigin(t *testing.T) {
	// A live background agent's own environment, as observed on 2.1.231.
	stubEnviron(t, map[int][]string{
		2825303: {
			"CLAUDE_CODE_ENTRYPOINT=cli",
			"CLAUDE_PID=2823234",
			"CLAUDE_CODE_SESSION_KIND=bg",
			"CLAUDE_JOB_DIR=/home/e/.claude/jobs/ac83bcd9",
			"CLAUDE_CODE_SESSION_NAME=project-manager-integrator",
		},
	})
	got, ok := NewOwnerResolver().Spawn(2825303, "27688829")
	if !ok {
		t.Fatal("Spawn not ok for a readable environ")
	}
	if got.SpawnerPID != 2823234 {
		t.Errorf("SpawnerPID = %d, want 2823234", got.SpawnerPID)
	}
	if !got.IsBackground {
		t.Error("IsBackground = false for CLAUDE_CODE_SESSION_KIND=bg")
	}
	if got.JobID != "ac83bcd9" {
		t.Errorf("JobID = %q, want ac83bcd9", got.JobID)
	}
}

// An interactive Claude carries none of these variables in its own environment,
// which is what makes their presence a background marker.
func TestSpawnInteractiveHasNoOrigin(t *testing.T) {
	stubEnviron(t, map[int][]string{
		2823234: {"PATH=/usr/bin", "HOME=/home/e", "TERM=xterm-kitty"},
	})
	got, ok := NewOwnerResolver().Spawn(2823234, "27681796")
	if !ok {
		t.Fatal("Spawn not ok")
	}
	if got.SpawnerPID != 0 || got.IsBackground || got.JobID != "" {
		t.Errorf("interactive session should have no spawn origin, got %+v", got)
	}
}

// A job dir alone is enough to call it background, even if the kind variable is
// missing — the two markers are independent on disk.
func TestSpawnJobDirImpliesBackground(t *testing.T) {
	stubEnviron(t, map[int][]string{
		7: {"CLAUDE_JOB_DIR=/home/e/.claude/jobs/1d3c6dd8/"},
	})
	got, _ := NewOwnerResolver().Spawn(7, "")
	if !got.IsBackground {
		t.Error("a CLAUDE_JOB_DIR should imply background")
	}
	if got.JobID != "1d3c6dd8" {
		t.Errorf("JobID = %q, want 1d3c6dd8 (trailing slash tolerated)", got.JobID)
	}
}

func TestSpawnDeadProcessIsNotOK(t *testing.T) {
	stubEnviron(t, map[int][]string{})
	if _, ok := NewOwnerResolver().Spawn(4242, "1"); ok {
		t.Error("an unreadable environ should report not-ok, not an empty Spawn")
	}
	if _, ok := NewOwnerResolver().Spawn(0, ""); ok {
		t.Error("pid 0 should report not-ok")
	}
}

func TestSpawnCachesByPidAndProcStart(t *testing.T) {
	calls := stubEnviron(t, map[int][]string{
		100: {"CLAUDE_PID=1", "CLAUDE_CODE_SESSION_KIND=bg"},
	})
	r := NewOwnerResolver()
	for range 5 {
		if _, ok := r.Spawn(100, "aaa"); !ok {
			t.Fatal("Spawn not ok")
		}
	}
	if *calls != 1 {
		t.Errorf("read /proc %d times, want 1 (cached)", *calls)
	}
	// Same pid, different start time = a different process (the spare pool churns
	// pids), so it must be read again rather than served from cache.
	if _, ok := r.Spawn(100, "bbb"); !ok {
		t.Fatal("Spawn not ok after procStart change")
	}
	if *calls != 2 {
		t.Errorf("read /proc %d times, want 2 (procStart is part of the key)", *calls)
	}
}

func TestForgetDropsUnwantedPids(t *testing.T) {
	stubEnviron(t, map[int][]string{
		1: {"CLAUDE_PID=9"},
		2: {"CLAUDE_PID=9"},
	})
	r := NewOwnerResolver()
	r.Spawn(1, "a")
	r.Spawn(2, "b")
	if len(r.cache) != 2 {
		t.Fatalf("cache = %d, want 2", len(r.cache))
	}
	r.Forget(map[int]bool{1: true})
	if len(r.cache) != 1 {
		t.Errorf("cache = %d, want 1 after Forget", len(r.cache))
	}
	if _, ok := r.cache["1:a"]; !ok {
		t.Error("the kept pid's entry should survive Forget")
	}
}

// The environ blob can hold credentials; only the CLAUDE_* keys this package names
// are ever retained.
func TestParseEnvironKeepsOnlyKnownKeys(t *testing.T) {
	blob := strings.Join([]string{
		"CLAUDE_PID=42",
		"AWS_SECRET_ACCESS_KEY=hunter2",
		"CLAUDE_CODE_OAUTH_TOKEN=sk-not-yours",
		"CLAUDE_CODE_SESSION_KIND=bg",
		"malformed-entry-without-equals",
	}, "\x00")

	got := parseEnviron(blob)
	if len(got) != 2 {
		t.Fatalf("kept %d keys, want 2: %v", len(got), got)
	}
	for k := range got {
		if k != envClaudePID && k != envSessionKnd {
			t.Errorf("unexpected key retained: %q", k)
		}
	}
}
