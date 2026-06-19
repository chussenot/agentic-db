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

## Build

```sh
CGO_ENABLED=0 go build -o claude-status .
```

The only external dependency is [`modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite)
(pure Go, no CGO) so the binary is statically linkable with `CGO_ENABLED=0`.

## Layout

```
main.go              multi-call dispatch (switch os.Args[1])
internal/state       shared name grammar, decay table, event->state mapping, render tables
internal/db          SQLite layer (schema, Session, Open/Upsert/LoadLive/...)
internal/niri        niri IPC (ListWindows; daemon adds eventstream/model)
internal/hook        hook subcommand (stub)
internal/daemon      daemon subcommand (stub)
internal/waybar      gen-waybar subcommand (stub)
internal/install     install/uninstall subcommands (stub)
internal/doctor      doctor subcommand
```
