# agentic-db

A multi-call Go binary (`claude-status`) that powers a per-workspace "Claude
activity" indicator in [waybar](https://github.com/Alexays/Waybar) under the
[niri](https://github.com/YaLTeR/niri) Wayland compositor.

> The repository is **agentic-db**; the binary it builds is still `claude-status`
> (as are the Go module and the on-disk DB), so every command below is unchanged.

Claude Code hooks invoke `claude-status hook`, which writes per-session state to a
SQLite database. A long-lived `claude-status daemon` reads that database plus the
niri window model and drives two renderers — of which only the second is still
wired up:

- **Workspace-name glyphs — VESTIGIAL.** The daemon sets niri workspace **names**,
  which waybar used to render directly as glyphs (blinking orange `●` working,
  blinking yellow `?` prompt, a two-cell shade bar fading over 60 minutes when
  idle). That bar configuration is retired: nothing consumes the names any more,
  and background agents are deliberately NOT fed into them. The code is kept
  dormant rather than deleted, so the dots could come back, but treat the tile as
  the renderer that matters — `aggregate`, the slot allocator, `internal/state`'s
  name grammar and `gen-waybar` all serve this path alone.
- **The pwetty `claude` tile — LIVE.** On each tick the daemon also precomputes a
  per-desktop payload and writes it to a tile cache. `claude-status tile-data`
  (and `tile-watch`) then serve that cache as the JSON a pwetty-hosted waybar
  tile expects — a cheap file read on the hot path, no `niri msg` spawn per
  render. Contract: `~/Perso/pwetty-box-rs/tiles/claude/schema.json`.
  A payload is the desktop's shortcut plus an `items` array of 1–2 rows, each
  row either a Claude session or an ordinary window — so a desktop holding a
  session *and* a browser shows both. The row count picks the tile's layout
  (1 → rich single, 2 → stacked dual); choosing **which** two rows a busier
  desktop shows is ours (prompt first, sessions over plain windows).

## Subcommands

| Command | Description |
|---|---|
| `hook` | Hot-path hook handler. Reads a Claude Code hook JSON event from stdin, derives session state, and upserts one row in the SQLite DB. Always exits 0. |
| `daemon` | Long-lived reconciler. Watches the DB + niri event stream and sets/unsets workspace names. |
| `install` | Idempotently merge our hooks into `~/.claude/settings.json`, add the `claude-resume` block to `.zshrc`, and print niri/waybar setup fragments. `--no-shell` skips the `.zshrc` edit; `--zshrc <path>` overrides its location. |
| `uninstall` | Remove only our hook entries and the `.zshrc` block. `--purge` also drops the state dir + DB; `--no-shell` leaves the `.zshrc` block. |
| `gc` | Run the dead-session reap pass once and report reaped rows. `--orphans` instead sweeps windowless rows Claude Code no longer lists (dry run; `--yes` deletes, `--orphan-age` sets the floor). |
| `gen-waybar` | Emit paste-ready waybar `format-icons` JSON and `style.css` fragments. |
| `doctor` | Dump the DB schema/rows and the live niri windows list (debugging). |
| `fleet` | Show background agents, who owns them, what they are waiting on, and each session's own-vs-fleet idle age. `--json`, `--cross-check` (diff against `claude agents --json`). |
| `events` | Print the audit log (one row per hook), newest first. `--session <id>`, `--limit N`. |
| `recap` | Summarize a time window of Claude sessions (topics, effort, streaks, metrics). `--period day\|week\|quarter`, `--since`, `--json`, `--metrics full\|none\|only`. |
| `recap-prompt` | Emit the LLM instructions that turn a recap into a written report. `--period`. |
| `resume` | Backend for the `claude-resume` picker: `--list` (fzf rows), `--show <id>` (preview), `--init zsh` (shell integration). Bare prints setup help. |
| `repo-backfill` | One-off, idempotent pass that backfills stored git repos for historical sessions (so old rows resolve in recap project metrics). |
| `tile-data` | Emit the pwetty `claude`-tile JSON for one niri desktop, keyed by its per-output workspace index. Reads the daemon's precomputed tile cache (falls back to a live build when no daemon is up); always prints valid JSON. `--output` overrides the niri connector. |
| `tile-watch` | Like `tile-data` but streams the payload for one desktop on every change (for a pwetty `stream: true` module) instead of printing once. |

## Background agents

Everything above assumes a session lives in a terminal window. Background agents do
not, and that breaks enough assumptions to be worth spelling out.

Two different things get called "background":

- a **background session** — `claude --bg`, a `/`-command, a colony dispatch. Its own
  process, its own `session_id`, and it fires all our hooks, so it has always had a
  row. What it does not have is a terminal: `window_id` and `terminal_pid` are
  permanently NULL, and those are the axes the glyphs, the tiles and the reap logic
  are built on.
- an **in-process task** — the `Agent`, `Monitor` and backgrounded `Bash` tools. No
  process and no session id at all. Tracked only as a background session's in-flight
  count today; for interactive parents it is not yet tracked (see the beads).

A background session announces itself in `~/.claude/sessions/<pid>.json` with
`kind: "bg"` and a `jobId`, which keys a much richer record at
`~/.claude/jobs/<jobId>/state.json`: lifecycle, a human `detail` line, in-flight
tasks, and `needs` — verbatim what it is waiting on from you. `internal/fleet` reads
those (mtime-gated, and only for sessions the status files prove alive, because dead
job directories linger on disk for months).

**`needs` is the signal, not the lifecycle.** A job reports `working|blocked|done`,
but "blocked" covers both "blocked on you" and "blocked on another agent". Only a
non-empty `needs` means a person is required — and on a live colony, two agents
asking a question ("confirm: …", "A or B") had a completely EMPTY first-party
`waitingFor`, so keying on the status file alone loses those asks.

**Two ownership edges, tracked separately**, because they are demonstrably different
sessions:

- `owner_session_id` — who spawned the fleet, from `CLAUDE_PID` in the agent's own
  environment. (Caveat: agents are claimed from a pre-forked spare pool, so this
  names the fleet ROOT, not the immediate parent. Good for "whose fleet is this",
  useless for "which agent spawned which".) When the spawner has exited — agents
  outlive their parent — attribution re-homes on the job's parent checkout.
- `console_session_id` — who was last told an agent needs input, from the
  `<agent> needs your input: <ask>` notifications. That is the desktop you would
  actually walk to. On the machine this was built for, a fleet spawned from one
  desktop was being watched from another, whose tile cheerfully read 19h idle.

Which explains the other trap: **a parent's own status file freezes while its
children work**, because its main loop is suspended on them. So `last_talk_ts` says
"19 hours" while six agents are mid-flight. `claude-status fleet` prints both
numbers side by side:

```sh
claude-status fleet          # OWN_IDLE vs FLEET_IDLE per session, plus every agent
claude-status fleet --json   # same, for tooling
```

### On the tile

A third kind of desktop turned out to matter most: the one running `claude agents`.
That window is a **dashboard, not a conversation**, so describing it by its host
session was the worst reading of all — the session can be a day idle while eleven
agents move in front of you. A console is detected from the view's own title
(`… · claude agents`) and the daemon keeps that marker **sticky** per window, because
the view retitles its terminal to the focused agent as you scroll; deriving it from
the current title alone makes the desktop flip back to "idle 22h" mid-navigation.

So a desktop gets a fleet row when it is a console (showing every agent on the
machine — the view is machine-wide) or when its session spawned agents (showing its
own). A console row *replaces* the host session's row; an owner row sits beside the
conversation. A console suppresses owned rows on the same desktop, since the whole set
already contains them.

The row is deliberately a **valid session row** carrying an additive `fleet` object:
the contract's `kind` is a closed enum and rows are discriminated by `state`, so a
`kind: "fleet"` row would be schema-invalid and might not paint. Its `state` is
truthful — `prompt` when any agent waits on you (which is what pulses the tile),
`working`, `shell` for a fleet blocked on itself, else `idle` aged on the *fleet's*
newest activity. A fleet-unaware template therefore renders it sanely today; drawing
the bead strip is `claude-status-db-7d8.2` in pwetty-box-rs.

The workspace-name glyphs are **not** fed and won't be: those dots are dead config.

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

`mise run install` also installs a single `claude-recap-job <daily|weekly>`
wrapper (in `share/`) plus two systemd **user** timers:

- `claude-daily-recap.timer` — daily at 09:00 → `~/dailies/daily-YYYYMMDD.md`
  (from the last covered day through yesterday, so a multi-day gap is caught in
  one run; falls back to the previous day, or Fri–Sun on a Monday).
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

[mise](https://mise.jdx.dev) tasks (see `mise.toml`):

```sh
mise run build           # -> ./claude-status (static, CGO_ENABLED=0)
mise run test            # go test ./...
mise run install         # build + deploy binary, recap-job, timers; flip the daemon
mise run install-units   # (re)install just the recap-job wrapper + systemd timers
mise run uninstall-units # disable and remove the recap timers
```

`mise run install` is idempotent and safe to re-run: it builds, atomically
replaces `~/.local/bin/claude-status`, deploys the recap-job wrapper and
re-enables the systemd timers (via `install-units`), and — **if a
`claude-status daemon` is already running** — stops it and relaunches the new
build detached (otherwise it just updates the binary and leaves startup to
niri's `spawn-at-startup`).

Plain `go build` works too; the only external dependency is
[`modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite) (pure Go, no CGO),
so the binary is statically linkable with `CGO_ENABLED=0`:

```sh
CGO_ENABLED=0 go build -o claude-status .
```

> First-time setup (hooks + niri/waybar config) is the binary's own
> `claude-status install` subcommand — distinct from `mise run install`, which
> builds and deploys the binary, recap-job, and timers.

## Layout

```
main.go              multi-call dispatch (switch os.Args[1])
internal/state       shared name grammar, decay table, event->state mapping, render tables
internal/db          SQLite layer (schema, Session, Open/Upsert/LoadLive/...)
internal/niri        niri IPC: ListWindows + event-stream client + window/workspace model
internal/hook        hook subcommand (state derivation, /proc->window resolution, audit row)
internal/clauded     reads Claude Code's first-party per-session status files (busy/idle/waiting)
internal/fleet       reads background-agent job records; ownership + fleet roll-up
internal/daemon      daemon subcommand (reconciler: aggregate, slots, decay, GC, tile cache)
internal/tile        tile-data/tile-watch subcommands: pwetty tile payload model + cache
internal/waybar      gen-waybar subcommand (format-icons + style.css generator)
internal/install     install/uninstall subcommands (settings.json hook merge)
internal/doctor      doctor + fleet + gc + events subcommands
internal/recap       recap/recap-prompt/repo-backfill subcommands (digest + LLM instructions)
internal/transcript  reads session transcripts (ai-title, opening prompt, cwd)
internal/git         best-effort git CLI wrapper for recap project metrics
internal/resume      resume subcommand (claude-resume rows/preview + zsh integration)
```
