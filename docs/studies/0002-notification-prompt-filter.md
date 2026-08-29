---
title: "Study: real hook traffic and the Notification prompt filter"
status: active
date: 2026-08-29
---

# Study: real hook traffic and the Notification prompt filter

What live Claude Code hook traffic taught the state machine, as recorded on
`notifyPatterns` in `internal/hook/hook.go` (the operative comment) and in the
`events` audit log design.

## The finding

`Notification` hook events are **not** a "Claude is blocked" signal. Observed
in real traffic:

- Genuine permission/approval requests — the only kind that means Claude is
  blocked waiting on the user.
- An idle nudge ("waiting for your input") that fires ~60s **after** `Stop`,
  while the session is already idle and decaying.
- Assorted others ("build finished", "Claude Code login successful").

Treating every Notification as a prompt **stranded sessions at a steady `?`
that never decayed** — the idle nudge kept re-promoting a session that was
already done. That bug is why the filter exists.

## The rule extracted

A Notification flips a session to `prompt` only when (a) its message matches
one of the tunable `notifyPatterns` substrings (`permission`, `approve`,
`confirm`) **and** (b) the session is mid-work (the state gate in
`internal/hook`). Everything else records the notification in the audit log
but leaves the prior status untouched.

## Why the audit log exists

Diagnosing this class of drift after the fact is exactly what the append-only
`events` table is for (one row per hook, with the message and whether the
filter matched): `claude-status events --session <id>` replays what arrived
and what state was derived. Retune by editing `notifyPatterns`; verify against
the log, not memory.
