---
title: "ADR-0001: The hook/daemon split"
status: active
date: 2026-08-29
decision-makers: [chussenot]
supersedes: none
---

# ADR-0001: The hook/daemon split

This ADR writes down what the code already decided; the operative statements
of the contract live as package comments in `internal/hook` and
`internal/daemon`, and this record must stay consistent with them.

## Context

Claude Code fires hooks on every event of a live session. The indicator must
observe those sessions without ever affecting them: a hook that exits non-zero
or hangs **blocks the very session it is observing**. On the output side, niri
workspace names are global compositor state — concurrent writers race, and a
correct decay display needs a clock (idle sessions fade over 60 minutes), but
hooks only run when events happen.

## Decision

Split the binary's runtime into two roles with one boundary between them (the
SQLite DB, ADR-0002):

- **`hook` is the hot path.** It does one SQLite upsert (plus one audit row)
  and **ALWAYS exits 0**: every error is logged to a ring-buffered file next
  to the DB and swallowed. Budget < 50ms. It never talks to niri except the
  one-time `/proc`-walk window resolution on `SessionStart`, and it never
  depends on the daemon being alive.
- **The daemon is the single long-lived reconciler and the only writer of
  compositor state.** It owns the niri model (event stream), polls the DB,
  advances decay buckets on its tick, reaps dead sessions, and is the sole
  mutator of workspace names and of the tile cache. Single-actor concurrency,
  redundant-IPC suppression (rename only on actual change).

## Why a daemon is RIGHT here when quivive's ADR-0002 refuses one

quivive's ADR-0002 refuses a daemon and stays a reader of ledgers — and that
refusal **leans on this repo existing**. The family carries exactly one
long-lived watcher, and agentic-db is its designated seat: decay needs a
clock, workspace names need a single writer, the tile cache needs a producer
that already holds the niri model — so the daemon costs (lifecycle,
supervision, single-writer discipline) are paid once, here. Every other family
member gets to read durable state this daemon maintains instead of growing a
watcher of its own. Refusing a daemon here would not remove the daemon; it
would smear it across N hook processes and a polling waybar exec.

## Alternatives, priced

- **Hooks write workspace names directly** — N racing writers on global
  compositor state; niri IPC lands on the hot path (the exact cost class the
  tile regression measured, see
  [study 0001](../studies/0001-tile-hot-path.md)); and no process exists to
  advance decay between events, so idle fade simply cannot render.
- **waybar `exec` polling the DB per render** — a process spawn (and a DB
  open) per bar refresh per tile; this was tried in spirit by the pre-cache
  tile path and stuttered desktop switches. Rejected by field evidence.
- **systemd timer instead of a daemon** — 1s-at-best granularity, no niri
  event stream (topology changes render late), and it still needs the
  single-writer discipline, so it saves nothing but the process while costing
  responsiveness.
- **One merged process (hook logic inside the daemon, hooks as IPC clients)**
  — the hook would then depend on daemon liveness, violating the always-exits-0
  contract on the days the daemon is down; SQLite already is the IPC.
