package tile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseDesktop(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "app.desktop")
	content := "[Desktop Entry]\n" +
		"Name=Vesktop\n" +
		"Icon=dev.vencord.Vesktop\n" +
		"StartupWMClass=vesktop\n" +
		"[Desktop Action new]\n" +
		"Icon=should-be-ignored\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	icon, wm, name := parseDesktop(p)
	if icon != "dev.vencord.Vesktop" {
		t.Errorf("Icon = %q, want dev.vencord.Vesktop", icon)
	}
	if wm != "vesktop" {
		t.Errorf("StartupWMClass = %q, want vesktop", wm)
	}
	if name != "Vesktop" {
		t.Errorf("Name = %q, want Vesktop", name)
	}

	// Missing file -> empty, no panic.
	if i, w, n := parseDesktop(filepath.Join(dir, "nope.desktop")); i != "" || w != "" || n != "" {
		t.Errorf("missing file gave %q,%q,%q", i, w, n)
	}
}

// TestDesktopEntryIconByName covers the snap-packaged app shape: niri reports
// the display name ("Proton Mail") as the app_id, the entry is named
// <snap>_<app>.desktop with no StartupWMClass=, and Icon= is an absolute path
// under the snap's own meta/gui/ tree. HOME is redirected so the per-user
// applications dir stands in for /var/lib/snapd/desktop/applications.
func TestDesktopEntryIconByName(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	apps := filepath.Join(home, ".local/share/applications")
	if err := os.MkdirAll(apps, 0o755); err != nil {
		t.Fatal(err)
	}
	iconPath := filepath.Join(home, "meta-gui", "icon.svg")
	if err := os.MkdirAll(filepath.Dir(iconPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(iconPath, []byte("<svg/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	entry := "[Desktop Entry]\nName=Zz Snap Mail\nIcon=" + iconPath + "\nExec=zz-snap-mail\n"
	if err := os.WriteFile(filepath.Join(apps, "zz-snap-mail_zz-snap-mail.desktop"), []byte(entry), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := desktopEntryIcon("Zz Snap Mail"); got != iconPath {
		t.Errorf("desktopEntryIcon(display name) = %q, want %q", got, iconPath)
	}
	if got := desktopEntryIcon("zz snap mail"); got != iconPath {
		t.Errorf("desktopEntryIcon is not case-insensitive on Name=: got %q", got)
	}
	if got := desktopEntryIcon("Zz Other App"); got != "" {
		t.Errorf("desktopEntryIcon(no match) = %q, want empty", got)
	}
	// End to end: the absolute SVG path is honored directly, so the tile gets
	// the real icon instead of the generic "app" glyph.
	if got := resolveAppIcon("Zz Snap Mail"); got != iconPath {
		t.Errorf("resolveAppIcon = %q, want %q", got, iconPath)
	}
}

// TestDesktopEntryIconWMClassBeatsName pins the precedence: a StartupWMClass=
// match is the stronger signal and wins over an earlier Name= match.
func TestDesktopEntryIconWMClassBeatsName(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	apps := filepath.Join(home, ".local/share/applications")
	if err := os.MkdirAll(apps, 0o755); err != nil {
		t.Fatal(err)
	}
	byName := "[Desktop Entry]\nName=zzprec\nIcon=by-name\n"
	byClass := "[Desktop Entry]\nName=Other\nIcon=by-class\nStartupWMClass=zzprec\n"
	if err := os.WriteFile(filepath.Join(apps, "a_name.desktop"), []byte(byName), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(apps, "b_class.desktop"), []byte(byClass), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := desktopEntryIcon("zzprec"); got != "by-class" {
		t.Errorf("desktopEntryIcon = %q, want by-class (WM class over Name)", got)
	}
}

func TestCutKey(t *testing.T) {
	// Exact key only: "Icon" must not match "IconName=".
	if v, ok := cutKey("Icon=firefox", "Icon"); !ok || v != "firefox" {
		t.Errorf(`cutKey("Icon=firefox") = %q,%v`, v, ok)
	}
	if v, ok := cutKey("Icon = spaced ", "Icon"); !ok || v != "spaced" {
		t.Errorf(`cutKey spaced = %q,%v`, v, ok)
	}
	if _, ok := cutKey("IconName=x", "Icon"); ok {
		t.Error(`cutKey matched "IconName=" for key "Icon"`)
	}
	if _, ok := cutKey("Name=x", "Icon"); ok {
		t.Error("cutKey matched a different key")
	}
}

// TestDesktopEntryIconLive exercises the WM-class bridge against the real system
// if a vesktop-style entry (app_id != icon name) is installed. Skips otherwise.
func TestDesktopEntryIconLive(t *testing.T) {
	icon := desktopEntryIcon("vesktop")
	if icon == "" {
		t.Skip("no vesktop desktop entry on this box")
	}
	if icon == "vesktop" {
		t.Skipf("vesktop entry Icon= is itself %q; no bridge needed here", icon)
	}
	// The bridged icon name should resolve to a real scalable SVG.
	if got := iconFromName(icon); got == "" {
		t.Errorf("bridged icon %q did not resolve to an SVG", icon)
	}
}
