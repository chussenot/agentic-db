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
	icon, wm := parseDesktop(p)
	if icon != "dev.vencord.Vesktop" {
		t.Errorf("Icon = %q, want dev.vencord.Vesktop", icon)
	}
	if wm != "vesktop" {
		t.Errorf("StartupWMClass = %q, want vesktop", wm)
	}

	// Missing file -> empty, no panic.
	if i, w := parseDesktop(filepath.Join(dir, "nope.desktop")); i != "" || w != "" {
		t.Errorf("missing file gave %q,%q", i, w)
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
