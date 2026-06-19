package niri

import "testing"

// These JSON lines are captured verbatim from a live `niri msg -j event-stream`
// (workspace names trimmed for brevity, shape preserved).
func TestParseEvent(t *testing.T) {
	tests := []struct {
		name string
		line string
		want func(Event) bool
		kind EventKind
	}{
		{
			name: "WorkspacesChanged",
			line: `{"WorkspacesChanged":{"workspaces":[{"id":32,"idx":5,"name":"ci2","output":"HDMI-A-1","is_urgent":false,"is_active":true,"is_focused":true,"active_window_id":205},{"id":14,"idx":2,"name":null,"output":"HDMI-A-1","is_urgent":false,"is_active":false,"is_focused":false,"active_window_id":61}]}}`,
			kind: KindWorkspacesChanged,
			want: func(e Event) bool {
				if len(e.Workspaces) != 2 {
					return false
				}
				w0 := e.Workspaces[0]
				if w0.ID != 32 || w0.Idx != 5 || w0.Name != "ci2" || w0.Output != "HDMI-A-1" || !w0.IsFocused {
					return false
				}
				// null name decodes to "".
				return e.Workspaces[1].Name == "" && !e.Workspaces[1].IsFocused
			},
		},
		{
			name: "WindowsChanged",
			line: `{"WindowsChanged":{"windows":[{"id":205,"title":"✳ extract-logvolmon-service","app_id":"kitty","pid":90437,"workspace_id":32,"is_focused":true}]}}`,
			kind: KindWindowsChanged,
			want: func(e Event) bool {
				if len(e.Windows) != 1 {
					return false
				}
				w := e.Windows[0]
				return w.ID == 205 && w.PID == 90437 && w.AppID == "kitty" && w.WorkspaceID == 32
			},
		},
		{
			name: "WindowOpenedOrChanged",
			line: `{"WindowOpenedOrChanged":{"window":{"id":230,"title":"CS_B2_TEST","app_id":"kitty","pid":539324,"workspace_id":32,"is_focused":true}}}`,
			kind: KindWindowOpenedOrChanged,
			want: func(e Event) bool {
				return e.Window.ID == 230 && e.Window.PID == 539324 && e.Window.WorkspaceID == 32
			},
		},
		{
			name: "WindowClosed",
			line: `{"WindowClosed":{"id":230}}`,
			kind: KindWindowClosed,
			want: func(e Event) bool { return e.WindowID == 230 },
		},
		{
			name: "WorkspaceActivated focused",
			line: `{"WorkspaceActivated":{"id":32,"focused":true}}`,
			kind: KindWorkspaceActivated,
			want: func(e Event) bool { return e.WorkspaceID == 32 && e.Focused },
		},
		{
			name: "WorkspaceActivated not focused",
			line: `{"WorkspaceActivated":{"id":35,"focused":false}}`,
			kind: KindWorkspaceActivated,
			want: func(e Event) bool { return e.WorkspaceID == 35 && !e.Focused },
		},
		{
			name: "ignored variant",
			line: `{"WindowFocusChanged":{"id":216}}`,
			kind: KindUnknown,
			want: func(e Event) bool { return true },
		},
		{
			name: "ignored ConfigLoaded",
			line: `{"ConfigLoaded":{"failed":false}}`,
			kind: KindUnknown,
			want: func(e Event) bool { return true },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev, err := ParseEvent([]byte(tt.line))
			if err != nil {
				t.Fatalf("ParseEvent error: %v", err)
			}
			if ev.Kind != tt.kind {
				t.Fatalf("kind = %v, want %v", ev.Kind, tt.kind)
			}
			if !tt.want(ev) {
				t.Fatalf("event fields mismatch: %+v", ev)
			}
		})
	}
}

func TestParseEventMalformed(t *testing.T) {
	if _, err := ParseEvent([]byte(`{not json`)); err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

// TestModelApplyEvent exercises the model's reference semantics: window->ws
// mapping, focused-output tracking, and window add/remove.
func TestModelApplyEvent(t *testing.T) {
	m := NewModel()

	m.ApplyEvent(Event{
		Kind: KindWorkspacesChanged,
		Workspaces: []Workspace{
			{ID: 1, Idx: 1, Output: "eDP-1", IsFocused: false},
			{ID: 2, Idx: 2, Output: "HDMI-A-1", IsFocused: true, Name: "cw3"},
		},
	})
	if got := m.FocusedOutput(); got != "HDMI-A-1" {
		t.Fatalf("FocusedOutput = %q, want HDMI-A-1", got)
	}

	m.ApplyEvent(Event{
		Kind:    KindWindowsChanged,
		Windows: []Window{{ID: 100, WorkspaceID: 2}},
	})
	if ws, ok := m.WindowWorkspace(100); !ok || ws != 2 {
		t.Fatalf("WindowWorkspace(100) = (%d,%v), want (2,true)", ws, ok)
	}
	if !m.HasWindow(100) {
		t.Fatal("HasWindow(100) = false")
	}

	// A new window appears via WindowOpenedOrChanged.
	m.ApplyEvent(Event{Kind: KindWindowOpenedOrChanged, Window: Window{ID: 101, WorkspaceID: 1}})
	if ws, ok := m.WindowWorkspace(101); !ok || ws != 1 {
		t.Fatalf("WindowWorkspace(101) = (%d,%v), want (1,true)", ws, ok)
	}

	// Focus moves to the eDP-1 workspace.
	if !m.ApplyEvent(Event{Kind: KindWorkspaceActivated, WorkspaceID: 1, Focused: true}) {
		t.Fatal("WorkspaceActivated returned false")
	}
	if got := m.FocusedOutput(); got != "eDP-1" {
		t.Fatalf("FocusedOutput after activate = %q, want eDP-1", got)
	}

	// Closing a window removes it.
	if !m.ApplyEvent(Event{Kind: KindWindowClosed, WindowID: 100}) {
		t.Fatal("WindowClosed of known window returned false")
	}
	if m.HasWindow(100) {
		t.Fatal("HasWindow(100) still true after close")
	}
	// Closing an unknown window is a no-op (no relabel needed).
	if m.ApplyEvent(Event{Kind: KindWindowClosed, WindowID: 999}) {
		t.Fatal("WindowClosed of unknown window returned true")
	}
}
