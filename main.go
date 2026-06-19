// Command claude-status is a multi-call binary powering a per-workspace "Claude
// activity" indicator in waybar under the niri Wayland compositor.
//
// It dispatches on its first argument to one of several subcommands; see usage.
// This file is the complete, frozen dispatcher — subcommand logic lives in the
// internal packages it delegates to.
package main

import (
	"fmt"
	"os"

	"github.com/zor/claude-status/internal/daemon"
	"github.com/zor/claude-status/internal/doctor"
	"github.com/zor/claude-status/internal/hook"
	"github.com/zor/claude-status/internal/install"
	"github.com/zor/claude-status/internal/waybar"
)

const usage = `claude-status — per-workspace Claude activity indicator for waybar/niri

usage: claude-status <command> [args]

commands:
  hook         handle a Claude Code hook event from stdin (hot path)
  daemon       run the long-lived reconciler
  install      merge our hooks into ~/.claude/settings.json (idempotent)
  uninstall    remove our hook entries (--purge also drops state)
  gc           run a single dead-session reap pass
  gen-waybar   emit waybar format-icons + style.css fragments
  doctor       dump the DB and live niri windows (debugging)
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	args := os.Args[2:]
	var err error
	switch os.Args[1] {
	case "hook":
		err = hook.Run(args)
	case "daemon":
		err = daemon.Run(args)
	case "install":
		err = install.Run(args)
	case "uninstall":
		err = install.RunUninstall(args)
	case "gc":
		err = doctor.RunGC(args)
	case "gen-waybar":
		err = waybar.Run(args)
	case "doctor":
		err = doctor.Run(args)
	default:
		fmt.Fprintf(os.Stderr, "claude-status: unknown command %q\n\n", os.Args[1])
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "claude-status %s: %v\n", os.Args[1], err)
		os.Exit(1)
	}
}
