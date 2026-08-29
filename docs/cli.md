---
title: CLI reference
status: active
date: 2026-08-29
---

# CLI reference

`claude-status` is a multi-call binary: the first argument selects a
subcommand (`main.go` is the complete, frozen dispatcher). This table is the
documented CLI surface; `scripts/check-docs` diffs it against the binary's own
usage output in both directions, so a row here that the binary rejects — or a
subcommand the binary grows that this table lacks — fails `mise run check`.

## Subcommands

| Command | Description |
|---|---|
| `hook` | Hot-path hook handler. Reads a Claude Code hook JSON event from stdin, derives session state, and upserts one row in the SQLite DB. Always exits 0. |
| `daemon` | Long-lived reconciler. Watches the DB + niri event stream and sets/unsets workspace names. |
| `install` | Idempotently merge our hooks into `~/.claude/settings.json`, add the `claude-resume` block to `.zshrc`, and print niri/waybar setup fragments. `--no-shell` skips the `.zshrc` edit; `--zshrc <path>` overrides its location. |
| `uninstall` | Remove only our hook entries and the `.zshrc` block. `--purge` also drops the state dir + DB; `--no-shell` leaves the `.zshrc` block. |
| `gc` | Run the dead-session reap pass once and report reaped rows. |
| `gen-waybar` | Emit paste-ready waybar `format-icons` JSON and `style.css` fragments. |
| `doctor` | Dump the DB schema/rows and the live niri windows list (debugging). |
| `events` | Print the audit log (one row per hook), newest first. `--session <id>`, `--limit N`. |
| `recap` | Summarize a time window of Claude sessions (topics, effort, streaks, metrics). `--period day\|week\|quarter`, `--since`, `--json`, `--metrics full\|none\|only`. |
| `recap-prompt` | Emit the LLM instructions that turn a recap into a written report. `--period`. |
| `resume` | Backend for the `claude-resume` picker: `--list` (fzf rows), `--show <id>` (preview), `--init zsh` (shell integration). Bare prints setup help. |
| `repo-backfill` | One-off, idempotent pass that backfills stored git repos for historical sessions (so old rows resolve in recap project metrics). |
| `tile-data` | Emit the pwetty `claude`-tile JSON for one niri desktop, keyed by its per-output workspace index. Reads the daemon's precomputed tile cache (falls back to a live build when no daemon is up); always prints valid JSON. `--output` overrides the niri connector. |
| `tile-watch` | Like `tile-data` but streams the payload for one desktop on every change (for a pwetty `stream: true` module) instead of printing once. |

The tile payload contract lives in the **waybar-pwetty-box** repository at
`tiles/claude/schema.json` (cited by repo — not by any one machine's
filesystem layout).

## Resume a dead session

When a crash kills your sessions, `claude-resume` fuzzy-picks a recent one
(across **all** projects, newest first) and resurrects it. `claude --resume`
is scoped to the directory the session started in, and a subprocess can't
change your shell's cwd — so the picker ships as a **zsh function** that `cd`s
into the session's dir (in your shell, where the `cd` sticks) and then runs
`claude --resume`. `fzf` does the picking; the binary only serves rows + a
preview.

`claude-status install` adds this to your `.zshrc` (a marker-delimited,
idempotent block — reversible via `uninstall`, `.bak` kept; if the file is
chezmoi-managed it prints a `chezmoi re-add` reminder). Then, in a new shell:

```sh
claude-resume          # fuzzy-pick + resurrect (cr is a guarded alias, if free)
claude-resume --here   # only sessions from the current repo
claude-resume --all    # include still-running sessions too
```

To wire it up by hand instead of via `install`, add to `~/.zshrc`:

```sh
eval "$(claude-status resume --init zsh)"
```

The preview pane shows each session's topic, repo/branch, cwd, and the tail of
its conversation, so you can tell dead sessions apart before reviving one.

## Recap / reporting

`recap` joins the event log (effort: active time, turns, prompts, streaks)
with the session transcripts under `~/.claude/projects` (intent: the session's
`ai-title`, your opening ask, project + branch) into a standup-ready markdown
digest for a time window. Active time uses a heartbeat model, so a
frozen/missed-`Stop` turn can't inflate it. The digest is deterministic; the
narrative is left to an LLM, which stays out of the binary so the pipeline is
composable:

```sh
claude-status recap --period day | claude -p "$(claude-status recap-prompt --period day)"
```

`recap` alone prints the digest (add `--json` for tooling); `recap-prompt`
prints period-tuned instructions (day = terse standup, week = themes,
quarter = narrative).

The digest ends with deterministic `## Metrics` (total wall-clock time,
per-session active min/avg/max, prompts sent, permission prompts) and
`## Project metrics` (per project: the origin remote as `gh/owner/repo` or
`host/path`, and commits landed in the window on the current branch; the
branch is noted when it isn't main/master). Every project that resolves to a
git repo is listed. Git state is read best-effort via `internal/git`, trying
each of a project's session cwds until one resolves — so work done from a
since-removed temp worktree still resolves via its surviving checkout, and a
project that resolves to no repo at all (a non-repo cwd like `$HOME`) is
simply dropped. A repo with no origin shows `(no remote)`.

`--metrics` controls the whole appended block: `full` (default, append it),
`none` (omit — used to feed the LLM so it can't restate figures), or `only`
(just the section — the recap job appends this verbatim after the LLM
narrative).
