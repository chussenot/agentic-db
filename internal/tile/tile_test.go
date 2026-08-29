package tile

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
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

func TestWrapPNGAsSVG(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	src := filepath.Join(t.TempDir(), "app.png")
	img := image.NewRGBA(image.Rect(0, 0, 2, 3))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := wrapPNGAsSVG("com.example.App", src)
	if err != nil {
		t.Fatal(err)
	}
	svg, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	s := string(svg)
	if !strings.Contains(s, `width="2" height="3"`) || !strings.Contains(s, "data:image/png;base64,") {
		t.Errorf("shim missing dims or payload: %s", s)
	}
	// Second call reuses the cached shim (same path, no error).
	again, err := wrapPNGAsSVG("com.example.App", src)
	if err != nil || again != out {
		t.Errorf("cached shim not reused: %q %v", again, err)
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
	if s := p.Sessions[0]; s.State != "empty" || s.Folder != "" {
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

func TestNextWatchLine(t *testing.T) {
	key := Key("HDMI-A-1", 3)
	live := map[string]Payload{key: {Shortcut: 3, Sessions: []SessionTile{{State: "prompt", Folder: "repo"}}}}
	liveLine, _ := json.Marshal(live[key])
	placeholder, _ := json.Marshal(emptyPayload(3, false))
	readErr := errors.New("torn read")

	cases := []struct {
		name        string
		tiles       map[string]Payload
		rerr        error
		last        string
		wantLine    string
		wantPublish bool
	}{
		{"first successful read publishes", live, nil, "", string(liveLine), true},
		{"read error before any state publishes placeholder", nil, readErr, "", string(placeholder), true},
		{"read error after good state keeps last, no publish", nil, readErr, string(liveLine), string(liveLine), false},
		{"unchanged payload deduped", live, nil, string(liveLine), string(liveLine), false},
		{"missing key after good state publishes placeholder", map[string]Payload{}, nil, string(liveLine), string(placeholder), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line, publish := nextWatchLine(tc.tiles, tc.rerr, key, 3, tc.last)
			if line != tc.wantLine || publish != tc.wantPublish {
				t.Fatalf("nextWatchLine = (%q, %v), want (%q, %v)", line, publish, tc.wantLine, tc.wantPublish)
			}
		})
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

func TestParseDesktopEntry(t *testing.T) {
	data := []byte("[Desktop Entry]\nName=Proton Mail\nIcon=/snap/proton-mail/current/meta/gui/icon.svg\nStartupWMClass=Proton Mail Bridge\nExec=proton-mail\n\n[Desktop Action new-window]\nName=New Window\nIcon=wrong\nStartupWMClass=wrong\n")
	name, icon, wm := parseDesktopEntry(data)
	if name != "Proton Mail" || icon != "/snap/proton-mail/current/meta/gui/icon.svg" || wm != "Proton Mail Bridge" {
		t.Errorf("parseDesktopEntry = (%q, %q, %q), want (Proton Mail, .../icon.svg, Proton Mail Bridge)", name, icon, wm)
	}
}

// TestDesktopEntryIcon covers the WM-class bridge: niri reports an app_id
// ("vesktop") that names neither an icon nor a .desktop file, but a desktop
// entry declares StartupWMClass=vesktop with the real icon name. Filename
// match is preferred and case-insensitive WM-class match is the fallback.
func TestDesktopEntryIcon(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("dev.vencord.Vesktop.desktop",
		"[Desktop Entry]\nName=Vesktop\nIcon=dev.vencord.Vesktop\nStartupWMClass=vesktop\n")
	write("firefox.desktop",
		"[Desktop Entry]\nName=Firefox\nIcon=firefox-icon\n")

	prev := appEntryDirs
	appEntryDirs = []string{dir}
	t.Cleanup(func() { appEntryDirs = prev })

	if got := desktopEntryIcon("firefox"); got != "firefox-icon" {
		t.Errorf("filename match: desktopEntryIcon(firefox) = %q, want firefox-icon", got)
	}
	if got := desktopEntryIcon("VesKtop"); got != "dev.vencord.Vesktop" {
		t.Errorf("wm-class match: desktopEntryIcon(VesKtop) = %q, want dev.vencord.Vesktop", got)
	}
	if got := desktopEntryIcon("nothere"); got != "" {
		t.Errorf("no match: desktopEntryIcon(nothere) = %q, want empty", got)
	}
}

// TestIconFromName pins the absolute-path rule: an absolute Icon= is honored
// only when it is an existing .svg — pwetty's renderer can't draw a raw PNG
// path, so those fall through to the PNG-wrap stage instead.
func TestIconFromName(t *testing.T) {
	dir := t.TempDir()
	svg := filepath.Join(dir, "a.svg")
	if err := os.WriteFile(svg, []byte("<svg/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	png := filepath.Join(dir, "a.png")
	if err := os.WriteFile(png, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := iconFromName(svg); got != svg {
		t.Errorf("iconFromName(existing .svg) = %q, want %q", got, svg)
	}
	if got := iconFromName(png); got != "" {
		t.Errorf("iconFromName(absolute .png) = %q, want empty", got)
	}
	if got := iconFromName(filepath.Join(dir, "missing.svg")); got != "" {
		t.Errorf("iconFromName(missing .svg) = %q, want empty", got)
	}
}

// TestFindSnapIcon covers the Proton Mail case: niri hands us the app_id
// "Proton Mail" (a display name, not a reverse-DNS id), and the icon it
// resolves to lives under a snap's own meta/gui/ tree, outside every
// iconThemeBases() dir.
func TestFindSnapIcon(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(func() { snapDesktopDir = "/var/lib/snapd/desktop/applications" })
	snapDesktopDir = dir

	iconDir := filepath.Join(dir, "meta-gui")
	if err := os.MkdirAll(iconDir, 0o755); err != nil {
		t.Fatal(err)
	}
	iconPath := filepath.Join(iconDir, "icon.svg")
	if err := os.WriteFile(iconPath, []byte("<svg/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	desktop := "[Desktop Entry]\nName=Proton Mail\nIcon=" + iconPath + "\n"
	if err := os.WriteFile(filepath.Join(dir, "proton-mail_proton-mail.desktop"), []byte(desktop), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := findSnapIcon("Proton Mail"); got != iconPath {
		t.Errorf("findSnapIcon(%q) = %q, want %q", "Proton Mail", got, iconPath)
	}
	if got := findSnapIcon("proton mail"); got != iconPath {
		t.Errorf("findSnapIcon case-insensitive match failed: got %q", got)
	}
	if got := findSnapIcon("Some Other App"); got != "" {
		t.Errorf("findSnapIcon(%q) = %q, want empty", "Some Other App", got)
	}
}

// TestResolveAppIconSnapFallback exercises resolveAppIcon end to end: no
// hicolor/Papirus match, but a snap desktop entry resolves it.
func TestResolveAppIconSnapFallback(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // keep iconThemeBases() empty of real system icons
	dir := t.TempDir()
	t.Cleanup(func() { snapDesktopDir = "/var/lib/snapd/desktop/applications" })
	snapDesktopDir = dir

	iconPath := filepath.Join(dir, "icon.svg")
	if err := os.WriteFile(iconPath, []byte("<svg/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	desktop := "[Desktop Entry]\nName=Some Snap App\nIcon=" + iconPath + "\n"
	if err := os.WriteFile(filepath.Join(dir, "app.desktop"), []byte(desktop), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := resolveAppIcon("Some Snap App"); got != iconPath {
		t.Errorf("resolveAppIcon = %q, want %q", got, iconPath)
	}
}
