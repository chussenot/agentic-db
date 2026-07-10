# claude-status

A multi-call Go binary that powers a per-workspace "Claude activity" indicator in
[waybar](https://github.com/Alexays/Waybar) under the [niri](https://github.com/YaLTeR/niri)
Wayland compositor.

Claude Code hooks invoke `claude-status hook`, which writes per-session state to a
SQLite database. A long-lived `claude-status daemon` reads that database plus the
niri window model and sets niri workspace **names**, which waybar renders as glyphs:

- **Working** — blinking orange `●`
- **Prompt/waiting** — blinking yellow `?` (Claude needs you)
- **Idle** — a two-cell shade bar that fades over 60 minutes as the session goes stale

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

## Resume a dead session

When a crash kills your sessions, `claude-resume` fuzzy-picks a recent one (across
**all** projects, newest first) and resurrects it. `claude --resume` is scoped to
the directory the session started in, and a subprocess can't change your shell's
cwd — so the picker ships as a **zsh function** that `cd`s into the session's dir
(in your shell, where the `cd` sticks) and then runs `claude --resume`. `fzf` does
the picking; the binary only serves rows + a preview.

`claude-status install` adds this to your `.zshrc` (a marker-delimited, idempotent
block — reversible via `uninstall`, `.bak` kept; if the file is chezmoi-managed it
prints a `chezmoi re-add` reminder). Then, in a new shell:

```sh
claude-resume          # fuzzy-pick + resurrect (cr is a guarded alias, if free)
claude-resume --here   # only sessions from the current repo
claude-resume --all    # include still-running sessions too
```

To wire it up by hand instead of via `install`, add to `~/.zshrc`:

```sh
eval "$(claude-status resume --init zsh)"
```

The preview pane shows each session's topic, repo/branch, cwd, and the tail of its
conversation, so you can tell dead sessions apart before reviving one.

## Recap / reporting

`recap` joins the event log (effort: active time, turns, prompts, streaks) with
the session transcripts under `~/.claude/projects` (intent: Claude's `ai-title`,
your opening ask, project + branch) into a standup-ready markdown digest for a
time window. Active time uses a heartbeat model, so a frozen/missed-`Stop` turn
can't inflate it. The digest is deterministic; the narrative is left to an LLM,
which stays out of the binary so the pipeline is composable:

```sh
claude-status recap --period day | claude -p "$(claude-status recap-prompt --period day)"
```

`recap` alone prints the digest (add `--json` for tooling); `recap-prompt` prints
period-tuned instructions (day = terse standup, week = themes, quarter = narrative).

The digest ends with deterministic `## Metrics` (total wall-clock time,
per-session active min/avg/max, prompts sent, permission prompts) and
`## Project metrics` (per project: the origin remote as `gh/owner/repo` or
`host/path`, and commits landed in the window on the current branch; the branch
is noted when it isn't main/master). Every project that resolves to a git repo
is listed. Git state is read best-effort via `internal/git`, trying each of a
project's session cwds until one resolves — so work done from a since-removed
temp worktree still resolves via its surviving checkout, and a project that
resolves to no repo at all (a non-repo cwd like `$HOME`) is simply dropped. A
repo with no origin shows `(no remote)`.
`--metrics` controls the whole appended block:
`full` (default, append it), `none` (omit — used to feed the LLM so it can't
restate figures), or `only` (just the section — the recap job appends this
verbatim after the LLM narrative).

### Scheduled reports

`make install` also installs a single `claude-recap-job <daily|weekly>` wrapper
(in `dist/`) plus two systemd **user** timers:

- `claude-daily-recap.timer` — daily at 09:00 → `~/dailies/daily-YYYYMMDD.md`
  (previous day, or Fri–Sun on a Monday).
- `claude-weekly-recap.timer` — Mondays at 09:00 → `~/weeklies/weekly-YYYYMMDD.md`
  (the previous complete calendar week, Mon 00:00 → Sun 23:59).

Both are `Persistent=true`, so a run missed while the laptop was suspended/off
fires at the next power-on (unlike cron). The job waits out the wakeup resource
storm before its LLM call (the weekly waits longer so it doesn't compete with the
daily on Monday mornings), retries if the network isn't up yet, and holds a
per-mode `flock` so overlapping triggers can't clobber a report.

The narrative comes from a non-deterministic `claude -p` call, so a report can
occasionally come out weak. Iterate on it with `force`, which skips the wait and
the already-present guard (overwriting today's doc); trailing words become a
revision note woven into the prompt (`recap-prompt --note`):

```sh
claude-recap-job daily force                                         # plain retry
claude-recap-job weekly force "keep ci-base and datadog-ci separate"  # steered redo
```

## Build & install

Two [mise](https://mise.jdx.dev) tasks (see `mise.toml`):

```sh
mise run build     # -> ./claude-status (static, CGO_ENABLED=0)
mise run install   # build, install to ~/.local/bin, and flip a running daemon
```

`mise run install` is idempotent and safe to re-run: it builds, atomically
replaces `~/.local/bin/claude-status`, and — **if a `claude-status daemon` is
already running** — stops it and relaunches the new build detached (otherwise it
just updates the binary and leaves startup to niri's `spawn-at-startup`).

Plain `go build` works too; the only external dependency is
[`modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite) (pure Go, no CGO),
so the binary is statically linkable with `CGO_ENABLED=0`:

```sh
CGO_ENABLED=0 go build -o claude-status .
```

> First-time setup (hooks + niri/waybar config) is the binary's own
> `claude-status install` subcommand — distinct from `mise run install`, which
> only builds and deploys the binary.

## Layout

```
main.go              multi-call dispatch (switch os.Args[1])
internal/state       shared name grammar, decay table, event->state mapping, render tables
internal/db          SQLite layer (schema, Session, Open/Upsert/LoadLive/...)
internal/niri        niri IPC: ListWindows + event-stream client + window/workspace model
internal/hook        hook subcommand (state derivation, /proc->window resolution, audit row)
internal/daemon      daemon subcommand (reconciler: aggregate, slots, decay, GC)
internal/waybar      gen-waybar subcommand (format-icons + style.css generator)
internal/install     install/uninstall subcommands (settings.json hook merge)
internal/doctor      doctor + gc + events subcommands
```
