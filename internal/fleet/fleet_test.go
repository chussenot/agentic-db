package fleet

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeJob creates <dir>/<jobID>/state.json with body. An empty body writes the
// directory only, reproducing a job dir that has no state file.
func writeJob(t *testing.T, dir, jobID, body string) string {
	t.Helper()
	jobDir := filepath.Join(dir, jobID)
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", jobID, err)
	}
	path := filepath.Join(jobDir, "state.json")
	if body == "" {
		return path
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", jobID, err)
	}
	return path
}

// A real colony worker mid-implementation, blocked on a permission prompt. Prompt
// text (intent) is present in the real file and deliberately kept here, so the test
// proves we do not decode it.
const workerStateJSON = `{
  "state": "working",
  "detail": "implementation in progress on worker; awaiting diff verification",
  "tempo": "blocked",
  "inFlight": { "tasks": 2, "queued": 0, "kinds": ["monitor", "local_agent"] },
  "fan": [
    { "id": "br0ifk7rz", "kind": "monitor", "label": "pact inbox for Worker-3", "startedAt": 1786711759435 },
    { "id": "a36e18c5960ea7150", "kind": "agent", "label": "Wire creator to project-manager new", "startedAt": 1786714063503 }
  ],
  "tokens": 23633,
  "intent": "SECRET PROMPT TEXT THAT MUST NOT BE DECODED",
  "respawnFlags": ["--name", "project-manager-3", "--model", "opus"],
  "name": "project-manager-3",
  "sessionId": "44644cdb-9225-485e-89f5-be3b0118a2f9",
  "daemonShort": "44644cdb",
  "cwd": "/home/e/taf/project-manager",
  "createdAt": "2026-08-14T12:48:40.597Z",
  "updatedAt": "2026-08-14T13:28:12.133Z",
  "worktreePath": "/home/e/taf/project-manager/.claude/worktrees/project-manager-3",
  "worktreeBranch": "worktree-project-manager-3",
  "backend": "daemon",
  "needs": "approve Bash: ls docs/explanation/adr/ | head -40"
}`

func TestReadJobsDecodesLiveJob(t *testing.T) {
	dir := t.TempDir()
	writeJob(t, dir, "44644cdb", workerStateJSON)

	got := NewReader(dir).ReadJobs([]string{"44644cdb"})
	job, ok := got["44644cdb"]
	if !ok {
		t.Fatalf("job not read; got %v", got)
	}
	if job.Lifecycle != LifecycleWorking {
		t.Errorf("Lifecycle = %q, want working", job.Lifecycle)
	}
	if job.Tempo != "blocked" {
		t.Errorf("Tempo = %q, want blocked", job.Tempo)
	}
	if job.Name != "project-manager-3" || job.SessionID != "44644cdb-9225-485e-89f5-be3b0118a2f9" {
		t.Errorf("identity wrong: %+v", job)
	}
	if job.Needs != "approve Bash: ls docs/explanation/adr/ | head -40" {
		t.Errorf("Needs = %q", job.Needs)
	}
	if !job.AwaitsHuman() {
		t.Error("a job with a non-empty Needs must await a human")
	}
	if job.InFlight.Tasks != 2 || job.InFlight.Queued != 0 || len(job.InFlight.Kinds) != 2 {
		t.Errorf("InFlight = %+v", job.InFlight)
	}
	if len(job.Fan) != 2 || job.Fan[0].ID != "br0ifk7rz" || job.Fan[1].Kind != "agent" {
		t.Errorf("Fan = %+v", job.Fan)
	}
	if !job.Fan[1].StartedAt.Equal(time.UnixMilli(1786714063503)) {
		t.Errorf("Fan[1].StartedAt = %v", job.Fan[1].StartedAt)
	}
	if job.Tokens != 23633 {
		t.Errorf("Tokens = %d", job.Tokens)
	}
	// Cwd is the PARENT CHECKOUT, not the worktree the session runs in. That
	// distinction is what makes repo attribution possible.
	if job.Cwd != "/home/e/taf/project-manager" {
		t.Errorf("Cwd = %q, want the parent checkout", job.Cwd)
	}
	if job.WorktreePath != "/home/e/taf/project-manager/.claude/worktrees/project-manager-3" {
		t.Errorf("WorktreePath = %q", job.WorktreePath)
	}
	if job.WorktreeBranch != "worktree-project-manager-3" {
		t.Errorf("WorktreeBranch = %q", job.WorktreeBranch)
	}
	if !job.UpdatedAt.Equal(time.Date(2026, 8, 14, 13, 28, 12, 133_000_000, time.UTC)) {
		t.Errorf("UpdatedAt = %v", job.UpdatedAt)
	}
	if job.FileModTime.IsZero() {
		t.Error("FileModTime should carry the state.json mtime")
	}
}

// The prompt-bearing fields must not reach the Job at all: there is no field to
// put them in, which is the point.
func TestReadJobsDoesNotSurfacePromptText(t *testing.T) {
	dir := t.TempDir()
	writeJob(t, dir, "44644cdb", workerStateJSON)
	job := NewReader(dir).ReadJobs([]string{"44644cdb"})["44644cdb"]

	for name, v := range map[string]string{
		"Detail": job.Detail, "Needs": job.Needs, "Name": job.Name,
		"Cwd": job.Cwd, "Template": job.Template, "Agent": job.Agent,
	} {
		if v == "SECRET PROMPT TEXT THAT MUST NOT BE DECODED" {
			t.Errorf("%s leaked the job intent", name)
		}
	}
}

func TestReadJobsBlockedWithNoNeedsDoesNotAwaitHuman(t *testing.T) {
	dir := t.TempDir()
	// A dispatcher blocked on its own workers, not on a person.
	writeJob(t, dir, "61621453", `{"state":"blocked","tempo":"blocked","needs":null,
		"detail":"waiting on chamber 3","sessionId":"61621453-x","name":"project-manager-dispatcher"}`)

	job := NewReader(dir).ReadJobs([]string{"61621453"})["61621453"]
	if job.Lifecycle != LifecycleBlocked {
		t.Fatalf("Lifecycle = %q", job.Lifecycle)
	}
	if job.AwaitsHuman() {
		t.Error("blocked with no needs must NOT await a human — it is stuck on another agent")
	}
}

func TestReadJobsSkipsUnreadableAndMissing(t *testing.T) {
	dir := t.TempDir()
	writeJob(t, dir, "good", `{"state":"done","sessionId":"s","name":"n"}`)
	writeJob(t, dir, "nostate", "")            // dir exists, no state.json
	writeJob(t, dir, "broken", `{not json at`) // malformed

	got := NewReader(dir).ReadJobs([]string{"good", "nostate", "broken", "never-existed"})
	if len(got) != 1 {
		t.Fatalf("got %d jobs, want only the good one: %v", len(got), got)
	}
	if _, ok := got["good"]; !ok {
		t.Error("the readable job should be present")
	}
}

// Liveness is the caller's business: this reader must never discover jobs by
// walking the directory, because dead job dirs linger on disk for months.
func TestReadJobsOnlyReturnsRequestedJobs(t *testing.T) {
	dir := t.TempDir()
	writeJob(t, dir, "live", `{"state":"working","sessionId":"a","name":"live"}`)
	writeJob(t, dir, "long-dead", `{"state":"done","sessionId":"b","name":"dead"}`)

	got := NewReader(dir).ReadJobs([]string{"live"})
	if len(got) != 1 {
		t.Fatalf("got %d jobs, want 1: %v", len(got), got)
	}
	if _, ok := got["long-dead"]; ok {
		t.Error("a job dir that was not asked for must not be returned")
	}
}

func TestReadJobsCachesUntilFileChanges(t *testing.T) {
	dir := t.TempDir()
	path := writeJob(t, dir, "j", `{"state":"working","detail":"first","sessionId":"s","name":"n"}`)
	r := NewReader(dir)

	if got := r.ReadJobs([]string{"j"})["j"].Detail; got != "first" {
		t.Fatalf("Detail = %q, want first", got)
	}
	// Rewrite with the SAME mtime and size: the cache should win, proving the
	// file was not re-parsed.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"state":"working","detail":"secnd","sessionId":"s","name":"n"}`), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if err := os.Chtimes(path, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if got := r.ReadJobs([]string{"j"})["j"].Detail; got != "first" {
		t.Errorf("Detail = %q, want the cached 'first'", got)
	}
	// Bump the mtime and it re-parses.
	later := fi.ModTime().Add(2 * time.Second)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if got := r.ReadJobs([]string{"j"})["j"].Detail; got != "secnd" {
		t.Errorf("Detail = %q, want the re-parsed 'secnd'", got)
	}
}

func TestReadJobsForgetsJobsNoLongerAskedFor(t *testing.T) {
	dir := t.TempDir()
	writeJob(t, dir, "a", `{"state":"working","sessionId":"a","name":"a"}`)
	writeJob(t, dir, "b", `{"state":"working","sessionId":"b","name":"b"}`)
	r := NewReader(dir)
	r.ReadJobs([]string{"a", "b"})
	if len(r.cache) != 2 {
		t.Fatalf("cache = %d entries, want 2", len(r.cache))
	}
	r.ReadJobs([]string{"a"})
	if len(r.cache) != 1 {
		t.Errorf("cache = %d entries, want 1 after b went away", len(r.cache))
	}
}
