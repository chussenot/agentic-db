package niri

import (
	"encoding/json"
	"fmt"
	"os/exec"
)

// This file holds the focus-moving half of the niri IPC: reading what currently
// holds focus, and handing focus to a window or a desktop. It exists for
// `tile-toggle`, which is the only caller that needs to *move* focus rather than
// observe it — the daemon only ever renames workspaces (see model.go's Actuator).
// These are deliberately plain package functions and NOT Actuator methods: that
// interface is the daemon's naming seam, and its recording test fake would
// otherwise have to grow methods it never exercises.

// FocusedWindowID returns the id of the currently focused niri window, or 0 when
// no window holds focus (an empty desktop — `niri msg -j focused-window` prints
// JSON null, which decodes to the zero Window).
func FocusedWindowID() (int, error) {
	cmd := exec.Command("niri", "msg", "-j", "focused-window")
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("niri msg -j focused-window: %w", err)
	}
	var w Window
	if err := json.Unmarshal(out, &w); err != nil {
		return 0, fmt.Errorf("decode niri focused-window: %w", err)
	}
	return w.ID, nil
}

// FocusWindow runs `niri msg action focus-window --id <id>`, moving focus to one
// specific window wherever it lives — this is what a second click on a desktop
// tile does.
func FocusWindow(id int) error {
	return runAction("focus-window", "--id", itoa(id))
}

// FocusWorkspaceByIndex runs `niri msg action focus-workspace <idx>`, the plain
// desktop jump a tile click has always performed. niri resolves a bare index
// against the FOCUSED output, and the reference form takes an index or a name but
// never a workspace id, so there is no id-precise alternative for the unnamed
// desktops — cross with FocusMonitor first to aim at another output's desktop.
func FocusWorkspaceByIndex(idx int) error {
	return runAction("focus-workspace", itoa(idx))
}

// FocusMonitor runs `niri msg action focus-monitor <output>`, moving focus to
// another monitor. It is the missing half of aiming at a specific output's
// desktop, since a bare workspace index only ever resolves on the focused output.
// niri exits 0 and no-ops when the output is disabled or absent.
func FocusMonitor(output string) error {
	return runAction("focus-monitor", output)
}
