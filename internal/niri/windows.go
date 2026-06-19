// Package niri talks to the niri Wayland compositor over its IPC, by shelling
// out to the `niri msg` CLI (no IPC library dependency).
//
// This file defines the shared Window type and the one-shot ListWindows
// snapshot used by the hook (to bridge session -> pid -> window -> workspace)
// and by the daemon. It is frozen: the daemon adds eventstream.go / model.go to
// this package but does not edit this file. Window is the shared type those
// files build on.
package niri

import (
	"encoding/json"
	"fmt"
	"os/exec"
)

// Window is one entry of `niri msg -j windows`. Only the fields claude-status
// needs are decoded; niri emits more (layout, focus flags) which we ignore.
type Window struct {
	// ID is the niri window id (stable for the window's lifetime).
	ID int `json:"id"`
	// PID is the client process id. On this system it is the terminal (kitty)
	// pid and is 1:1 with windows, which makes the /proc-ancestor pid match in
	// the hook reliable.
	PID int `json:"pid"`
	// AppID is the Wayland app_id (e.g. "kitty", "firefox").
	AppID string `json:"app_id"`
	// Title is the current window title.
	Title string `json:"title"`
	// WorkspaceID is the id of the workspace the window currently sits on
	// (windows move between workspaces; the daemon tracks this live).
	WorkspaceID int `json:"workspace_id"`
}

// ListWindows returns a snapshot of all niri windows by running
// `niri msg -j windows` and unmarshaling its JSON array. It is a one-shot call
// (the daemon uses the event stream for live updates); the hook calls it at
// most once per resolution.
func ListWindows() ([]Window, error) {
	cmd := exec.Command("niri", "msg", "-j", "windows")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("niri msg -j windows: %w", err)
	}
	var windows []Window
	if err := json.Unmarshal(out, &windows); err != nil {
		return nil, fmt.Errorf("decode niri windows: %w", err)
	}
	return windows, nil
}
