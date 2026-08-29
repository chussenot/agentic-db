---
title: agentic-db
status: active
date: 2026-08-29
---

# agentic-db

A per-workspace "Claude activity" indicator for [waybar](https://github.com/Alexays/Waybar)
under the [niri](https://github.com/YaLTeR/niri) Wayland compositor, built as
a multi-call Go binary (`claude-status`).

> The repository is **agentic-db**; the binary it builds is still `claude-status`
> (as are the Go module and the on-disk DB), so every command below is unchanged.
> Renaming them is a recorded deferral — see the
> [YAGNI register](docs/adr/0003-yagni-deferral-register.md).

## The gap this fills

Running several Claude Code sessions across workspaces gives you no ambient
signal about *which one needs you*. A session can sit blocked on a permission
prompt on workspace 7 while you stare at workspace 2. agentic-db watches every
session and puts the answer where your eyes already are: **working** (blinking
orange `●`), **needs you** (blinking yellow `?`), or **idle** (a shade bar
that fades over 60 minutes) — per workspace, in the bar, plus a richer
per-desktop pwetty tile (the **waybar-pwetty-box** repository).

It is also the durable memory of those sessions: an append-only event log and
per-session history that back `recap` (standup-ready digests of what you and
Claude actually did) and `claude-resume` (resurrect a dead session from any
project).

## The hook hot-path contract

Claude Code hooks invoke `claude-status hook` on every event of a live
session, so the hook can hurt the thing it observes. The contract, load-bearing
across the whole design: the hook does **one SQLite upsert and ALWAYS exits 0**
— a Claude Code hook that can fail blocks the session it observes. Everything
slow, global, or clock-driven (workspace names, decay, the tile cache) belongs
to the single long-lived daemon, the only writer of compositor state. The
reasoning is priced in [ADR-0001](docs/adr/0001-hook-daemon-split.md); the
data flow is diagrammed in [docs/architecture.md](docs/architecture.md).

The pwetty tile's payload contract is `tiles/claude/schema.json` in the
**waybar-pwetty-box** repository.

## The boundary with quivive

agentic-db is the designated long-lived watcher of a small tool family: pact
records, recount explains, quivive says when to look, agentic-db watches the
session and keeps the durable history. quivive reads ledgers and refuses to
run a daemon *because this daemon exists* (its ADR-0002 leans on this repo);
in return, this repo carries the daemon costs — single-writer discipline,
lifecycle, the flip contract — exactly once for the family. See the family
diagram in [docs/architecture.md](docs/architecture.md#the-family).

## Quick start

```sh
mise run install          # build + deploy the binary, recap job, timers; flip the daemon
claude-status install     # one-time: merge hooks into ~/.claude/settings.json + shell setup
```

Everything else lives in docs:

- [CLI reference](docs/cli.md) — the full subcommand table (drift-checked
  against the binary's `--help` by CI), `claude-resume`, and recap/reporting.
- [Build & install](docs/install.md) — mise tasks, the static-binary promise,
  systemd recap timers.
- [Architecture](docs/architecture.md) — data flow and family diagrams,
  package layout.
- [Decision records](docs/adr/0001-hook-daemon-split.md) — the hook/daemon
  split, [SQLite as the store](docs/adr/0002-sqlite-as-the-store.md), the
  [YAGNI deferral register](docs/adr/0003-yagni-deferral-register.md), and the
  [install/flip contract](docs/adr/0004-install-atomic-replace-and-daemon-flip.md).
- [Field studies](docs/studies/0001-tile-hot-path.md) — evidence behind the
  tile hot path and the [Notification prompt filter](docs/studies/0002-notification-prompt-filter.md).
- [CHANGELOG](CHANGELOG.md) — generated record plus hand-written notes;
  versions are cut with `cog bump` only.
