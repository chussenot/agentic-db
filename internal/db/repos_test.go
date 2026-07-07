package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/mrzor/claude-status/internal/state"
)

func TestUpsertRepoDedup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude.sqlite")
	d, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	// Same remote seen twice (from two different checkout paths) -> one repo.
	id1, err := d.UpsertRepo("gh/owner/proj", "/a/proj", 1000)
	if err != nil {
		t.Fatalf("UpsertRepo 1: %v", err)
	}
	id2, err := d.UpsertRepo("gh/owner/proj", "/b/proj", 2000)
	if err != nil {
		t.Fatalf("UpsertRepo 2: %v", err)
	}
	if id1 != id2 {
		t.Errorf("remote dedup: ids %d != %d, want the same repo", id1, id2)
	}

	// A local-only repo (no remote) dedups on root_path.
	l1, err := d.UpsertRepo("", "/local/only", 1000)
	if err != nil {
		t.Fatalf("UpsertRepo local 1: %v", err)
	}
	l2, err := d.UpsertRepo("", "/local/only", 3000)
	if err != nil {
		t.Fatalf("UpsertRepo local 2: %v", err)
	}
	if l1 != l2 {
		t.Errorf("local-only dedup: ids %d != %d, want the same repo", l1, l2)
	}
	if l1 == id1 {
		t.Errorf("local-only repo collided with the remote repo (id %d)", l1)
	}

	// Empty remote AND empty root is meaningless.
	if _, err := d.UpsertRepo("", "", 1000); err == nil {
		t.Error("UpsertRepo(\"\",\"\") should error")
	}

	var n int
	if err := d.sql.QueryRow(`SELECT COUNT(*) FROM repos`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("repos rows = %d, want 2 (one remote, one local-only)", n)
	}
}

func TestLinkSessionReposAndLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude.sqlite")
	d, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	id, _ := d.UpsertRepo("gh/owner/proj", "/a/proj", 1000)

	// Link is idempotent on (session_id, repo_id) and refreshes branch.
	if err := d.LinkSessionRepo("sess-1", id, newStr("main"), 1000); err != nil {
		t.Fatalf("LinkSessionRepo: %v", err)
	}
	if err := d.LinkSessionRepo("sess-1", id, newStr("feature"), 2000); err != nil {
		t.Fatalf("LinkSessionRepo (re): %v", err)
	}

	var n int
	if err := d.sql.QueryRow(`SELECT COUNT(*) FROM session_repos`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("session_repos rows = %d, want 1 (idempotent link)", n)
	}

	m, err := d.LoadSessionRepos()
	if err != nil {
		t.Fatalf("LoadSessionRepos: %v", err)
	}
	refs := m["sess-1"]
	if len(refs) != 1 {
		t.Fatalf("refs = %d, want 1", len(refs))
	}
	if refs[0].Remote != "gh/owner/proj" || refs[0].Root != "/a/proj" || refs[0].Branch != "feature" {
		t.Errorf("ref = %+v, want remote=gh/owner/proj root=/a/proj branch=feature (refreshed)", refs[0])
	}
}

func TestLoadSessionReposPrimaryOrdering(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude.sqlite")
	d, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	early, _ := d.UpsertRepo("gh/owner/first", "/first", 1000)
	late, _ := d.UpsertRepo("gh/owner/second", "/second", 2000)
	// Link the later-seen repo first to prove ordering is by first_seen_ts, not
	// insertion order.
	if err := d.LinkSessionRepo("s", late, newStr("b"), 5000); err != nil {
		t.Fatal(err)
	}
	if err := d.LinkSessionRepo("s", early, newStr("a"), 3000); err != nil {
		t.Fatal(err)
	}

	refs := mustLoad(t, d)["s"]
	if len(refs) != 2 {
		t.Fatalf("refs = %d, want 2", len(refs))
	}
	if refs[0].Remote != "gh/owner/first" {
		t.Errorf("primary (element 0) = %q, want gh/owner/first (earliest first_seen)", refs[0].Remote)
	}
}

func mustLoad(t *testing.T, d *DB) map[string][]RepoRef {
	t.Helper()
	m, err := d.LoadSessionRepos()
	if err != nil {
		t.Fatalf("LoadSessionRepos: %v", err)
	}
	return m
}

// TestMigrateIdempotentAndRepoIDRoundTrips reopens a populated DB (exercising the
// ALTER TABLE guards) and confirms sessions.repo_id survives a round trip.
func TestMigrateIdempotentAndRepoIDRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude.sqlite")
	d, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	repoID, _ := d.UpsertRepo("gh/owner/proj", "/a/proj", 1000)
	s := Session{
		SessionID:  "sess-1",
		Cwd:        newStr("/a/proj"),
		RepoID:     sql.NullInt64{Int64: repoID, Valid: true},
		State:      string(state.Idle),
		LastSeenTS: 1000,
		CreatedTS:  1000,
	}
	if err := d.Upsert(s); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	d.Close()

	// Reopen: migrate() runs again over the existing tables (must not error).
	d2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer d2.Close()
	got, found, err := d2.Get("sess-1")
	if err != nil || !found {
		t.Fatalf("Get: found=%v err=%v", found, err)
	}
	if !got.RepoID.Valid || got.RepoID.Int64 != repoID {
		t.Errorf("repo_id = %+v, want %d", got.RepoID, repoID)
	}
}
