---
title: "Study: the tile hot path — from per-render IPC to a daemon-written cache"
status: active
date: 2026-08-29
---

# Study: the tile hot path — from per-render IPC to a daemon-written cache

Field findings behind the tile cache design, consolidated from the evidence
recorded in-tree (the `internal/tile` package comment and the commit log).
Raw captures from the live desktop belong in this directory as they are
gathered; screenshots of the rendered tile go in `docs/media/`.

## Finding 1: per-render `niri msg` spawns stutter the compositor

A waybar `cffi/pwetty#i` module reruns its producer every interval, **once per
tile**. The original `tile-data` did a live niri IPC query per run; as the
`internal/tile` package comment records:

> a regression once hammered niri with `niri msg` IPC and stuttered desktop
> switches

The fix shape: the daemon — which already holds the niri model and live
sessions — precomputes **every** desktop's payload on its tick and writes one
cache file; `tile-data` becomes a single file read (no `niri msg` spawn, no DB
open). The live path survives only as a fallback when no daemon has ever
written the cache.

## Finding 2: a read error must not fabricate idleness (commit `b0751da`)

`tile-watch` initially emitted the empty placeholder on any cache read error,
silently replacing previously-published live state — e.g. **a prompt session
masked by plausible idleness** — and because payloads are string-deduped, the
replacement was never repaired for states with no time-varying field. Rule
extracted from the fix: after a first successful emit, a read error keeps the
last published line; the placeholder is only correct before the daemon has
ever written the cache.

## Finding 3: an empty desktop is not an idle session (commit `c0df78e`)

The empty-desktop placeholder originally synthesized a fully-decayed idle
session to satisfy the `sessions[]` contract — rendering a maxed-out two-cell
pause bar on every desktop with nothing open. The contract now carries a
distinct `state="empty"` (a dim hollow ring). Lesson: don't satisfy a schema
by faking a semantically different state.

## Finding 4: app identity in the wild is messy (commits `299d53b`, `1c26d14`)

Real hook/window traffic keeps producing identity edge cases the clean model
misses: snap-packaged apps hand niri a free-form display name ("Proton Mail")
instead of a reverse-DNS app_id and hide their icon outside every theme dir
(resolved via snapd's desktop entries); Claude Code v2.1.x daemonized sessions
so the hook's `/proc` ancestry walk dead-ended at pid 1 (resolved by matching
client processes by cwd, binding **only** on an unambiguous single-window
match — a wrong binding is worse than no binding).
