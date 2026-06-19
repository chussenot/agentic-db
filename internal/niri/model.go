// This file (added by the daemon, alongside the frozen windows.go) holds the
// in-memory niri model — the live window/workspace topology rebuilt from the
// event stream — plus the IPC "actuators" that rename workspaces. It is a
// direct port of niri-topic-namer's State machinery, minus the title-scraping
// (state now comes from the DB, not titles): the model only answers
// "which workspace is window W on?" and "what is the focused output?", and
// renames workspaces by index or name reference.
//
// Reference semantics (the crux of focus invariance, ported verbatim from the
// Python):
//   - A workspace's FIRST name must be set by INDEX reference
//     (`set-workspace-name --workspace <idx> <name>`), which niri resolves only
//     on the focused output. So the daemon bootstraps a workspace the first time
//     its output is focused.
//   - Once named, a workspace is renamed by NAME reference
//     (`set-workspace-name --workspace <oldName> <newName>`), which works on any
//     output — keeping dots live on non-focused monitors.
//   - Clearing is `unset-workspace-name <name>`, also a name reference.
//
// The model is NOT goroutine-safe; the daemon owns it from a single actor
// goroutine (matching the Python's single-threaded loop).

package niri

import (
	"fmt"
	"os/exec"
)

// Model is the live niri topology. It is mutated only by ApplyEvent (and the
// constructor) and queried by the reconciler. Not safe for concurrent use.
type Model struct {
	windows       map[int]Window    // window id -> window (incl. WorkspaceID)
	workspaces    map[int]Workspace // workspace id -> workspace
	focusedOutput string            // connector name of the focused output
}

// NewModel returns an empty model. The first WorkspacesChanged/WindowsChanged
// events (which niri emits immediately on subscribing) populate it.
func NewModel() *Model {
	return &Model{
		windows:    make(map[int]Window),
		workspaces: make(map[int]Workspace),
	}
}

// ApplyEvent folds one decoded event into the model. It returns true if the
// change could affect desired workspace names (so the daemon should mark itself
// dirty and re-reconcile), false for no-ops. Mirrors State.apply_event.
func (m *Model) ApplyEvent(ev Event) bool {
	switch ev.Kind {
	case KindWorkspacesChanged:
		next := make(map[int]Workspace, len(ev.Workspaces))
		for _, w := range ev.Workspaces {
			next[w.ID] = w
		}
		m.workspaces = next
		m.refreshFocusedOutput()
		return true

	case KindWindowsChanged:
		next := make(map[int]Window, len(ev.Windows))
		for _, w := range ev.Windows {
			next[w.ID] = w
		}
		m.windows = next
		return true

	case KindWindowOpenedOrChanged:
		m.windows[ev.Window.ID] = ev.Window
		return true

	case KindWindowClosed:
		if _, ok := m.windows[ev.WindowID]; !ok {
			return false
		}
		delete(m.windows, ev.WindowID)
		return true

	case KindWorkspaceActivated:
		// Focus moved (possibly to another output). Update the per-workspace
		// focus flags and recompute the focused output. Matches the Python: only
		// recompute when the activation actually carried focus.
		if ev.Focused {
			for id, w := range m.workspaces {
				w.IsFocused = w.ID == ev.WorkspaceID
				m.workspaces[id] = w
			}
			m.refreshFocusedOutput()
		}
		return true

	default:
		return false
	}
}

func (m *Model) refreshFocusedOutput() {
	for _, w := range m.workspaces {
		if w.IsFocused {
			m.focusedOutput = w.Output
			return
		}
	}
}

// FocusedOutput returns the connector name of the currently focused output, or
// "" if not yet known.
func (m *Model) FocusedOutput() string { return m.focusedOutput }

// WindowWorkspace returns the workspace id the given window currently sits on.
// ok is false if the window is unknown to the model (closed, or never seen).
func (m *Model) WindowWorkspace(windowID int) (workspaceID int, ok bool) {
	w, ok := m.windows[windowID]
	if !ok {
		return 0, false
	}
	return w.WorkspaceID, true
}

// HasWindow reports whether the model currently knows window windowID. The GC
// predicate uses this: a session whose cached window_id is absent from the live
// model is dead.
func (m *Model) HasWindow(windowID int) bool {
	_, ok := m.windows[windowID]
	return ok
}

// Workspaces returns the current workspace snapshot (id -> Workspace). The
// returned map is the model's own; callers must not mutate it. Used by the
// reconciler to iterate workspaces and to bootstrap by index/output.
func (m *Model) Workspaces() map[int]Workspace { return m.workspaces }

// Workspace returns the workspace with the given id and whether it exists.
func (m *Model) Workspace(id int) (Workspace, bool) {
	w, ok := m.workspaces[id]
	return w, ok
}

// SetName records the name the daemon assigned to a workspace locally, keeping
// the model's view consistent with the IPC call the reconciler just made
// (mirrors the Python writing ws["name"] = name after each action). This avoids
// a round-trip wait for the WorkspacesChanged echo.
func (m *Model) SetName(workspaceID int, name string) {
	if w, ok := m.workspaces[workspaceID]; ok {
		w.Name = name
		m.workspaces[workspaceID] = w
	}
}

// ---- IPC actuators -------------------------------------------------------
//
// These shell out to `niri msg action ...`, exactly like the Python's run().
// They are methods on Model only for cohesion; they hold no state and are
// swapped for fakes in tests via the Actuator interface below.

// Actuator is the set of niri rename operations the reconciler performs. The
// real implementation shells out to niri; tests inject a recording fake to keep
// IPC side effects out of unit tests.
type Actuator interface {
	// SetWorkspaceNameByIndex names a workspace by per-output index reference
	// (resolved only on the focused output) — the bootstrap path.
	SetWorkspaceNameByIndex(idx int, name string) error
	// SetWorkspaceNameByName renames an already-named workspace by name
	// reference (works on any output) — the steady-state path.
	SetWorkspaceNameByName(old, new string) error
	// UnsetWorkspaceName clears a workspace name by name reference.
	UnsetWorkspaceName(name string) error
}

// IPCActuator is the production Actuator: it runs `niri msg action`. The zero
// value is ready to use.
type IPCActuator struct{}

// SetWorkspaceNameByIndex runs `niri msg action set-workspace-name --workspace
// <idx> <name>`.
func (IPCActuator) SetWorkspaceNameByIndex(idx int, name string) error {
	return runAction("set-workspace-name", "--workspace", itoa(idx), name)
}

// SetWorkspaceNameByName runs `niri msg action set-workspace-name --workspace
// <old> <new>`.
func (IPCActuator) SetWorkspaceNameByName(old, new string) error {
	return runAction("set-workspace-name", "--workspace", old, new)
}

// UnsetWorkspaceName runs `niri msg action unset-workspace-name <name>`.
func (IPCActuator) UnsetWorkspaceName(name string) error {
	return runAction("unset-workspace-name", name)
}

func runAction(args ...string) error {
	cmd := exec.Command("niri", append([]string{"msg", "action"}, args...)...)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("niri msg action %v: %w", args, err)
	}
	return nil
}

func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}
