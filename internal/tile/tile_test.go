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
	sessions := []db.Session{claudeSession(24, "/home/x/claude-status-db", "working", 0)}

	p := PayloadFor(w, wins, sessions, now)
	if p.Shortcut != 5 || !p.Active || len(p.Sessions) != 1 {
		t.Fatalf("claude payload wrong: %+v", p)
	}
	s := p.Sessions[0]
	if s.State != "working" || s.Folder != "claude-status-db" || s.Title != "Diagnose bug" {
		t.Fatalf("claude session wrong: %+v", s)
	}
	if p.IsClaude != nil {
		t.Errorf("claude desktop should leave is_claude default (nil), got %v", *p.IsClaude)
	}
}

func TestPayloadForDualSharedWindow(t *testing.T) {
	// Two sessions in two kitty tabs of ONE niri window (window 7). Both must
	// surface as a 2-entry sessions[]; neither is masked. This is the desktop-3/5
	// multi-session case grouped by workspace, not window.
	now := time.UnixMilli(1_000_000)
	w := ws(3, "HDMI-A-1", 30, true)
	wins := []niri.Window{win(7, 30, "kitty", "ci-base")}
	sessions := []db.Session{
		claudeSession(7, "/a/ci-base-idle", "idle", now.UnixMilli()),
		claudeSession(7, "/a/ci-base-prompt", "prompt", 0),
	}
	p := PayloadFor(w, wins, sessions, now)
	if len(p.Sessions) != 2 {
		t.Fatalf("want 2 sessions, got %d: %+v", len(p.Sessions), p)
	}
	// prompt sorts first (statePriority), so it's never dropped and drives the pulse.
	if p.Sessions[0].State != "prompt" || p.Sessions[1].State != "idle" {
		t.Fatalf("dual order wrong (want prompt,idle): %+v", p.Sessions)
	}
	// Both share window 7's title.
	if p.Sessions[0].Title != "ci-base" || p.Sessions[1].Title != "ci-base" {
		t.Errorf("co-window sessions should share the window title: %+v", p.Sessions)
	}
}

func TestPayloadForCapsAtTwo(t *testing.T) {
	now := time.UnixMilli(1_000_000)
	w := ws(3, "HDMI-A-1", 30, false)
	wins := []niri.Window{win(7, 30, "kitty", "t")}
	sessions := []db.Session{
		claudeSession(7, "/a/one", "idle", now.UnixMilli()),
		claudeSession(7, "/a/two", "idle", now.UnixMilli()),
		claudeSession(7, "/a/three", "prompt", 0),
	}
	p := PayloadFor(w, wins, sessions, now)
	if len(p.Sessions) != 2 {
		t.Fatalf("want cap of 2, got %d", len(p.Sessions))
	}
	if p.Sessions[0].State != "prompt" {
		t.Errorf("prompt must survive the cap (kept first): %+v", p.Sessions)
	}
}

func TestPayloadForIdleDecay(t *testing.T) {
	now := time.UnixMilli(1_000_000)
	talk := now.Add(-12 * time.Minute).UnixMilli()
	w := ws(3, "HDMI-A-1", 30, false)
	wins := []niri.Window{win(7, 30, "kitty", "t")}
	sessions := []db.Session{claudeSession(7, "/a/ci-base", "idle", talk)}

	p := PayloadFor(w, wins, sessions, now)
	if len(p.Sessions) != 1 {
		t.Fatalf("want 1 session: %+v", p)
	}
	if s := p.Sessions[0]; s.State != "idle" || s.IdleAgo != "12m" || s.IdleLevel == 0 {
		t.Fatalf("idle decay wrong: %+v", s)
	}
}

func TestPayloadForAppDesktop(t *testing.T) {
	now := time.UnixMilli(1_000_000)
	w := ws(1, "HDMI-A-1", 2, false)
	wins := []niri.Window{win(15, 2, "org.keepassxc.KeePassXC", "Passbase")}
	p := PayloadFor(w, wins, nil, now) // no tracked session
	if p.IsClaude == nil || *p.IsClaude != false {
		t.Fatalf("app desktop must set is_claude=false: %+v", p)
	}
	if p.App != "KeePassXC" { // reverse-DNS app_id cleaned to its last component
		t.Errorf("app label = %q, want KeePassXC", p.App)
	}
	if p.AppIcon == "" { // resolved to an svg path or the bundled "app" fallback
		t.Errorf("app_icon should never be empty: %+v", p)
	}
}

func TestCleanAppLabel(t *testing.T) {
	cases := map[string]string{
		"firefox":                 "Firefox",   // all-lower -> capitalized
		"org.keepassxc.KeePassXC": "KeePassXC", // reverse-DNS -> last comp, intact
		"kitty":                   "Kitty",
		"":                        "",
	}
	for in, want := range cases {
		if got := cleanAppLabel(in); got != want {
			t.Errorf("cleanAppLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIconCandidatesCoversReverseDNS(t *testing.T) {
	c := iconCandidates("org.keepassxc.KeePassXC")
	has := func(s string) bool {
		for _, x := range c {
			if x == s {
				return true
			}
		}
		return false
	}
	if !has("keepassxc") { // the lowercased last component finds keepassxc.svg
		t.Errorf("candidates %v missing 'keepassxc'", c)
	}
}

func TestPayloadForEmptyDesktop(t *testing.T) {
	now := time.UnixMilli(1_000_000)
	p := PayloadFor(ws(8, "HDMI-A-1", 80, true), nil, nil, now)
	if len(p.Sessions) != 1 || !p.Active {
		t.Fatalf("empty desktop placeholder wrong: %+v", p)
	}
	if s := p.Sessions[0]; s.State != "idle" || s.IdleLevel == 0 || s.Folder != "" {
		t.Fatalf("empty placeholder session wrong: %+v", s)
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
	if got := all[Key("HDMI-A-1", 5)]; len(got.Sessions) != 1 || got.Sessions[0].Folder != "repo" {
		t.Errorf("HDMI-A-1:5 should be the claude tile: %+v", got)
	}
	if _, ok := all[Key("eDP-1", 1)]; !ok {
		t.Error("eDP-1:1 (empty) should still be present")
	}
}

func TestPayloadForDeterministicWindowPick(t *testing.T) {
	now := time.UnixMilli(1_000_000)
	w := ws(1, "HDMI-A-1", 2, false)
	// Same two windows, opposite input orders (mimics randomized map iteration).
	a := PayloadFor(w, []niri.Window{win(15, 2, "keepassxc", "KeePassXC"), win(9, 2, "firefox", "FF")}, nil, now)
	b := PayloadFor(w, []niri.Window{win(9, 2, "firefox", "FF"), win(15, 2, "keepassxc", "KeePassXC")}, nil, now)
	if a.App != b.App || a.Title != b.Title {
		t.Fatalf("window pick not deterministic: %q/%q vs %q/%q", a.App, a.Title, b.App, b.Title)
	}
	if a.App != "Firefox" { // lowest id (9) wins, label cleaned
		t.Errorf("expected lowest-id window (Firefox), got %q", a.App)
	}
}

func TestCacheRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tiles.json")
	in := map[string]Payload{
		Key("HDMI-A-1", 5): {Shortcut: 5, Sessions: []SessionTile{{State: "working", Folder: "repo"}}},
	}
	if err := WriteCache(path, in); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}
	out, err := ReadCache(path)
	if err != nil {
		t.Fatalf("ReadCache: %v", err)
	}
	got := out[Key("HDMI-A-1", 5)]
	if got.Shortcut != 5 || len(got.Sessions) != 1 || got.Sessions[0].State != "working" || got.Sessions[0].Folder != "repo" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}
