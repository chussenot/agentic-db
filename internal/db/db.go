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

// DB wraps the *sql.DB handle. It is safe for concurrent use (database/sql
// pools connections); WAL lets readers proceed without blocking the writer.
type DB struct {
	sql *sql.DB
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
	return &DB{sql: sqldb}, nil
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
  (session_id, cwd, window_id, terminal_pid, state, notify_kind,
   last_talk_ts, last_event_ts, last_seen_ts, created_ts)
VALUES (?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(session_id) DO UPDATE SET
  cwd           = excluded.cwd,
  window_id     = excluded.window_id,
  terminal_pid  = excluded.terminal_pid,
  state         = excluded.state,
  notify_kind   = excluded.notify_kind,
  last_talk_ts  = excluded.last_talk_ts,
  last_event_ts = excluded.last_event_ts,
  last_seen_ts  = excluded.last_seen_ts
`,
		s.SessionID, s.Cwd, s.WindowID, s.TerminalPID, s.State, s.NotifyKind,
		s.LastTalkTS, s.LastEventTS, s.LastSeenTS, s.CreatedTS)
	if err != nil {
		return fmt.Errorf("upsert: %w", err)
	}
	return tx.Commit()
}

const selectCols = `session_id, cwd, window_id, terminal_pid, state, notify_kind,
  last_talk_ts, last_event_ts, last_seen_ts, created_ts`

func scanSession(row interface{ Scan(...any) error }) (Session, error) {
	var s Session
	err := row.Scan(&s.SessionID, &s.Cwd, &s.WindowID, &s.TerminalPID, &s.State,
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
