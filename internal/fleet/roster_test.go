package fleet

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A trimmed but structurally faithful roster, including the auth fields the real
// file carries so the test can prove they are not decoded.
const rosterJSON = `{
  "proto": 1,
  "supervisorPid": 2825234,
  "updatedAt": 1786713518000,
  "workers": {
    "61621453": {
      "pid": 2825277,
      "procStart": "27688800",
      "sessionId": "61621453-5247-4233-8c76-34c5033fd0fb",
      "cwd": "/home/e/taf/project-manager",
      "startedAt": 1786709346784,
      "attempt": 1,
      "ptyAuth": "SECRET",
      "rvAuth": "SECRET",
      "dispatch": {
        "source": "shell",
        "agent": "colony-dispatcher",
        "nonce": "SECRET",
        "seed": { "name": "project-manager-dispatcher", "intent": "SECRET PROMPT" }
      }
    },
    "c15e702a": {
      "pid": 2832688,
      "sessionId": "c15e702a-5f2b-4820-a2c0-b4d7326d9ddc",
      "cwd": "/home/e/taf/project-manager",
      "attempt": 2,
      "dispatch": { "source": "slash" }
    }
  }
}`

func writeRoster(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "roster.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write roster: %v", err)
	}
	return path
}

func TestReadRoster(t *testing.T) {
	got, ok := ReadRoster(writeRoster(t, rosterJSON))
	if !ok {
		t.Fatal("ReadRoster not ok")
	}
	if got.SupervisorPID != 2825234 {
		t.Errorf("SupervisorPID = %d, want 2825234", got.SupervisorPID)
	}
	if !got.UpdatedAt.Equal(time.UnixMilli(1786713518000)) {
		t.Errorf("UpdatedAt = %v", got.UpdatedAt)
	}
	if len(got.Workers) != 2 {
		t.Fatalf("got %d workers, want 2", len(got.Workers))
	}
	w := got.Workers["61621453"]
	if w.SessionID != "61621453-5247-4233-8c76-34c5033fd0fb" {
		t.Errorf("SessionID = %q", w.SessionID)
	}
	if w.Source != "shell" || w.Agent != "colony-dispatcher" {
		t.Errorf("dispatch not decoded: %+v", w)
	}
	if w.Attempt != 1 || got.Workers["c15e702a"].Attempt != 2 {
		t.Errorf("Attempt not decoded: %+v / %+v", w, got.Workers["c15e702a"])
	}
	// The field is named PTYHostPID precisely so nobody mistakes it for the
	// Claude session pid: the session inside this worker is 2825304, not 2825277.
	if w.PTYHostPID != 2825277 {
		t.Errorf("PTYHostPID = %d, want the pty host 2825277", w.PTYHostPID)
	}
	if !w.StartedAt.Equal(time.UnixMilli(1786709346784)) {
		t.Errorf("StartedAt = %v", w.StartedAt)
	}
}

func TestReadRosterMissingOrMalformedIsNotFatal(t *testing.T) {
	if _, ok := ReadRoster(filepath.Join(t.TempDir(), "absent.json")); ok {
		t.Error("a missing roster should report not-ok")
	}
	if _, ok := ReadRoster(writeRoster(t, `{"workers": [broken`)); ok {
		t.Error("a malformed roster should report not-ok")
	}
	// An empty roster is valid: the machine has a supervisor but no workers.
	got, ok := ReadRoster(writeRoster(t, `{"proto":1,"supervisorPid":10,"workers":{}}`))
	if !ok || got.SupervisorPID != 10 || len(got.Workers) != 0 {
		t.Errorf("empty roster mishandled: ok=%v %+v", ok, got)
	}
}

// A roster OUTLIVES its supervisor: the file is rewritten on every dispatch and
// never cleaned up, so a pool that exited days ago still parses perfectly. That is
// how `claude-status fleet` came to announce "supervisor: pid 1260360, 1 worker(s)"
// with /proc/1260360 long gone and the file 4.8 days old. ReadRoster is the wrong
// place to answer it (the parse really did succeed); SupervisorAlive is.
func TestSupervisorAliveIsWhatMakesARosterCurrent(t *testing.T) {
	r, ok := ReadRoster(writeRoster(t, rosterJSON))
	if !ok {
		t.Fatal("ReadRoster not ok")
	}

	orig := procExists
	t.Cleanup(func() { procExists = orig })

	procExists = func(pid int) bool { return pid == r.SupervisorPID }
	if !r.SupervisorAlive() {
		t.Error("a roster whose supervisor pid is alive must still be reported")
	}

	procExists = func(int) bool { return false }
	if r.SupervisorAlive() {
		t.Error("a roster whose supervisor pid is gone must not be reported")
	}

	// A roster naming no supervisor is never alive, whatever /proc says: pid 0 is
	// not a process, and asking about it would be a category error.
	procExists = func(int) bool { return true }
	if (Roster{}).SupervisorAlive() {
		t.Error("a roster with no supervisorPid must not be reported")
	}
}
