// Package db is the SQLite persistence layer for claude-status. It owns the
// single `sessions` table (current per-session state, no event log) and the
// pragmas tuned for the hook/daemon concurrency model: many short-lived hook
// writers + one long-lived daemon reader, under WAL with IMMEDIATE write
// transactions.
//
// The driver is modernc.org/sqlite (pure Go, registered as "sqlite"), so the
// binary builds with CGO_ENABLED=0.
package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Now is the package clock seam. Tests may override it for deterministic
// timestamps. Production code calls Now() rather than time.Now() directly so
// the override takes effect everywhere.
var Now = time.Now

// schema is the table + index, created on Open if absent. It mirrors the design
// doc exactly. Timestamps are unix milliseconds (time.Time.UnixMilli()).
const schema = `
CREATE TABLE IF NOT EXISTS sessions (
  session_id    TEXT PRIMARY KEY,
  cwd           TEXT,
  window_id     INTEGER,
  terminal_pid  INTEGER,
  state         TEXT NOT NULL,
  notify_kind   TEXT,
  last_talk_ts  INTEGER,
  last_event_ts INTEGER,
  last_seen_ts  INTEGER NOT NULL,
  created_ts    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_window ON sessions(window_id);

-- events is an append-only audit log retained INDEFINITELY: it is not pruned
-- automatically (PruneEvents exists as a manual primitive only). Two producers
-- append here: the hook writes one row per invocation (recording what arrived
-- and what state we derived), and the daemon writes a synthetic
-- event='TitleChanged' row (with window_title set, new_state='unchanged')
-- whenever a live session's niri window title changes. It exists to diagnose
-- state drift (e.g. a session stuck in 'prompt') after the fact and to datamine
-- window titles over time, since the sessions table only holds current state.
CREATE TABLE IF NOT EXISTS events (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  ts          INTEGER NOT NULL,   -- unix ms
  session_id  TEXT NOT NULL,
  event       TEXT NOT NULL,      -- hook_event_name as received
  message     TEXT,               -- Notification message (truncated); NULL otherwise
  matched     INTEGER,            -- 1/0 = prompt filter matched (Notification only); NULL otherwise
  new_state   TEXT NOT NULL,      -- resulting state, or 'unchanged' / 'deleted'
  window_id   INTEGER,            -- resolved niri window id at the time, if any
  window_title TEXT               -- niri window title; set ONLY on event='TitleChanged' rows
);
CREATE INDEX IF NOT EXISTS idx_events_session ON events(session_id);

-- repos is the normalized git repository catalogue. A session's repo is captured
-- once at hook time (see session_repos) instead of re-derived from cwd heuristics
-- at recap time. A repo IS its remote when it has one, else its root_path — the
-- two partial unique indexes enforce that identity without NULLs colliding.
-- root_path (the git worktree toplevel) is also the directory recap runs
-- git rev-list in to count window commits, so it doubles as a live handle.
CREATE TABLE IF NOT EXISTS repos (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  remote        TEXT,             -- normalized (e.g. "gh/owner/repo"); NULL for a local-only repo
  root_path     TEXT,             -- git worktree toplevel; NULL if unknown
  first_seen_ts INTEGER NOT NULL,
  last_seen_ts  INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_repos_remote ON repos(remote) WHERE remote IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_repos_root   ON repos(root_path)
  WHERE remote IS NULL AND root_path IS NOT NULL;

-- session_repos is the DURABLE session<->repo association. Unlike sessions (which
-- are deleted on SessionEnd), these rows are never removed, so recap — which runs
-- over the historical event log — can resolve any past session's repo. The
-- composite primary key makes it N-M ready: a session that works several repos
-- (e.g. via /add-dir) records one row per repo. branch is the branch observed for
-- this session in this repo (NULL if detached/unknown).
CREATE TABLE IF NOT EXISTS session_repos (
  session_id    TEXT NOT NULL,
  repo_id       INTEGER NOT NULL,
  branch        TEXT,
  first_seen_ts INTEGER NOT NULL,
  PRIMARY KEY (session_id, repo_id)
);
CREATE INDEX IF NOT EXISTS idx_session_repos_session ON session_repos(session_id);
`

// Session mirrors one row of the sessions table. NULLable columns use the
// database/sql Null* wrappers so callers can distinguish "unset" (e.g. a remote
// session with no niri window) from a zero value.
type Session struct {
	// SessionID is the Claude Code session_id (primary key).
	SessionID string
	// Cwd is the session working directory (informational).
	Cwd sql.NullString
	// WindowID is the niri window id, cached at SessionStart; NULL if the
	// session could not be resolved to a local niri window (remote/ssh/tmux).
	WindowID sql.NullInt64
	// TerminalPID is the terminal (kitty) pid used for /proc liveness checks;
	// NULL if unresolved.
	TerminalPID sql.NullInt64
	// RepoID is the repos.id resolved from cwd at hook time; NULL until the
	// session's cwd first resolves to a git work tree (or if it never does). It
	// caches the resolution and gates re-resolution; session_repos is the durable
	// record.
	RepoID sql.NullInt64
	// State is one of state.Working / state.Prompt / state.Idle (stored as the
	// backing string). NOT NULL.
	State string
	// NotifyKind optionally classifies a Notification (e.g. "permission");
	// NULL otherwise.
	NotifyKind sql.NullString
	// LastTalkTS is the unix-ms time Claude last finished talking; drives decay.
	// Bumped on Stop/SubagentStop/SessionStart. NULL until first set.
	LastTalkTS sql.NullInt64
	// LastEventTS is the unix-ms time of the most recent hook event.
	LastEventTS sql.NullInt64
	// LastSeenTS is the unix-ms liveness heartbeat, bumped on every hook. NOT NULL.
	LastSeenTS int64
	// CreatedTS is the unix-ms row creation time. NOT NULL.
	CreatedTS int64
}

// Event is one row of the audit log: a single hook invocation and the
// state we derived from it. See the events table in schema.
type Event struct {
	ID        int64
	TS        int64          // unix ms
	SessionID string         // Claude session_id
	Event     string         // hook_event_name as received
	Message   sql.NullString // Notification message (truncated); NULL otherwise
	Matched   sql.NullBool   // prompt filter result (Notification only); NULL otherwise
	NewState  string         // resulting state, or "unchanged" / "deleted"
	WindowID  sql.NullInt64  // resolved niri window id at the time, if any
	// WindowTitle is the niri window title. Set only on event="TitleChanged" rows
	// (written by the daemon when a session's window title changes); NULL on the
	// hook-written rows, which never capture a title (no niri IPC on the hot path).
	WindowTitle sql.NullString
}

// DB wraps the *sql.DB handle. It is safe for concurrent use (database/sql
// pools connections); WAL lets readers proceed without blocking the writer.
type DB struct {
	sql *sql.DB
}

// InsertEvent appends one audit row. It is a single short write on the shared
// handle (no transaction needed — append-only, no read-modify-write). Best
// effort: callers on the hook hot path log-and-swallow any error.
func (d *DB) InsertEvent(e Event) error {
	_, err := d.sql.Exec(`
INSERT INTO events (ts, session_id, event, message, matched, new_state, window_id, window_title)
VALUES (?,?,?,?,?,?,?,?)`,
		e.TS, e.SessionID, e.Event, e.Message, e.Matched, e.NewState, e.WindowID, e.WindowTitle)
	if err != nil {
		return fmt.Errorf("insert event: %w", err)
	}
	return nil
}

// RecentEvents returns up to limit audit rows, newest first. When sessionID is
// non-empty it filters to that session.
func (d *DB) RecentEvents(limit int, sessionID string) ([]Event, error) {
	q := `SELECT id, ts, session_id, event, message, matched, new_state, window_id, window_title FROM events`
	args := []any{}
	if sessionID != "" {
		q += ` WHERE session_id = ?`
		args = append(args, sessionID)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := d.sql.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("recent events: %w", err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.TS, &e.SessionID, &e.Event, &e.Message,
			&e.Matched, &e.NewState, &e.WindowID, &e.WindowTitle); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// EventsBetween returns every audit row with ts in [fromMS, toMS], oldest
// first, across all sessions. The recap aggregator folds these into a windowed
// digest; ascending order lets it reconstruct each session's state timeline in
// one pass. Unlike RecentEvents there is no row cap — a recap window is bounded
// by time, not count.
func (d *DB) EventsBetween(fromMS, toMS int64) ([]Event, error) {
	rows, err := d.sql.Query(`
SELECT id, ts, session_id, event, message, matched, new_state, window_id, window_title
FROM events WHERE ts >= ? AND ts <= ? ORDER BY ts ASC`, fromMS, toMS)
	if err != nil {
		return nil, fmt.Errorf("events between: %w", err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.TS, &e.SessionID, &e.Event, &e.Message,
			&e.Matched, &e.NewState, &e.WindowID, &e.WindowTitle); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// DistinctEventSessionIDs returns every session id that appears in the audit log,
// oldest-first by earliest event. It is the enumeration backfill uses to find
// historical sessions — the sessions table is long gone for ended sessions, but
// the event log persists.
func (d *DB) DistinctEventSessionIDs() ([]string, error) {
	rows, err := d.sql.Query(`
SELECT session_id FROM events GROUP BY session_id ORDER BY MIN(ts) ASC`)
	if err != nil {
		return nil, fmt.Errorf("distinct session ids: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			return nil, fmt.Errorf("scan session id: %w", err)
		}
		out = append(out, sid)
	}
	return out, rows.Err()
}

// RecentSession is one session's activity span, derived purely from the audit
// log: its id and the first/last event timestamps observed for it. It exists so
// callers (the resume picker) can rank historical sessions by recency without
// the sessions table, which is long gone for ended sessions.
type RecentSession struct {
	SessionID string
	FirstTS   int64 // unix ms, earliest event
	LastTS    int64 // unix ms, most recent event (the recency key)
}

// RecentSessions returns up to limit distinct sessions from the event log,
// most-recently-active first (by MAX(ts)). A non-positive limit returns them
// all. Like DistinctEventSessionIDs it reads the append-only log, so it sees
// dead sessions whose sessions-table row was deleted on SessionEnd.
func (d *DB) RecentSessions(limit int) ([]RecentSession, error) {
	q := `SELECT session_id, MIN(ts), MAX(ts) FROM events
GROUP BY session_id ORDER BY MAX(ts) DESC`
	args := []any{}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := d.sql.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("recent sessions: %w", err)
	}
	defer rows.Close()
	var out []RecentSession
	for rows.Next() {
		var s RecentSession
		if err := rows.Scan(&s.SessionID, &s.FirstTS, &s.LastTS); err != nil {
			return nil, fmt.Errorf("scan recent session: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// PruneEvents keeps only the newest keep rows, deleting older ones. It returns
// the number deleted. The audit log is retained indefinitely by default — the
// daemon no longer calls this; it remains a manual primitive for a one-off trim
// if the table ever needs bounding.
func (d *DB) PruneEvents(keep int) (int, error) {
	res, err := d.sql.Exec(
		`DELETE FROM events WHERE id <= (SELECT MAX(id) FROM events) - ?`, keep)
	if err != nil {
		return 0, fmt.Errorf("prune events: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// LatestTitles returns the most recently recorded window title per session,
// read from the newest event='TitleChanged' row of each session. The daemon
// seeds its in-memory last-title map with this on startup so a restart does not
// re-emit a TitleChanged row for every session whose title is unchanged (only
// titles that drifted while the daemon was down produce a fresh row).
func (d *DB) LatestTitles() (map[string]string, error) {
	rows, err := d.sql.Query(`
SELECT session_id, window_title FROM events
WHERE id IN (
  SELECT MAX(id) FROM events WHERE event = 'TitleChanged' GROUP BY session_id
)`)
	if err != nil {
		return nil, fmt.Errorf("latest titles: %w", err)
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var sid string
		var title sql.NullString
		if err := rows.Scan(&sid, &title); err != nil {
			return nil, fmt.Errorf("scan latest title: %w", err)
		}
		if title.Valid {
			out[sid] = title.String
		}
	}
	return out, rows.Err()
}

// SQL exposes the underlying *sql.DB for callers (e.g. the daemon) that need
// custom queries beyond the provided methods.
func (d *DB) SQL() *sql.DB { return d.sql }

// Open opens (creating if needed) the SQLite database at path. It mkdir -p's
// the parent directory, applies the WAL/busy_timeout/synchronous/_txlock
// pragmas from the design doc via the DSN, and creates the schema if absent.
func Open(path string) (*DB, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	dsn := path + "?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(2000)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_txlock=immediate"
	sqldb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if _, err := sqldb.Exec(schema); err != nil {
		sqldb.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	if err := migrate(sqldb); err != nil {
		sqldb.Close()
		return nil, fmt.Errorf("migrate schema: %w", err)
	}
	return &DB{sql: sqldb}, nil
}

// migrate applies additive schema changes to a database created by an older
// version. CREATE TABLE IF NOT EXISTS only builds missing tables, never alters
// existing ones, so a column added to an existing table needs an explicit ALTER.
// ADD COLUMN is cheap (no table rewrite) and idempotent here: a "duplicate
// column name" error means the column already exists, which we swallow.
func migrate(sqldb *sql.DB) error {
	if _, err := sqldb.Exec(`ALTER TABLE events ADD COLUMN window_title TEXT`); err != nil &&
		!strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("add events.window_title: %w", err)
	}
	// repo_id caches the resolved repo on the live row and gates re-resolution in
	// the hook (mirrors window_id): the durable record lives in session_repos.
	if _, err := sqldb.Exec(`ALTER TABLE sessions ADD COLUMN repo_id INTEGER`); err != nil &&
		!strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("add sessions.repo_id: %w", err)
	}
	return nil
}

// Close closes the underlying handle.
func (d *DB) Close() error { return d.sql.Close() }

// Upsert inserts or replaces the row for s.SessionID inside an IMMEDIATE
// transaction (the DSN's _txlock=immediate takes the write lock upfront,
// avoiding BUSY upgrade deadlocks under hook bursts). All columns are written
// from s as given; callers are responsible for populating timestamps.
func (d *DB) Upsert(s Session) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()
	_, err = tx.Exec(`
INSERT INTO sessions
  (session_id, cwd, window_id, terminal_pid, repo_id, state, notify_kind,
   last_talk_ts, last_event_ts, last_seen_ts, created_ts)
VALUES (?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(session_id) DO UPDATE SET
  cwd           = excluded.cwd,
  window_id     = excluded.window_id,
  terminal_pid  = excluded.terminal_pid,
  repo_id       = excluded.repo_id,
  state         = excluded.state,
  notify_kind   = excluded.notify_kind,
  last_talk_ts  = excluded.last_talk_ts,
  last_event_ts = excluded.last_event_ts,
  last_seen_ts  = excluded.last_seen_ts
`,
		s.SessionID, s.Cwd, s.WindowID, s.TerminalPID, s.RepoID, s.State, s.NotifyKind,
		s.LastTalkTS, s.LastEventTS, s.LastSeenTS, s.CreatedTS)
	if err != nil {
		return fmt.Errorf("upsert: %w", err)
	}
	return tx.Commit()
}

const selectCols = `session_id, cwd, window_id, terminal_pid, repo_id, state, notify_kind,
  last_talk_ts, last_event_ts, last_seen_ts, created_ts`

func scanSession(row interface{ Scan(...any) error }) (Session, error) {
	var s Session
	err := row.Scan(&s.SessionID, &s.Cwd, &s.WindowID, &s.TerminalPID, &s.RepoID, &s.State,
		&s.NotifyKind, &s.LastTalkTS, &s.LastEventTS, &s.LastSeenTS, &s.CreatedTS)
	return s, err
}

// LoadLive returns all current session rows. ("Live" reflects that the daemon
// polls this set; liveness reaping is the daemon's job via ReapDead, not a
// filter here.)
func (d *DB) LoadLive() ([]Session, error) {
	rows, err := d.sql.Query(`SELECT ` + selectCols + ` FROM sessions`)
	if err != nil {
		return nil, fmt.Errorf("load: %w", err)
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Get returns the row for sessionID. found is false (with a nil error) when no
// such row exists.
func (d *DB) Get(sessionID string) (s Session, found bool, err error) {
	row := d.sql.QueryRow(`SELECT `+selectCols+` FROM sessions WHERE session_id = ?`, sessionID)
	s, err = scanSession(row)
	if err == sql.ErrNoRows {
		return Session{}, false, nil
	}
	if err != nil {
		return Session{}, false, fmt.Errorf("get: %w", err)
	}
	return s, true, nil
}

// Delete removes the row for sessionID (the SessionEnd clean path). Deleting a
// missing row is not an error.
func (d *DB) Delete(sessionID string) error {
	if _, err := d.sql.Exec(`DELETE FROM sessions WHERE session_id = ?`, sessionID); err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	return nil
}

// ReapDead deletes every row for which predicate returns true and returns the
// number deleted. The predicate lets the caller (the daemon's GC tick) decide
// liveness — e.g. /proc/<terminal_pid> gone, window_id absent from the niri
// model, or last_seen_ts too old. The scan and deletes run against the live
// handle; the daemon is the sole writer so there is no contention.
func (d *DB) ReapDead(predicate func(Session) bool) (int, error) {
	sessions, err := d.LoadLive()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, s := range sessions {
		if predicate(s) {
			if err := d.Delete(s.SessionID); err != nil {
				return n, err
			}
			n++
		}
	}
	return n, nil
}

// nullString maps "" to a NULL string so an unknown remote/root/branch stores as
// NULL rather than the empty string (which the partial unique indexes treat as a
// real value).
func nullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// RepoRef is a repos row joined with the per-session branch, as consumed by
// recap. Remote is the normalized remote ("" for a local-only repo), Root the
// git worktree toplevel, Branch the branch observed for the owning session.
type RepoRef struct {
	Remote string
	Root   string
	Branch string
}

// UpsertRepo records (or refreshes) the repo identified by remote/rootPath and
// returns its id. Identity is the remote when non-empty, else the root_path — a
// SELECT-then-INSERT under an IMMEDIATE tx (rather than ON CONFLICT) so the two
// partial unique indexes don't need distinct conflict targets. A repeated repo
// bumps last_seen_ts; a first sighting inserts. It is best-effort input: an empty
// remote AND empty rootPath is meaningless and returns an error.
func (d *DB) UpsertRepo(remote, rootPath string, ts int64) (int64, error) {
	if remote == "" && rootPath == "" {
		return 0, fmt.Errorf("upsert repo: empty remote and root_path")
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	rem := nullString(remote)
	root := nullString(rootPath)

	// Identity lookup: by remote when we have one, else by root_path among the
	// local-only (remote IS NULL) rows.
	var (
		id  int64
		sel *sql.Row
	)
	if remote != "" {
		sel = tx.QueryRow(`SELECT id FROM repos WHERE remote = ?`, rem)
	} else {
		sel = tx.QueryRow(`SELECT id FROM repos WHERE remote IS NULL AND root_path = ?`, root)
	}
	switch err := sel.Scan(&id); err {
	case nil:
		// Refresh last_seen_ts, and fill root_path if we now know it and didn't.
		if _, err := tx.Exec(`UPDATE repos SET last_seen_ts = ?,
		    root_path = COALESCE(root_path, ?) WHERE id = ?`, ts, root, id); err != nil {
			return 0, fmt.Errorf("touch repo: %w", err)
		}
	case sql.ErrNoRows:
		res, err := tx.Exec(`INSERT INTO repos (remote, root_path, first_seen_ts, last_seen_ts)
		    VALUES (?,?,?,?)`, rem, root, ts, ts)
		if err != nil {
			return 0, fmt.Errorf("insert repo: %w", err)
		}
		if id, err = res.LastInsertId(); err != nil {
			return 0, fmt.Errorf("repo id: %w", err)
		}
	default:
		return 0, fmt.Errorf("select repo: %w", err)
	}
	return id, tx.Commit()
}

// LinkSessionRepo records the durable association between a session and a repo
// (one row per repo, N-M ready). It is idempotent on (session_id, repo_id) and
// refreshes the branch. Unlike the sessions row, this survives SessionEnd so
// recap can resolve historical sessions.
func (d *DB) LinkSessionRepo(sessionID string, repoID int64, branch sql.NullString, ts int64) error {
	_, err := d.sql.Exec(`
INSERT INTO session_repos (session_id, repo_id, branch, first_seen_ts)
VALUES (?,?,?,?)
ON CONFLICT(session_id, repo_id) DO UPDATE SET branch = excluded.branch`,
		sessionID, repoID, branch, ts)
	if err != nil {
		return fmt.Errorf("link session_repo: %w", err)
	}
	return nil
}

// LoadSessionRepos returns every session's repos, keyed by session_id, in a
// single join. first_seen_ts orders the slice so the primary (earliest-seen)
// repo of a session is element 0 — recap groups by that one for now.
func (d *DB) LoadSessionRepos() (map[string][]RepoRef, error) {
	rows, err := d.sql.Query(`
SELECT sr.session_id, r.remote, r.root_path, sr.branch
FROM session_repos sr JOIN repos r ON r.id = sr.repo_id
ORDER BY sr.session_id, sr.first_seen_ts, sr.repo_id`)
	if err != nil {
		return nil, fmt.Errorf("load session_repos: %w", err)
	}
	defer rows.Close()
	out := map[string][]RepoRef{}
	for rows.Next() {
		var sid string
		var remote, root, branch sql.NullString
		if err := rows.Scan(&sid, &remote, &root, &branch); err != nil {
			return nil, fmt.Errorf("scan session_repo: %w", err)
		}
		out[sid] = append(out[sid], RepoRef{
			Remote: remote.String, Root: root.String, Branch: branch.String,
		})
	}
	return out, rows.Err()
}

// DefaultDBPath returns the canonical database path,
// $XDG_STATE_HOME/claude-status/claude.sqlite, falling back to
// ~/.local/state/claude-status/claude.sqlite when XDG_STATE_HOME is unset.
func DefaultDBPath() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "claude-status", "claude.sqlite")
}
