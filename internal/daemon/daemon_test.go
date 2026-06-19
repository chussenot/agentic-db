package daemon

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/zor/claude-status/internal/db"
	"github.com/zor/claude-status/internal/niri"
)

// fakeActuator records the niri rename calls the reconciler would make, instead
// of shelling out, so the bootstrap/name-reference/suppression logic can be
// asserted without a compositor.
type fakeActuator struct {
	calls []string
}

func (f *fakeActuator) SetWorkspaceNameByIndex(idx int, name string) error {
	f.calls = append(f.calls, fmt.Sprintf("idx %d -> %s", idx, name))
	return nil
}
func (f *fakeActuator) SetWorkspaceNameByName(old, new string) error {
	f.calls = append(f.calls, fmt.Sprintf("name %s -> %s", old, new))
	return nil
}
func (f *fakeActuator) UnsetWorkspaceName(name string) error {
	f.calls = append(f.calls, fmt.Sprintf("unset %s", name))
	return nil
}

// newTestDaemon wires a daemon around a real *niri.Model and a fake actuator.
func newTestDaemon(act niri.Actuator) *daemon {
	return &daemon{
		model:   niri.NewModel(),
		act:     act,
		slots:   newSlotAllocator(),
		managed: make(map[int]string),
	}
}

func workingSession(windowID int) db.Session {
	return db.Session{State: "working", WindowID: sql.NullInt64{Int64: int64(windowID), Valid: true}}
}

// TestReconcileFocusInvariantBootstrap verifies the crux: a workspace's first
// name uses an INDEX reference and only on the focused output; once named, it is
// updated by NAME reference regardless of which output is focused.
func TestReconcileFocusInvariantBootstrap(t *testing.T) {
	act := &fakeActuator{}
	d := newTestDaemon(act)

	// Two outputs; HDMI-A-1 is focused. ws 2 (HDMI) and ws 1 (eDP) each host a
	// working Claude window.
	d.model.ApplyEvent(niri.Event{
		Kind: niri.KindWorkspacesChanged,
		Workspaces: []niri.Workspace{
			{ID: 1, Idx: 1, Output: "eDP-1", IsFocused: false},
			{ID: 2, Idx: 5, Output: "HDMI-A-1", IsFocused: true},
		},
	})
	d.model.ApplyEvent(niri.Event{
		Kind: niri.KindWindowsChanged,
		Windows: []niri.Window{
			{ID: 100, WorkspaceID: 2}, // on focused output
			{ID: 200, WorkspaceID: 1}, // on non-focused output
		},
	})
	d.sessions = []db.Session{workingSession(100), workingSession(200)}

	d.reconcile()

	// Only the focused-output workspace (ws 2, idx 5) is bootstrapped, by index.
	if len(act.calls) != 1 {
		t.Fatalf("calls = %v, want exactly one index bootstrap", act.calls)
	}
	if act.calls[0] != "idx 5 -> cw1" {
		t.Fatalf("bootstrap call = %q, want %q", act.calls[0], "idx 5 -> cw1")
	}
	if d.managed[2] != "cw1" {
		t.Fatalf("managed[2] = %q, want cw1", d.managed[2])
	}
	if _, named := d.managed[1]; named {
		t.Fatal("ws 1 on non-focused output should NOT be named yet")
	}

	// Focus moves to eDP-1. Now ws 1 can bootstrap.
	act.calls = nil
	d.model.ApplyEvent(niri.Event{Kind: niri.KindWorkspaceActivated, WorkspaceID: 1, Focused: true})
	d.reconcile()
	if len(act.calls) != 1 || act.calls[0] != "idx 1 -> cw2" {
		t.Fatalf("after focus move, calls = %v, want [idx 1 -> cw2]", act.calls)
	}

	// Now make ws 2 (which is no longer the focused output) go idle. It must be
	// updated by NAME reference — proving focus invariance.
	act.calls = nil
	now := time.UnixMilli(0)
	d.sessions = []db.Session{
		{State: "idle", WindowID: sql.NullInt64{Int64: 100, Valid: true},
			LastTalkTS: sql.NullInt64{Int64: now.UnixMilli(), Valid: true}},
		workingSession(200),
	}
	db.Now = func() time.Time { return now }
	defer func() { db.Now = time.Now }()
	d.reconcile()

	foundNameRef := false
	for _, c := range act.calls {
		if c == "name cw1 -> ci1l0" {
			foundNameRef = true
		}
	}
	if !foundNameRef {
		t.Fatalf("expected a name-reference update cw1 -> ci1l0, calls = %v", act.calls)
	}
}

// TestReconcileRedundantSuppression confirms no IPC fires when desired == managed.
func TestReconcileRedundantSuppression(t *testing.T) {
	act := &fakeActuator{}
	d := newTestDaemon(act)
	d.model.ApplyEvent(niri.Event{
		Kind:       niri.KindWorkspacesChanged,
		Workspaces: []niri.Workspace{{ID: 2, Idx: 5, Output: "HDMI-A-1", IsFocused: true}},
	})
	d.model.ApplyEvent(niri.Event{Kind: niri.KindWindowsChanged, Windows: []niri.Window{{ID: 100, WorkspaceID: 2}}})
	d.sessions = []db.Session{workingSession(100)}

	d.reconcile()
	first := len(act.calls)
	if first != 1 {
		t.Fatalf("first reconcile calls = %d, want 1", first)
	}
	// Same desired state again -> no new IPC.
	d.reconcile()
	if len(act.calls) != first {
		t.Fatalf("redundant reconcile emitted IPC: %v", act.calls)
	}
}

// TestReconcileUnsetOnSessionGone confirms a workspace name is unset (by name
// reference) and its slot freed when its Claude session disappears.
func TestReconcileUnsetOnSessionGone(t *testing.T) {
	act := &fakeActuator{}
	d := newTestDaemon(act)
	d.model.ApplyEvent(niri.Event{
		Kind:       niri.KindWorkspacesChanged,
		Workspaces: []niri.Workspace{{ID: 2, Idx: 5, Output: "HDMI-A-1", IsFocused: true}},
	})
	d.model.ApplyEvent(niri.Event{Kind: niri.KindWindowsChanged, Windows: []niri.Window{{ID: 100, WorkspaceID: 2}}})
	d.sessions = []db.Session{workingSession(100)}
	d.reconcile()

	// Session gone.
	act.calls = nil
	d.sessions = nil
	d.reconcile()

	if len(act.calls) != 1 || act.calls[0] != "unset cw1" {
		t.Fatalf("calls = %v, want [unset cw1]", act.calls)
	}
	if _, ok := d.managed[2]; ok {
		t.Fatal("managed[2] should be cleared")
	}
	if _, ok := d.slots.slotOf(2); ok {
		t.Fatal("slot for ws 2 should be freed")
	}
}

// TestAdoptExistingNames verifies startup adoption reclaims slots from
// pre-existing names so they are neither reused nor renamed.
func TestAdoptExistingNames(t *testing.T) {
	act := &fakeActuator{}
	d := newTestDaemon(act)
	d.applyEvent(niri.Event{
		Kind: niri.KindWorkspacesChanged,
		Workspaces: []niri.Workspace{
			{ID: 2, Idx: 5, Output: "HDMI-A-1", IsFocused: true, Name: "ci3l2"},
			{ID: 1, Idx: 1, Output: "eDP-1", Name: "manual-name"}, // not ours
		},
	})
	if got := d.managed[2]; got != "ci3l2" {
		t.Fatalf("managed[2] = %q, want ci3l2 (adopted)", got)
	}
	if s, ok := d.slots.slotOf(2); !ok || s != 3 {
		t.Fatalf("slotOf(2) = (%d,%v), want (3,true)", s, ok)
	}
	if _, ok := d.managed[1]; ok {
		t.Fatal("non-matching manual name should not be adopted")
	}
	// A fresh assignment must skip the adopted slot 3.
	for i := 0; i < 5; i++ {
		if s, _ := d.slots.assign(500 + i); s == 3 {
			t.Fatal("assign handed out adopted slot 3")
		}
	}
}
