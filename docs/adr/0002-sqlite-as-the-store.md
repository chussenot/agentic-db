---
title: "ADR-0002: SQLite as the store"
status: active
date: 2026-08-29
decision-makers: [chussenot]
supersedes: none
---

# ADR-0002: SQLite as the store

## Context

The hook/daemon split (ADR-0001) needs one shared boundary: many short-lived
hook writers (one per Claude Code event, concurrent across sessions) and one
long-lived daemon reader, on a single machine, at human-scale write rates. The
hook must be able to write with **no daemon running**, and the daemon must
read the full live set sub-millisecond at a 13ms poll.

## Decision

SQLite, via `modernc.org/sqlite` — the **pure-Go** driver — with WAL mode and
IMMEDIATE write transactions (pragmas owned by `internal/db`). The DB holds
the `sessions` current-state table plus the append-only `events` audit log.

The driver choice carries a load-bearing consequence: **no CGO**. The binary
builds with `CGO_ENABLED=0` and is fully static, which is exactly what the
installer's atomic-rename + daemon-flip contract (ADR-0004) assumes — a rename
can swap the directory entry under a running daemon only because the new
binary drags no shared-library state with it. `mise run build` *proves* the
binary is static (ldd check) rather than remembering it.

## Alternatives, priced

- **A flat JSON/state file** — no concurrent upsert story: N hooks re-writing
  one file means torn writes or a hand-rolled lock protocol; no indexed reads
  for the daemon; the audit log would grow into exactly the append/compact
  machinery SQLite already ships.
- **IPC to the daemon (unix socket) as the store** — the hook then fails when
  the daemon is down, violating the always-exits-0 contract; state would need
  separate persistence anyway for daemon restarts.
- **mattn/go-sqlite3 (CGO driver)** — faster, but drags CGO into the build:
  dynamic linking (or cross-compile pain), and the static-binary promise the
  flip contract depends on is gone. The write rate here is human-scale; the
  pure-Go driver's overhead is unmeasurable at this load.
- **A server database (Postgres) or Dolt** — a server dependency for a
  single-desktop indicator; absurd operationally. (Beads uses Dolt for issue
  sync — a different job with different sync needs.)
