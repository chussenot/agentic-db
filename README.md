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
| `install` | Idempotently merge our hooks into `~/.claude/settings.json` and print niri/waybar setup fragments. |
| `uninstall` | Remove only our hook entries. `--purge` also drops the state dir + DB. |
| `gc` | Run the dead-session reap pass once and report reaped rows. |
| `gen-waybar` | Emit paste-ready waybar `format-icons` JSON and `style.css` fragments. |
| `doctor` | Dump the DB schema/rows and the live niri windows list (debugging). |
| `events` | Print the audit log (one row per hook), newest first. `--session <id>`, `--limit N`. |

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
