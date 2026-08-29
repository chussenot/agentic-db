---
title: "ADR-0004: Install by atomic rename; flip the daemon via /proc argv"
status: active
date: 2026-08-29
decision-makers: [chussenot]
supersedes: none
---

# ADR-0004: Install by atomic rename; flip the daemon via /proc argv

The decision was made — and reasoned through — in the `install` task in
`mise.toml`. Its comments are the operative record; this ADR **lifts** them so
the decision is findable next to the other ADRs. If the task and this page
ever disagree, the task is right and this page is stale.

## The contract, as the task states it

On replacing the installed binary:

> Atomic replace: copy then rename over `$dest`. rename swaps the directory
> entry, so it is safe even if a daemon is currently exec'ing the old inode
> (no ETXTBSY).

On finding the daemon to flip:

> Find a running daemon by its argv via /proc (NOT the process list), so this
> never matches the task's own shell. Transient `hook` invocations are
> skipped.

On the handover between old and new daemon:

> Bounded wait for the old daemon to exit before starting the new one, so
> they don't briefly both reconcile. No sleep (keeps it harness-friendly).

And when no daemon is running, the task deliberately does **not** start one:
the binary is updated and startup stays owned by niri's `spawn-at-startup`.

## What the contract leans on

- **A static binary** (ADR-0002): the rename trick is only whole because the
  new binary carries no shared-library dependencies to skew against a running
  process. `mise run build` proves this with an `ldd` check on every build.
- **A single daemon** (ADR-0001): "kill the one whose argv is
  `claude-status daemon`" is only well-defined because the daemon is the
  family's one long-lived watcher; the /proc argv match is what keeps
  transient `hook` processes and the installer's own shell out of the blast
  radius.

## Alternatives, priced

- **`cp` over the destination** — writes into the live inode: ETXTBSY on a
  running daemon, or worse, a torn text segment.
- **`pkill claude-status`** — matches by name, which the task's own comment
  rules out: it can catch the installer's shell and in-flight `hook`
  invocations whose only sin is sharing the binary.
- **systemd-managed daemon (`systemctl restart`)** — clean flips for the cost
  of moving daemon lifecycle away from niri `spawn-at-startup`, where session
  startup already lives; revisit only if the daemon gains reasons to be
  supervised (crash-looping it currently doesn't do).
