package install

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureSettings mirrors the real ~/.claude/settings.json shape: a model, a
// SessionStart hook running `bd prime`, and enabledPlugins.
const fixtureSettings = `{
  "model": "opus",
  "hooks": {
    "SessionStart": [
      {
        "matcher": "",
        "hooks": [
          {
            "type": "command",
            "command": "bd prime --hook-json"
          }
        ]
      }
    ]
  },
  "enabledPlugins": {
    "gopls-lsp@claude-plugins-official": true
  },
  "effortLevel": "high"
}
`

// writeFixture writes the fixture settings into a fresh temp dir and returns the
// path. Always uses t.TempDir() — never the real settings file.
func writeFixture(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	return path
}

// readSettings parses a settings file into a map for assertions.
func readSettings(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading settings %s: %v", path, err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("settings %s not valid JSON: %v\n%s", path, err, data)
	}
	return m
}

// commandsForEvent returns every hook command string registered under the given
// event across all matcher groups.
func commandsForEvent(t *testing.T, settings map[string]any, event string) []string {
	t.Helper()
	hooks, ok := settings["hooks"].(map[string]any)
	if !ok {
		return nil
	}
	arr, ok := hooks[event].([]any)
	if !ok {
		return nil
	}
	var cmds []string
	for _, g := range arr {
		group, ok := g.(map[string]any)
		if !ok {
			continue
		}
		gh, ok := group["hooks"].([]any)
		if !ok {
			continue
		}
		for _, h := range gh {
			hm, ok := h.(map[string]any)
			if !ok {
				continue
			}
			if c, ok := hm["command"].(string); ok {
				cmds = append(cmds, c)
			}
		}
	}
	return cmds
}

func containsSub(ss []string, sub string) bool {
	for _, s := range ss {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func TestInstallMergesPreservingExisting(t *testing.T) {
	path := writeFixture(t, fixtureSettings)

	if err := Run([]string{"--settings", path, "--bin", "claude-status"}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	settings := readSettings(t, path)

	// Original top-level keys preserved.
	if settings["model"] != "opus" {
		t.Errorf("model not preserved: got %v", settings["model"])
	}
	if _, ok := settings["enabledPlugins"]; !ok {
		t.Errorf("enabledPlugins not preserved")
	}
	if settings["effortLevel"] != "high" {
		t.Errorf("effortLevel not preserved: got %v", settings["effortLevel"])
	}

	// SessionStart keeps bd prime AND gains our hook.
	ss := commandsForEvent(t, settings, "SessionStart")
	if !containsSub(ss, "bd prime") {
		t.Errorf("bd prime hook lost from SessionStart: %v", ss)
	}
	if !containsSub(ss, hookMarker) {
		t.Errorf("our hook not added to SessionStart: %v", ss)
	}

	// All seven events have our hook.
	for _, ev := range events {
		cmds := commandsForEvent(t, settings, ev)
		if !containsSub(cmds, hookMarker) {
			t.Errorf("event %s missing our hook: %v", ev, cmds)
		}
	}

	// A .bak exists.
	if _, err := os.Stat(path + ".bak"); err != nil {
		t.Errorf("expected backup file: %v", err)
	}
}

func TestInstallIdempotent(t *testing.T) {
	path := writeFixture(t, fixtureSettings)

	if err := Run([]string{"--settings", path, "--bin", "claude-status"}); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if err := Run([]string{"--settings", path, "--bin", "claude-status"}); err != nil {
		t.Fatalf("second Run: %v", err)
	}

	settings := readSettings(t, path)
	for _, ev := range events {
		cmds := commandsForEvent(t, settings, ev)
		count := 0
		for _, c := range cmds {
			if strings.Contains(c, hookMarker) {
				count++
			}
		}
		if count != 1 {
			t.Errorf("event %s has %d of our hooks, want exactly 1: %v", ev, count, cmds)
		}
	}

	// bd prime still present exactly once.
	ss := commandsForEvent(t, settings, "SessionStart")
	bd := 0
	for _, c := range ss {
		if strings.Contains(c, "bd prime") {
			bd++
		}
	}
	if bd != 1 {
		t.Errorf("bd prime present %d times, want 1: %v", bd, ss)
	}
}

func TestIdempotentAcrossDifferentBinPaths(t *testing.T) {
	path := writeFixture(t, fixtureSettings)

	if err := Run([]string{"--settings", path, "--bin", "/usr/bin/claude-status"}); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	// Different bin path; marker substring matching should still recognise ours.
	if err := Run([]string{"--settings", path, "--bin", "~/.local/bin/claude-status"}); err != nil {
		t.Fatalf("second Run: %v", err)
	}

	settings := readSettings(t, path)
	cmds := commandsForEvent(t, settings, "Stop")
	count := 0
	for _, c := range cmds {
		if strings.Contains(c, hookMarker) {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 of our hooks despite differing bin paths, got %d: %v", count, cmds)
	}
}

func TestUninstallRemovesOnlyOurs(t *testing.T) {
	path := writeFixture(t, fixtureSettings)

	if err := Run([]string{"--settings", path, "--bin", "claude-status"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := RunUninstall([]string{"--settings", path}); err != nil {
		t.Fatalf("RunUninstall: %v", err)
	}

	settings := readSettings(t, path)

	// Unrelated keys intact.
	if settings["model"] != "opus" {
		t.Errorf("model not preserved through uninstall: %v", settings["model"])
	}
	if _, ok := settings["enabledPlugins"]; !ok {
		t.Errorf("enabledPlugins lost through uninstall")
	}

	// bd prime survives.
	ss := commandsForEvent(t, settings, "SessionStart")
	if !containsSub(ss, "bd prime") {
		t.Errorf("bd prime lost on uninstall: %v", ss)
	}

	// No claude-status hook entries anywhere.
	for _, ev := range events {
		cmds := commandsForEvent(t, settings, ev)
		if containsSub(cmds, hookMarker) {
			t.Errorf("our hook survived uninstall under %s: %v", ev, cmds)
		}
	}

	// Events we created and that became empty should be gone; SessionStart
	// (still holding bd prime) must remain.
	hooks := settings["hooks"].(map[string]any)
	if _, ok := hooks["SessionStart"]; !ok {
		t.Errorf("SessionStart removed but it still holds bd prime")
	}
	for _, ev := range []string{"UserPromptSubmit", "PostToolUse", "Notification", "Stop", "SubagentStop", "SessionEnd"} {
		if _, ok := hooks[ev]; ok {
			t.Errorf("empty event %s not cleaned up after uninstall", ev)
		}
	}
}

func TestUninstallPreservesForeignSessionStartGroup(t *testing.T) {
	// SessionStart with only bd prime; after install+uninstall the bd prime
	// group must remain with its single hook.
	path := writeFixture(t, fixtureSettings)
	if err := Run([]string{"--settings", path, "--bin", "claude-status"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := RunUninstall([]string{"--settings", path}); err != nil {
		t.Fatalf("RunUninstall: %v", err)
	}
	settings := readSettings(t, path)
	ss := commandsForEvent(t, settings, "SessionStart")
	if len(ss) != 1 || !strings.Contains(ss[0], "bd prime") {
		t.Errorf("expected exactly bd prime under SessionStart, got %v", ss)
	}
}

func TestMalformedJSONAborts(t *testing.T) {
	malformed := `{ "model": "opus", this is not json `
	path := writeFixture(t, malformed)

	err := Run([]string{"--settings", path, "--bin", "claude-status"})
	if err == nil {
		t.Fatal("expected error on malformed JSON, got nil")
	}

	// Original file untouched.
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("reading back: %v", readErr)
	}
	if string(data) != malformed {
		t.Errorf("malformed file was modified:\n%s", data)
	}
	// No backup written.
	if _, statErr := os.Stat(path + ".bak"); statErr == nil {
		t.Errorf("backup written despite malformed input")
	}
}

func TestInstallMissingFileStartsFromEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	if err := Run([]string{"--settings", path, "--bin", "claude-status"}); err != nil {
		t.Fatalf("Run on missing file: %v", err)
	}

	settings := readSettings(t, path)
	for _, ev := range events {
		cmds := commandsForEvent(t, settings, ev)
		if !containsSub(cmds, hookMarker) {
			t.Errorf("event %s missing our hook in fresh settings: %v", ev, cmds)
		}
	}
	// No backup for a file that didn't exist.
	if _, err := os.Stat(path + ".bak"); err == nil {
		t.Errorf("backup written for nonexistent original")
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	path := writeFixture(t, fixtureSettings)

	if err := Run([]string{"--settings", path, "--bin", "claude-status", "--dry-run"}); err != nil {
		t.Fatalf("dry-run Run: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if string(data) != fixtureSettings {
		t.Errorf("dry-run modified the file:\n%s", data)
	}
	if _, err := os.Stat(path + ".bak"); err == nil {
		t.Errorf("dry-run wrote a backup")
	}
}

func TestStateDirHonorsXDG(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)
	got := stateDir()
	want := filepath.Join(tmp, "claude-status")
	if got != want {
		t.Errorf("stateDir() = %q, want %q", got, want)
	}
}

func TestPurgeRemovesStateDir(t *testing.T) {
	tmpState := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmpState)
	dir := stateDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "claude.sqlite"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write db: %v", err)
	}

	settingsPath := writeFixture(t, fixtureSettings)
	if err := RunUninstall([]string{"--settings", settingsPath, "--purge"}); err != nil {
		t.Fatalf("RunUninstall --purge: %v", err)
	}

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("state dir not purged: stat err = %v", err)
	}
}
