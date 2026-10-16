package tile

import (
	"database/sql"
	"path/filepath"
	"strconv"
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

	p := PayloadFor(w, wins, sessions, FleetView{}, now)
	if p.Shortcut != 5 || !p.Active || len(p.Items) != 1 {
		t.Fatalf("claude payload wrong: %+v", p)
	}
	s := p.Items[0]
	if s.Kind != KindSession || s.State != "working" || s.Folder != "claude-status-db" || s.Title != "Diagnose bug" {
		t.Fatalf("session row wrong: %+v", s)
	}
	if s.App != "" || s.AppIcon != "" {
		t.Errorf("session row must not carry app fields: %+v", s)
	}
}

func TestPayloadForDualSharedWindow(t *testing.T) {
	// Two sessions in two kitty tabs of ONE niri window (window 7). Both must
	// surface as 2 rows; neither is masked. This is the desktop-3/5 multi-session
	// case grouped by workspace, not window.
	now := time.UnixMilli(1_000_000)
	w := ws(3, "HDMI-A-1", 30, true)
	wins := []niri.Window{win(7, 30, "kitty", "ci-base")}
	sessions := []db.Session{
		claudeSession(7, "/a/ci-base-idle", "idle", now.UnixMilli()),
		claudeSession(7, "/a/ci-base-prompt", "prompt", 0),
	}
	p := PayloadFor(w, wins, sessions, FleetView{}, now)
	if len(p.Items) != 2 {
		t.Fatalf("want 2 rows, got %d: %+v", len(p.Items), p)
	}
	// prompt sorts first (rowPriority), so it's never dropped and drives the pulse.
	if p.Items[0].State != "prompt" || p.Items[1].State != "idle" {
		t.Fatalf("dual order wrong (want prompt,idle): %+v", p.Items)
	}
	// Both share window 7's title, and the window yields no extra app row.
	if p.Items[0].Title != "ci-base" || p.Items[1].Title != "ci-base" {
		t.Errorf("co-window sessions should share the window title: %+v", p.Items)
	}
	// …and its id, which is why tile-toggle sees ONE candidate here, not two.
	if p.Items[0].WinID != 7 || p.Items[1].WinID != 7 {
		t.Errorf("co-window sessions should share the window id: %+v", p.Items)
	}
}

func TestPayloadForMixedSessionAndApp(t *testing.T) {
	// One Claude session (window 7) plus an ordinary window (9) on the same
	// desktop: BOTH surface, session row first. The old sessions[]-only feed
	// dropped the plain window entirely.
	now := time.UnixMilli(1_000_000)
	w := ws(6, "HDMI-A-1", 60, false)
	wins := []niri.Window{win(9, 60, "firefox", "JSON Schema anyOf — Mozilla Firefox"), win(7, 60, "kitty", "unify rows")}
	sessions := []db.Session{claudeSession(7, "/a/pwetty-box-rs", "working", 0)}

	p := PayloadFor(w, wins, sessions, FleetView{}, now)
	if len(p.Items) != 2 {
		t.Fatalf("want 2 rows (session + app), got %d: %+v", len(p.Items), p)
	}
	if s := p.Items[0]; s.Kind != KindSession || s.Folder != "pwetty-box-rs" || s.Title != "unify rows" {
		t.Fatalf("row 0 should be the session: %+v", s)
	}
	a := p.Items[1]
	if a.Kind != KindApp || a.App != "Firefox" || a.Title != "JSON Schema anyOf — Mozilla Firefox" {
		t.Fatalf("row 1 should be the Firefox app row: %+v", a)
	}
	// Both kinds carry the niri window a click should focus.
	if p.Items[0].WinID != 7 || a.WinID != 9 {
		t.Errorf("rows should carry their window ids (want 7, 9): %+v", p.Items)
	}
	if a.AppIcon == "" { // an svg path or the bundled "app" fallback, never empty
		t.Errorf("app row needs an app_icon: %+v", a)
	}
	if a.State != "" || a.Folder != "" {
		t.Errorf("app row must not carry session fields: %+v", a)
	}
}

func TestPayloadForMixedSessionWinsTheCap(t *testing.T) {
	// A session and TWO ordinary windows: we must pick the 2 rows, and the Claude
	// session is the one that cannot be dropped.
	now := time.UnixMilli(1_000_000)
	w := ws(6, "HDMI-A-1", 60, false)
	wins := []niri.Window{
		win(3, 60, "firefox", "FF"),
		win(4, 60, "org.keepassxc.KeePassXC", "Passbase"),
		win(20, 60, "kitty", "idle work"),
	}
	sessions := []db.Session{claudeSession(20, "/a/repo", "idle", now.Add(-5*time.Minute).UnixMilli())}

	p := PayloadFor(w, wins, sessions, FleetView{}, now)
	if len(p.Items) != 2 {
		t.Fatalf("want cap of 2, got %d: %+v", len(p.Items), p)
	}
	// Idle session still outranks any app row; lowest-id app takes the other slot
	// (claude-status-db-mu8 will make that the ACTIVE window instead).
	if p.Items[0].Kind != KindSession || p.Items[0].Folder != "repo" {
		t.Fatalf("session row must survive the cap: %+v", p.Items)
	}
	if p.Items[1].Kind != KindApp || p.Items[1].App != "Firefox" {
		t.Fatalf("second row should be the lowest-id app window: %+v", p.Items)
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
	p := PayloadFor(w, wins, sessions, FleetView{}, now)
	if len(p.Items) != 2 {
		t.Fatalf("want cap of 2, got %d", len(p.Items))
	}
	if p.Items[0].State != "prompt" {
		t.Errorf("prompt must survive the cap (kept first): %+v", p.Items)
	}
}

func TestPayloadForIdleDecay(t *testing.T) {
	now := time.UnixMilli(1_000_000)
	talk := now.Add(-12 * time.Minute).UnixMilli()
	w := ws(3, "HDMI-A-1", 30, false)
	wins := []niri.Window{win(7, 30, "kitty", "t")}
	sessions := []db.Session{claudeSession(7, "/a/ci-base", "idle", talk)}

	p := PayloadFor(w, wins, sessions, FleetView{}, now)
	if len(p.Items) != 1 {
		t.Fatalf("want 1 row: %+v", p)
	}
	if s := p.Items[0]; s.State != "idle" || s.IdleAgo != "12m" || s.IdleLevel == 0 {
		t.Fatalf("idle decay wrong: %+v", s)
	}
}

// TestFmtAgoShapeDodgesRendererBugs pins the two renderer bugs the producer sides
// around: idle_ago must be <integer><suffix> (pwetty-box-rs-00i — anything else
// parses as a couple of minutes and renders as "just idled") and must stay ~3
// chars or it paints over the folder name (pwetty-box-rs-9l7 — "120m" collides).
func TestFmtAgoShapeDodgesRendererBugs(t *testing.T) {
	cases := map[time.Duration]string{
		-time.Minute:         "0m", // clock skew clamps, never a negative label
		30 * time.Second:     "0m",
		12 * time.Minute:     "12m",
		59 * time.Minute:     "59m",
		60 * time.Minute:     "1h", // NOT "60m"
		120 * time.Minute:    "2h", // the pwetty-box-rs-9l7 collision case
		23 * time.Hour:       "23h",
		24 * time.Hour:       "1d", // NOT "24h"
		3 * 24 * time.Hour:   "3d",
		500 * 24 * time.Hour: "99d", // clamped: 4 chars would overrun the slot
	}
	for d, want := range cases {
		got := fmtAgo(d)
		if got != want {
			t.Errorf("fmtAgo(%v) = %q, want %q", d, got, want)
		}
		if len(got) > 3 {
			t.Errorf("fmtAgo(%v) = %q is longer than the 3-char slot", d, got)
		}
		suffix := got[len(got)-1]
		if suffix != 'm' && suffix != 'h' && suffix != 'd' {
			t.Errorf("fmtAgo(%v) = %q lacks an m/h/d suffix", d, got)
		}
		if _, err := strconv.Atoi(got[:len(got)-1]); err != nil {
			t.Errorf("fmtAgo(%v) = %q is not <integer><suffix>", d, got)
		}
	}
}

func TestPayloadForAppDesktop(t *testing.T) {
	now := time.UnixMilli(1_000_000)
	w := ws(1, "HDMI-A-1", 2, false)
	wins := []niri.Window{win(15, 2, "org.keepassxc.KeePassXC", "Passbase")}
	p := PayloadFor(w, wins, nil, FleetView{}, now) // no tracked session
	if len(p.Items) != 1 {
		t.Fatalf("want 1 app row: %+v", p)
	}
	a := p.Items[0]
	if a.Kind != KindApp {
		t.Fatalf("windowless-of-sessions desktop must yield an app row: %+v", a)
	}
	if a.App != "KeePassXC" { // reverse-DNS app_id cleaned to its last component
		t.Errorf("app label = %q, want KeePassXC", a.App)
	}
	if a.AppIcon == "" { // resolved to an svg path or the bundled "app" fallback
		t.Errorf("app_icon should never be empty: %+v", a)
	}
	if a.Title != "Passbase" {
		t.Errorf("app row title = %q, want Passbase", a.Title)
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
	p := PayloadFor(ws(8, "HDMI-A-1", 80, true), nil, nil, FleetView{}, now)
	// A windowless desktop is a dimmed 1-row placeholder, never a 0-row items[]
	// (the contract's minItems is 1); the real fix is the `empty` preset
	// (claude-status-db-wsh).
	if len(p.Items) != 1 || !p.Active {
		t.Fatalf("empty desktop placeholder wrong: %+v", p)
	}
	if s := p.Items[0]; s.Kind != KindSession || s.State != "empty" || s.IdleLevel != 0 || s.Folder != "" {
		t.Fatalf("empty placeholder row wrong: %+v", s)
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

	all := BuildAll(workspaces, windows, sessions, FleetView{}, now)
	if _, ok := all[Key("HDMI-A-1", 5)]; !ok {
		t.Fatalf("missing HDMI-A-1:5 key; got %v", all)
	}
	if got := all[Key("HDMI-A-1", 5)]; len(got.Items) != 1 || got.Items[0].Folder != "repo" {
		t.Errorf("HDMI-A-1:5 should be a single session row: %+v", got)
	}
	if _, ok := all[Key("eDP-1", 1)]; !ok {
		t.Error("eDP-1:1 (empty) should still be present")
	}
}

func TestPayloadForDeterministicWindowPick(t *testing.T) {
	now := time.UnixMilli(1_000_000)
	w := ws(1, "HDMI-A-1", 2, false)
	// Same two windows, opposite input orders (mimics randomized map iteration).
	a := PayloadFor(w, []niri.Window{win(15, 2, "keepassxc", "KeePassXC"), win(9, 2, "firefox", "FF")}, nil, FleetView{}, now)
	b := PayloadFor(w, []niri.Window{win(9, 2, "firefox", "FF"), win(15, 2, "keepassxc", "KeePassXC")}, nil, FleetView{}, now)
	if len(a.Items) != 2 || len(b.Items) != 2 {
		t.Fatalf("two windows -> two app rows: %+v / %+v", a, b)
	}
	for i := range a.Items {
		if a.Items[i].App != b.Items[i].App || a.Items[i].Title != b.Items[i].Title {
			t.Fatalf("row order not deterministic: %+v vs %+v", a.Items, b.Items)
		}
	}
	if a.Items[0].App != "Firefox" { // lowest id (9) first, label cleaned
		t.Errorf("expected lowest-id window (Firefox) first, got %q", a.Items[0].App)
	}
}

func TestPayloadForWindowlessSessionHasNoWinID(t *testing.T) {
	// A session we never pinned to a niri window can't be focused, so its row must
	// leave WinID at 0 — that's what makes `win_id,omitempty` mean "not focusable"
	// rather than "window 0", and what keeps tile-toggle from focusing nothing.
	now := time.UnixMilli(1_000_000)
	s := claudeSession(0, "/a/detached", "working", 0)
	s.WindowID = sql.NullInt64{} // never learned
	p := PayloadFor(ws(4, "HDMI-A-1", 40, false), nil, []db.Session{s}, FleetView{}, now)
	if len(p.Items) != 1 {
		t.Fatalf("want the lone session row, got %+v", p.Items)
	}
	if p.Items[0].WinID != 0 {
		t.Errorf("windowless session must not claim a window: %+v", p.Items[0])
	}
}

func TestCacheRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tiles.json")
	in := map[string]Payload{
		Key("HDMI-A-1", 5): {Shortcut: 5, Items: []Item{
			{Kind: KindSession, State: "working", Folder: "repo"},
			{Kind: KindApp, App: "Firefox", AppIcon: "app", Title: "FF"},
		}},
	}
	if err := WriteCache(path, in); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}
	out, err := ReadCache(path)
	if err != nil {
		t.Fatalf("ReadCache: %v", err)
	}
	got := out[Key("HDMI-A-1", 5)]
	if got.Shortcut != 5 || len(got.Items) != 2 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if got.Items[0].Kind != KindSession || got.Items[0].State != "working" || got.Items[0].Folder != "repo" {
		t.Errorf("session row lost in round-trip: %+v", got.Items[0])
	}
	if got.Items[1].Kind != KindApp || got.Items[1].App != "Firefox" || got.Items[1].AppIcon != "app" {
		t.Errorf("app row lost in round-trip: %+v", got.Items[1])
	}
}
