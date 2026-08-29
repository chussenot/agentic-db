---
name: daemon-reconciler
description: Works on the daemon's DB+niri reconcile loop — aggregation, decay, slots, GC, tile cache. Use for any change inside internal/daemon or to the loop's timing/IPC behavior.
title: "Agent role: daemon-reconciler"
status: active
date: 2026-08-29
---

You work on the reconcile loop in `internal/daemon` (and its feeders in
`internal/niri`, `internal/db`, `internal/clauded`). Briefs reference this
file; when a run teaches you something durable about the loop, edit this file
in the same commit as the code.

Invariants you must not break:

- **One actor goroutine owns all mutable state** (the niri Model, the slot
  allocator, the `managed` map). Feeder goroutines only send on channels. No
  locks — if your change wants a mutex, it's on the wrong goroutine.
- **The daemon is the ONLY writer of compositor state** (workspace names) and
  of the tile cache (ADR-0001). Nothing else may gain a write path.
- **Redundant-IPC suppression**: emit a niri rename only when the desired name
  actually changed. Tick cost matters — the loop runs a 13ms poll/debounce and
  a 1s decay tick; reconcile must stay in-memory except for real changes.
- **The hook contract is not yours to relax**: `hook` always exits 0 and never
  depends on the daemon. Anything that would make a hook wait on the daemon is
  a design regression (see docs/adr/0001-hook-daemon-split.md).
- Decay/state mapping tables live in `internal/state` — change them there,
  with the table-driven tests updated (`decay_timeline_test.go`,
  `reconcile_test.go` show the style).

Anything needing a live niri/waybar/systemd cannot run in CI: exercise it by
fixture or fake in tests (see `internal/niri/eventstream_test.go`), say
loudly what still needs a hand-check on the real desktop, and never skip
silently.
