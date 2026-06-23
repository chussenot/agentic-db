package tile

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/mrzor/claude-status/internal/db"
	"github.com/mrzor/claude-status/internal/niri"
)

func ws(idx int, output string, id int, focused bool) niri.Workspace {
	return niri.Workspace{ID: id, Idx: idx, Output: output, IsFocused: focused}
}

func win(id, wsID int, app, title string) niri.Window {
	return niri.Window{ID: id, WorkspaceID: wsID, AppID: app, Title: title}
}

func claudeSession(winID int, cwd, state string, lastTalk int64) db.Session {
	s := db.Session{
		SessionID:  "s" + cwd,
		WindowID:   sql.NullInt64{Int64: int64(winID), Valid: true},
		Cwd:        sql.NullString{String: cwd, Valid: true},
		State:      state,
		LastTalkTS: sql.NullInt64{Int64: lastTalk, Valid: true},
	}
	return s
}

func TestPayloadForClaudeDesktop(t *testing.T) {
	now := time.UnixMilli(1_000_000)
	w := ws(5, "HDMI-A-1", 50, true)
	wins := []niri.Window{win(24, 50, "kitty", "Diagnose bug")}
	byWin := map[int]db.Session{24: claudeSession(24, "/home/x/claude-status-db", "working", 0)}

	p := PayloadFor(w, wins, byWin, now)
	if p.Shortcut != 5 || p.State != "working" || p.Folder != "claude-status-db" || p.Title != "Diagnose bug" || !p.Active {
		t.Fatalf("claude payload wrong: %+v", p)
	}
	if p.IsClaude != nil {
		t.Errorf("claude desktop should leave is_claude default (nil), got %v", *p.IsClaude)
	}
}

func TestPayloadForIdleDecay(t *testing.T) {
	now := time.UnixMilli(1_000_000)
	talk := now.Add(-12 * time.Minute).UnixMilli()
	w := ws(3, "HDMI-A-1", 30, false)
	wins := []niri.Window{win(7, 30, "kitty", "t")}
	byWin := map[int]db.Session{7: claudeSession(7, "/a/ci-base", "idle", talk)}

	p := PayloadFor(w, wins, byWin, now)
	if p.State != "idle" || p.IdleAgo != "12m" || p.IdleLevel == 0 {
		t.Fatalf("idle decay wrong: %+v", p)
	}
}

func TestPayloadForAppDesktop(t *testing.T) {
	now := time.UnixMilli(1_000_000)
	w := ws(1, "HDMI-A-1", 2, false)
	wins := []niri.Window{win(15, 2, "org.keepassxc.KeePassXC", "Passbase")}
	p := PayloadFor(w, wins, map[int]db.Session{}, now) // no tracked session
	if p.IsClaude == nil || *p.IsClaude != false {
		t.Fatalf("app desktop must set is_claude=false: %+v", p)
	}
	if p.App != "org.keepassxc.KeePassXC" || p.AppIcon != "org.keepassxc.KeePassXC" {
		t.Errorf("app fields wrong: %+v", p)
	}
}

func TestPayloadForEmptyDesktop(t *testing.T) {
	now := time.UnixMilli(1_000_000)
	p := PayloadFor(ws(8, "HDMI-A-1", 80, true), nil, map[int]db.Session{}, now)
	if p.State != "idle" || p.IdleLevel == 0 || p.Folder != "" || !p.Active {
		t.Fatalf("empty desktop placeholder wrong: %+v", p)
	}
}

func TestBuildAllKeysByOutputAndIdx(t *testing.T) {
	now := time.UnixMilli(1_000_000)
	workspaces := map[int]niri.Workspace{
		50: ws(5, "HDMI-A-1", 50, false),
		2:  ws(1, "eDP-1", 2, false), // same idx as nothing here; different output
	}
	windows := []niri.Window{win(24, 50, "kitty", "wt")}
	sessions := []db.Session{claudeSession(24, "/x/repo", "working", 0)}

	all := BuildAll(workspaces, windows, sessions, now)
	if _, ok := all[Key("HDMI-A-1", 5)]; !ok {
		t.Fatalf("missing HDMI-A-1:5 key; got %v", all)
	}
	if all[Key("HDMI-A-1", 5)].Folder != "repo" {
		t.Errorf("HDMI-A-1:5 should be the claude tile: %+v", all[Key("HDMI-A-1", 5)])
	}
	if _, ok := all[Key("eDP-1", 1)]; !ok {
		t.Error("eDP-1:1 (empty) should still be present")
	}
}

func TestPayloadForDeterministicWindowPick(t *testing.T) {
	now := time.UnixMilli(1_000_000)
	w := ws(1, "HDMI-A-1", 2, false)
	// Same two windows, opposite input orders (mimics randomized map iteration).
	a := PayloadFor(w, []niri.Window{win(15, 2, "keepassxc", "KeePassXC"), win(9, 2, "firefox", "FF")}, map[int]db.Session{}, now)
	b := PayloadFor(w, []niri.Window{win(9, 2, "firefox", "FF"), win(15, 2, "keepassxc", "KeePassXC")}, map[int]db.Session{}, now)
	if a.App != b.App || a.Title != b.Title {
		t.Fatalf("window pick not deterministic: %q/%q vs %q/%q", a.App, a.Title, b.App, b.Title)
	}
	if a.App != "firefox" { // lowest id (9) wins
		t.Errorf("expected lowest-id window (firefox), got %q", a.App)
	}
}

func TestCacheRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tiles.json")
	in := map[string]Payload{
		Key("HDMI-A-1", 5): {Shortcut: 5, State: "working", Folder: "repo"},
	}
	if err := WriteCache(path, in); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}
	out, err := ReadCache(path)
	if err != nil {
		t.Fatalf("ReadCache: %v", err)
	}
	got := out[Key("HDMI-A-1", 5)]
	if got.State != "working" || got.Folder != "repo" || got.Shortcut != 5 {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}
