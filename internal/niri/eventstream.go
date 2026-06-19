// This file (added by the daemon, alongside the frozen windows.go) implements
// the long-running niri IPC event-stream client. It subscribes to
// `niri msg -j event-stream`, which emits one JSON object per line, each an
// externally-tagged enum: a single-key object whose key names the event variant
// and whose value is that variant's payload. We decode only the variants the
// daemon's model cares about; everything else (ConfigLoaded, CastsChanged,
// KeyboardLayoutsChanged, OverviewOpenedOrClosed, WindowFocusChanged, ...) is
// surfaced as an Unknown event the caller can ignore.
//
// Verified shapes (captured live from `niri msg -j event-stream`):
//
//	{"WorkspacesChanged":{"workspaces":[{"id":32,"idx":5,"name":"ci2","output":"HDMI-A-1","is_focused":true,...}, ...]}}
//	{"WindowsChanged":{"windows":[{"id":205,"title":"…","app_id":"kitty","pid":90437,"workspace_id":32,...}, ...]}}
//	{"WindowOpenedOrChanged":{"window":{"id":230,"title":"CS_B2_TEST","app_id":"kitty","pid":539324,"workspace_id":32,...}}}
//	{"WindowClosed":{"id":230}}
//	{"WorkspaceActivated":{"id":32,"focused":true}}

package niri

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
)

// Workspace is one entry of the WorkspacesChanged payload. Only the fields the
// daemon model needs are decoded. niri emits more (is_active, is_urgent,
// active_window_id) which we ignore.
type Workspace struct {
	// ID is the stable workspace id (unique across outputs).
	ID int `json:"id"`
	// Idx is the per-output 1-based index (what waybar shows as the number). Two
	// workspaces on different outputs can share an idx; this is why the daemon's
	// slot allocator must be globally unique, not idx-based.
	Idx int `json:"idx"`
	// Name is the workspace name (the daemon's signal channel), or empty if
	// unnamed. niri sends JSON null for unnamed, decoded here as "".
	Name string `json:"name"`
	// Output is the connector the workspace lives on (e.g. "HDMI-A-1", "eDP-1").
	Output string `json:"output"`
	// IsFocused reports whether this workspace currently holds focus. Exactly one
	// workspace is focused at a time; its Output is the focused output.
	IsFocused bool `json:"is_focused"`
}

// EventKind names the event-stream variants the daemon handles. Everything else
// maps to KindUnknown.
type EventKind int

const (
	// KindUnknown is any event variant the daemon does not act on.
	KindUnknown EventKind = iota
	// KindWorkspacesChanged carries the full workspace list (Workspaces set).
	KindWorkspacesChanged
	// KindWindowsChanged carries the full window list (Windows set).
	KindWindowsChanged
	// KindWindowOpenedOrChanged carries a single updated window (Window set).
	KindWindowOpenedOrChanged
	// KindWindowClosed carries the id of a removed window (WindowID set).
	KindWindowClosed
	// KindWorkspaceActivated carries the activated workspace id (WorkspaceID set)
	// and whether it took focus (Focused).
	KindWorkspaceActivated
)

// Event is a decoded niri event-stream line. Which fields are populated depends
// on Kind. Unknown lines yield Event{Kind: KindUnknown} so the stream is a
// faithful, gap-free sequence the caller can range over.
type Event struct {
	Kind EventKind
	// Workspaces is set for KindWorkspacesChanged: the full workspace snapshot.
	Workspaces []Workspace
	// Windows is set for KindWindowsChanged: the full window snapshot.
	Windows []Window
	// Window is set for KindWindowOpenedOrChanged: the single affected window.
	Window Window
	// WindowID is set for KindWindowClosed: the id of the closed window.
	WindowID int
	// WorkspaceID is set for KindWorkspaceActivated: the activated workspace id.
	WorkspaceID int
	// Focused is set for KindWorkspaceActivated: whether the activation moved
	// focus (vs. merely activating a workspace on a non-focused output).
	Focused bool
}

// rawEvent is the externally-tagged enum on the wire: at most one of these keys
// is non-nil per line. json.RawMessage defers decoding the payload until we know
// the variant.
type rawEvent struct {
	WorkspacesChanged *struct {
		Workspaces []Workspace `json:"workspaces"`
	} `json:"WorkspacesChanged"`
	WindowsChanged *struct {
		Windows []Window `json:"windows"`
	} `json:"WindowsChanged"`
	WindowOpenedOrChanged *struct {
		Window Window `json:"window"`
	} `json:"WindowOpenedOrChanged"`
	WindowClosed *struct {
		ID int `json:"id"`
	} `json:"WindowClosed"`
	WorkspaceActivated *struct {
		ID      int  `json:"id"`
		Focused bool `json:"focused"`
	} `json:"WorkspaceActivated"`
}

// ParseEvent decodes a single event-stream line into an Event. A line that is
// valid JSON but not one of the handled variants returns Event{Kind:
// KindUnknown} with a nil error; only malformed JSON is an error. This is the
// pure, testable core of the stream (StreamEvents wraps it around a process).
func ParseEvent(line []byte) (Event, error) {
	var raw rawEvent
	if err := json.Unmarshal(line, &raw); err != nil {
		return Event{}, fmt.Errorf("decode niri event: %w", err)
	}
	switch {
	case raw.WorkspacesChanged != nil:
		return Event{Kind: KindWorkspacesChanged, Workspaces: raw.WorkspacesChanged.Workspaces}, nil
	case raw.WindowsChanged != nil:
		return Event{Kind: KindWindowsChanged, Windows: raw.WindowsChanged.Windows}, nil
	case raw.WindowOpenedOrChanged != nil:
		return Event{Kind: KindWindowOpenedOrChanged, Window: raw.WindowOpenedOrChanged.Window}, nil
	case raw.WindowClosed != nil:
		return Event{Kind: KindWindowClosed, WindowID: raw.WindowClosed.ID}, nil
	case raw.WorkspaceActivated != nil:
		return Event{
			Kind:        KindWorkspaceActivated,
			WorkspaceID: raw.WorkspaceActivated.ID,
			Focused:     raw.WorkspaceActivated.Focused,
		}, nil
	default:
		return Event{Kind: KindUnknown}, nil
	}
}

// StreamEvents starts `niri msg -j event-stream` and returns a channel of
// decoded events. The reader goroutine runs until the context is cancelled, the
// niri process exits, or stdout closes; on any of these it kills the process and
// closes the channel. Malformed lines are skipped (logged is the caller's job —
// we keep this dependency-free); KindUnknown events are forwarded so the
// consumer sees a continuous stream.
//
// The returned channel is unbuffered-ish (small buffer) and MUST be drained:
// the daemon's actor loop selects on it. Callers cancel ctx to stop.
func StreamEvents(ctx context.Context) (<-chan Event, error) {
	cmd := exec.CommandContext(ctx, "niri", "msg", "-j", "event-stream")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("event-stream stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start event-stream: %w", err)
	}

	out := make(chan Event, 64)
	go func() {
		defer close(out)
		// Best-effort reap of the child once we are done reading.
		defer func() { _ = cmd.Wait() }()

		sc := bufio.NewScanner(stdout)
		// niri lines (the full windows snapshot) can be large; bump the cap well
		// past bufio's 64KiB default.
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			ev, perr := ParseEvent(sc.Bytes())
			if perr != nil {
				continue // skip a malformed line, keep streaming
			}
			select {
			case out <- ev:
			case <-ctx.Done():
				return
			}
		}
		// Scanner stopped: stream closed (niri exited) or ctx cancelled killed it.
	}()
	return out, nil
}
