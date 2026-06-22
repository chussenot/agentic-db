package clauded

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// write drops a session file into dir under <name>.
func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func TestReadParsesAndKeysBySessionID(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "10694.json", `{"pid":10694,"sessionId":"sess-a","cwd":"/home/x","status":"busy","statusUpdatedAt":1782114927705,"version":"2.1.183"}`)
	write(t, dir, "6843.json", `{"pid":6843,"sessionId":"sess-b","cwd":"/home/y","status":"idle","statusUpdatedAt":1782114122401}`)

	got, err := Read(dir)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d sessions, want 2", len(got))
	}
	a := got["sess-a"]
	if a.PID != 10694 || a.Status != Busy || a.Cwd != "/home/x" || a.Version != "2.1.183" {
		t.Errorf("sess-a decoded wrong: %+v", a)
	}
	if !a.StatusUpdatedAt.Equal(time.UnixMilli(1782114927705)) {
		t.Errorf("sess-a StatusUpdatedAt = %v", a.StatusUpdatedAt)
	}
	if got["sess-b"].Status != Idle {
		t.Errorf("sess-b status = %q, want idle", got["sess-b"].Status)
	}
}

func TestReadCapturesWaitingStatus(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "1.json", `{"pid":1,"sessionId":"s","status":"waiting","statusUpdatedAt":1782114927705}`)
	got, _ := Read(dir)
	if got["s"].Status != Waiting {
		t.Errorf("status = %q, want waiting", got["s"].Status)
	}
	if !got["s"].Status.Known() {
		t.Error("waiting should be a Known status")
	}
}

func TestReadMissingDirIsEmptyNotError(t *testing.T) {
	got, err := Read(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("missing dir should not error, got %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want empty map, got %d", len(got))
	}
}

func TestReadSkipsMalformedAndNonJSON(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "good.json", `{"pid":1,"sessionId":"ok","status":"busy","statusUpdatedAt":1}`)
	write(t, dir, "bad.json", `{not json`)
	write(t, dir, "noid.json", `{"pid":2,"status":"busy"}`) // no sessionId -> skip
	write(t, dir, "notes.txt", `ignored, not .json`)

	got, err := Read(dir)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 1 || got["ok"].PID != 1 {
		t.Errorf("want only the good row, got %+v", got)
	}
}

func TestReadUnknownFieldsTolerated(t *testing.T) {
	dir := t.TempDir()
	// Simulate a future Claude version adding fields and an unrecognized status.
	write(t, dir, "1.json", `{"pid":1,"sessionId":"s","status":"compacting","statusUpdatedAt":1,"futureField":{"x":1},"peerProtocol":2}`)
	got, err := Read(dir)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	s := got["s"]
	if s.Status != Status("compacting") {
		t.Errorf("unknown status should be preserved verbatim, got %q", s.Status)
	}
	if s.Status.Known() {
		t.Error("an unrecognized status must not report Known")
	}
}

func TestReadDedupesBySessionIDKeepingFresher(t *testing.T) {
	dir := t.TempDir()
	// Same session reported by two files (pid reuse / stale leftover); fresher wins.
	write(t, dir, "100.json", `{"pid":100,"sessionId":"dup","status":"idle","statusUpdatedAt":1000}`)
	write(t, dir, "200.json", `{"pid":200,"sessionId":"dup","status":"busy","statusUpdatedAt":2000}`)
	got, _ := Read(dir)
	if len(got) != 1 {
		t.Fatalf("want 1 deduped session, got %d", len(got))
	}
	if got["dup"].PID != 200 || got["dup"].Status != Busy {
		t.Errorf("fresher file should win, got %+v", got["dup"])
	}
}

func TestReadOmittedStatusUpdatedAtIsZeroTime(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "1.json", `{"pid":1,"sessionId":"s","status":"idle"}`)
	got, _ := Read(dir)
	if !got["s"].StatusUpdatedAt.IsZero() {
		t.Errorf("missing statusUpdatedAt should be zero time, got %v", got["s"].StatusUpdatedAt)
	}
}
