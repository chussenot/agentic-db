---
title: Build & install
status: active
date: 2026-08-29
---

# Build & install

Tasks run through [mise](https://mise.jdx.dev) (`mise.toml` is the task
runner — there is no Makefile, by decision; see commit 9724702):

```sh
mise run build           # -> ./claude-status (static, CGO_ENABLED=0, proven by ldd)
mise run test            # go test ./...
mise run check           # the full serial gate CI runs (fmt, vet, lint, test, build, docs)
mise run install         # build + deploy binary, recap-job, timers; flip the daemon
mise run install-units   # (re)install just the recap-job wrapper + systemd timers
mise run uninstall-units # disable and remove the recap timers
```

`mise run install` is idempotent and safe to re-run: it builds, atomically
replaces `~/.local/bin/claude-status`, deploys the recap-job wrapper and
re-enables the systemd timers (via `install-units`), and — **if a
`claude-status daemon` is already running** — stops it and relaunches the new
build detached (otherwise it just updates the binary and leaves startup to
niri's `spawn-at-startup`). The atomic-rename + daemon-flip contract is
recorded in [ADR-0004](adr/0004-install-atomic-replace-and-daemon-flip.md).

Plain `go build` works too; the only external dependency is
[`modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite) (pure Go, no
CGO), so the binary is statically linkable with `CGO_ENABLED=0`:

```sh
CGO_ENABLED=0 go build -o claude-status .
```

> First-time setup (hooks + niri/waybar config) is the binary's own
> `claude-status install` subcommand — distinct from `mise run install`, which
> builds and deploys the binary, recap-job, and timers.

## Scheduled reports

`mise run install` also installs a single `claude-recap-job <daily|weekly>`
wrapper (in `share/`) plus two systemd **user** timers:

- `claude-daily-recap.timer` — daily at 09:00 → `~/dailies/daily-YYYYMMDD.md`
  (from the last covered day through yesterday, so a multi-day gap is caught
  in one run; falls back to the previous day, or Fri–Sun on a Monday).
- `claude-weekly-recap.timer` — Mondays at 09:00 → `~/weeklies/weekly-YYYYMMDD.md`
  (the previous complete calendar week, Mon 00:00 → Sun 23:59).

Both are `Persistent=true`, so a run missed while the laptop was suspended/off
fires at the next power-on (unlike cron). The job waits out the wakeup
resource storm before its LLM call (the weekly waits longer so it doesn't
compete with the daily on Monday mornings), retries if the network isn't up
yet, and holds a per-mode `flock` so overlapping triggers can't clobber a
report.

The narrative comes from a non-deterministic `claude -p` call, so a report can
occasionally come out weak. Iterate on it with `force`, which skips the wait
and the already-present guard (overwriting today's doc); trailing words become
a revision note woven into the prompt (`recap-prompt --note`):

```sh
claude-recap-job daily force                                         # plain retry
claude-recap-job weekly force "keep ci-base and datadog-ci separate"  # steered redo
```

## Testing without a desktop

CI (one job, `mise run check`) exercises everything that runs by fixture or
fake: the niri model, the reconciler, the hook path, the tile payloads.
Anything needing a live niri/waybar/systemd is exercised by hand on the real
desktop — never skipped silently: a test that cannot run in CI says so and
names where it does run.
