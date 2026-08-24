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

func TestReadCapturesShellStatus(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "1.json", `{"pid":1,"sessionId":"s","status":"shell","statusUpdatedAt":1782114927705}`)
	got, _ := Read(dir)
	if got["s"].Status != Shell || !got["s"].Status.Known() {
		t.Errorf("status = %q (known=%v), want shell/known", got["s"].Status, got["s"].Status.Known())
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

// stubProc stubs the /proc liveness seam: only the listed pids are "alive".
func stubProc(t *testing.T, alive ...int) {
	t.Helper()
	set := make(map[int]bool, len(alive))
	for _, p := range alive {
		set[p] = true
	}
	orig := procExists
	procExists = func(pid int) bool { return set[pid] }
	t.Cleanup(func() { procExists = orig })
}

// stubProcStart stubs the process-start-time seam: pids in start report that
// value, any other pid reports "unreadable".
func stubProcStart(t *testing.T, start map[int]string) {
	t.Helper()
	orig := procStarted
	procStarted = func(pid int) (string, bool) {
		v, ok := start[pid]
		return v, ok
	}
	t.Cleanup(func() { procStarted = orig })
}

// Verbatim body of a real background-agent session file (Claude Code 2.1.231),
// with the messaging socket path kept to prove unknown/ignored fields still parse.
const bgSessionFile = `{
  "cwd": "/home/e/taf/project-manager/.claude/worktrees/project-manager-3",
  "entrypoint": "cli",
  "jobId": "44644cdb",
  "kind": "bg",
  "messagingSocketPath": "/run/user/1000/cc-socks/2920644.sock",
  "name": "project-manager-3",
  "peerProtocol": 1,
  "pid": 2920644,
  "procStart": "27771327",
  "sessionId": "44644cdb-9225-485e-89f5-be3b0118a2f9",
  "startedAt": 1786711721191,
  "status": "waiting",
  "statusUpdatedAt": 1786714092128,
  "updatedAt": 1786714092128,
  "version": "2.1.231",
  "waitingFor": "permission prompt"
}`

// Verbatim body of a real interactive session file. Note parkedJobId: this
// session parked a bg job once, which must NOT make it read as a background
// session.
const interactiveSessionFile = `{
  "cwd": "/home/e/taf/project-manager",
  "entrypoint": "cli",
  "kind": "interactive",
  "name": "project-manager-f4",
  "nameSource": "derived",
  "parkedJobId": "c15e702a",
  "peerProtocol": 1,
  "pid": 2823234,
  "procStart": "27681796",
  "sessionId": "2e1ce074-51a8-4fa2-9a1c-e9b80d019c48",
  "startedAt": 1786709275649,
  "status": "idle",
  "statusUpdatedAt": 1786709521415,
  "updatedAt": 1786709523073,
  "version": "2.1.231"
}`

func TestReadDecodesBackgroundIdentity(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "2920644.json", bgSessionFile)

	got, err := Read(dir)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	s := got["44644cdb-9225-485e-89f5-be3b0118a2f9"]
	if s.Kind != KindBackground {
		t.Errorf("Kind = %q, want %q", s.Kind, KindBackground)
	}
	if !s.IsBackground() {
		t.Error("IsBackground() = false for a kind:bg session")
	}
	if s.JobID != "44644cdb" {
		t.Errorf("JobID = %q, want 44644cdb", s.JobID)
	}
	if s.Name != "project-manager-3" {
		t.Errorf("Name = %q", s.Name)
	}
	if s.ProcStart != "27771327" {
		t.Errorf("ProcStart = %q", s.ProcStart)
	}
	if s.Entrypoint != "cli" {
		t.Errorf("Entrypoint = %q", s.Entrypoint)
	}
	if !s.StartedAt.Equal(time.UnixMilli(1786711721191)) {
		t.Errorf("StartedAt = %v", s.StartedAt)
	}
	if !s.UpdatedAt.Equal(time.UnixMilli(1786714092128)) {
		t.Errorf("UpdatedAt = %v", s.UpdatedAt)
	}
	if !s.IsUserPrompt() {
		t.Error("waiting on a permission prompt should be a user prompt")
	}
}

func TestReadInteractiveParkedJobIsNotBackground(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "2823234.json", interactiveSessionFile)

	got, _ := Read(dir)
	s := got["2e1ce074-51a8-4fa2-9a1c-e9b80d019c48"]
	if s.Kind != KindInteractive {
		t.Errorf("Kind = %q, want %q", s.Kind, KindInteractive)
	}
	if s.ParkedJobID != "c15e702a" {
		t.Errorf("ParkedJobID = %q, want c15e702a", s.ParkedJobID)
	}
	// The trap: parkedJobId names a bg job this session once parked. It is a
	// single value, not a child inventory, and it must never imply bg — the job
	// it names lives on under its own session id.
	if s.IsBackground() {
		t.Error("an interactive session with parkedJobId must not read as background")
	}
	if s.JobID != "" {
		t.Errorf("JobID = %q, want empty for an interactive session", s.JobID)
	}
	if s.NameSource != "derived" {
		t.Errorf("NameSource = %q", s.NameSource)
	}
}

func TestReadDecodesAgentType(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "2825303.json", `{"pid":2825303,"sessionId":"s","kind":"bg","jobId":"ac83bcd9",
		"agent":"colony-integrator","name":"project-manager-integrator","status":"shell","statusUpdatedAt":1}`)

	got, _ := Read(dir)
	if got["s"].Agent != "colony-integrator" {
		t.Errorf("Agent = %q, want colony-integrator", got["s"].Agent)
	}
}

func TestIsUserPromptAllowList(t *testing.T) {
	cases := []struct {
		name       string
		status     Status
		waitingFor string
		want       bool
	}{
		{"permission prompt", Waiting, "permission prompt", true},
		{"input needed (AskUserQuestion)", Waiting, "input needed", true},
		{"internal subagent wait", Waiting, "subagent", false},
		{"tool wait", Waiting, "tool", false},
		{"waiting with no reason", Waiting, "", false},
		{"busy is never a prompt", Busy, "permission prompt", false},
		{"idle is never a prompt", Idle, "input needed", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := Session{Status: c.status, WaitingFor: c.waitingFor}
			if got := s.IsUserPrompt(); got != c.want {
				t.Errorf("IsUserPrompt() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestStatStartTime(t *testing.T) {
	// Field 22 is starttime. Counting must begin after the LAST ')' because the
	// comm field can itself contain spaces and parentheses.
	cases := []struct {
		name string
		stat string
		want string
	}{
		{
			name: "plain comm",
			stat: "2920644 (claude) S 2920305 2920305 0 0 -1 4194304 1 2 3 4 5 6 7 8 20 0 1 0 27771327 100 200\n",
			want: "27771327",
		},
		{
			name: "comm with spaces and parens",
			stat: "77 (weird ) proc) S 1 1 0 0 -1 0 1 2 3 4 5 6 7 8 20 0 1 0 999 100 200\n",
			want: "999",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := statStartTime(c.stat)
			if !ok {
				t.Fatalf("statStartTime(%q) not ok", c.name)
			}
			if got != c.want {
				t.Errorf("starttime = %q, want %q", got, c.want)
			}
		})
	}
	if _, ok := statStartTime("truncated 1 2 3"); ok {
		t.Error("a stat line with no ')' and too few fields should not parse")
	}
	if _, ok := statStartTime("1 (claude) S 2 3"); ok {
		t.Error("a stat line with too few fields after comm should not parse")
	}
}

func TestAliveDetectsPidReuse(t *testing.T) {
	// pid 7 is alive but its start time is unreadable (no entry in the map).
	stubProc(t, 42, 7)
	stubProcStart(t, map[int]string{42: "27771327"})

	same := Session{PID: 42, ProcStart: "27771327"}
	if !same.Alive() {
		t.Error("matching procStart should be alive")
	}
	// Same pid, different start time: a DIFFERENT process reused the pid, so the
	// session that wrote this file is gone. Background agents are claimed from a
	// pre-forked spare pool, so pid churn is routine.
	reused := Session{PID: 42, ProcStart: "11111111"}
	if reused.Alive() {
		t.Error("procStart mismatch should be dead (pid reuse)")
	}
	// No recorded procStart: unknowable, must not drop on a guess.
	if !(Session{PID: 42}).Alive() {
		t.Error("missing procStart should stay alive")
	}
	// Live start time unreadable: also unknowable.
	if !(Session{PID: 7, ProcStart: "27771327"}).Alive() {
		t.Error("unreadable live procStart should stay alive")
	}
}

func TestAlive(t *testing.T) {
	stubProc(t, 42)
	if !(Session{PID: 42}).Alive() {
		t.Error("pid 42 stubbed alive but Alive()=false")
	}
	if (Session{PID: 99}).Alive() {
		t.Error("pid 99 not alive but Alive()=true")
	}
	// No usable pid -> treated as alive (cannot check, must not drop on a guess).
	if !(Session{PID: 0}).Alive() {
		t.Error("pid 0 should be treated as alive")
	}
}

func TestReadLiveDropsDeadPidEntries(t *testing.T) {
	dir := t.TempDir()
	// live: process running. zombie: file present but its claude pid is dead
	// (hard kill left the file behind).
	write(t, dir, "10.json", `{"pid":10,"sessionId":"live","status":"idle","statusUpdatedAt":1}`)
	write(t, dir, "20.json", `{"pid":20,"sessionId":"zombie","status":"busy","statusUpdatedAt":1}`)
	stubProc(t, 10) // only pid 10 alive

	// Raw Read keeps both (doctor needs the zombie visible).
	raw, err := Read(dir)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(raw) != 2 {
		t.Fatalf("raw Read should keep both files, got %d", len(raw))
	}

	// ReadLive drops the dead-pid zombie.
	live, err := ReadLive(dir)
	if err != nil {
		t.Fatalf("ReadLive: %v", err)
	}
	if _, ok := live["zombie"]; ok {
		t.Error("ReadLive kept a dead-pid (crash-zombie) entry")
	}
	if _, ok := live["live"]; !ok {
		t.Error("ReadLive dropped a live session")
	}
}
