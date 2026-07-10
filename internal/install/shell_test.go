package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const bin = "claude-status"

func TestApplyZshBlockAppendsOnce(t *testing.T) {
	orig := "export FOO=1\nalias x=y\n"
	out, replaced := applyZshBlock(orig, bin)
	if replaced {
		t.Error("first apply should append, not replace")
	}
	if strings.Count(out, zshBeginMarker) != 1 || strings.Count(out, zshEndMarker) != 1 {
		t.Fatalf("want exactly one marker pair:\n%s", out)
	}
	if !strings.HasPrefix(out, orig) {
		t.Error("original content should be preserved as a prefix")
	}
	if !strings.Contains(out, `eval "$(claude-status resume --init zsh)"`) {
		t.Error("block should contain the eval line")
	}
	// Exactly one blank line separates prior content from the block.
	if !strings.Contains(out, "alias x=y\n\n"+zshBeginMarker) {
		t.Errorf("want a single blank-line separator before the block:\n%q", out)
	}
}

func TestApplyZshBlockIdempotentReplace(t *testing.T) {
	once, _ := applyZshBlock("setopt whatever\n", bin)
	twice, replaced := applyZshBlock(once, bin)
	if !replaced {
		t.Error("second apply should replace the existing block")
	}
	if once != twice {
		t.Errorf("re-apply not idempotent:\nA=%q\nB=%q", once, twice)
	}
	if strings.Count(twice, zshBeginMarker) != 1 {
		t.Errorf("duplicate block after re-apply:\n%s", twice)
	}
}

func TestApplyZshBlockUpgradesStaleBin(t *testing.T) {
	// A block written by an older bin name gets rewritten in place, not duplicated.
	old, _ := applyZshBlock("", "/old/path/claude-status")
	upgraded, replaced := applyZshBlock(old, bin)
	if !replaced || strings.Count(upgraded, zshBeginMarker) != 1 {
		t.Fatalf("upgrade should replace in place: replaced=%v\n%s", replaced, upgraded)
	}
	if strings.Contains(upgraded, "/old/path/") {
		t.Error("stale bin path should be gone after upgrade")
	}
}

func TestStripZshBlockRoundTrip(t *testing.T) {
	orig := "export FOO=1\nalias x=y\n"
	withBlock, _ := applyZshBlock(orig, bin)
	stripped, removed := stripZshBlock(withBlock)
	if !removed {
		t.Fatal("strip should report the block was present")
	}
	if stripped != orig {
		t.Errorf("round-trip mismatch:\nwant %q\ngot  %q", orig, stripped)
	}
}

func TestStripZshBlockAbsent(t *testing.T) {
	in := "just some config\n"
	out, removed := stripZshBlock(in)
	if removed || out != in {
		t.Errorf("strip of absent block should be a no-op: removed=%v out=%q", removed, out)
	}
}

func TestEnsureAndRemoveZshBlockFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".zshrc")
	if err := os.WriteFile(path, []byte("# my zshrc\nalias g=git\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Install.
	status, _, err := ensureZshBlock(path, bin)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if status != "added block" {
		t.Errorf("status = %q, want 'added block'", status)
	}
	if _, err := os.Stat(path + ".bak"); err != nil {
		t.Errorf("expected a .bak backup: %v", err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), zshBeginMarker) {
		t.Fatal("block not written to file")
	}

	// Re-install is a no-op.
	status2, _, err := ensureZshBlock(path, bin)
	if err != nil {
		t.Fatal(err)
	}
	if status2 != "already present (unchanged)" {
		t.Errorf("re-install status = %q, want unchanged", status2)
	}

	// Uninstall restores the original content.
	removed, _, err := removeZshBlock(path)
	if err != nil || !removed {
		t.Fatalf("remove: removed=%v err=%v", removed, err)
	}
	data, _ = os.ReadFile(path)
	if got := string(data); got != "# my zshrc\nalias g=git\n" {
		t.Errorf("after uninstall, file = %q, want the original", got)
	}

	// Removing again is a no-op.
	removed2, _, err := removeZshBlock(path)
	if err != nil || removed2 {
		t.Errorf("second remove should be a no-op: removed=%v err=%v", removed2, err)
	}
}

// TestEnsureZshBlockFollowsSymlink proves that writing through a symlinked .zshrc
// (the chezmoi layout) edits the link's target in place rather than replacing the
// symlink — so the live file stays the chezmoi source.
func TestEnsureZshBlockFollowsSymlink(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "dot_zshrc") // pretend chezmoi source
	link := filepath.Join(dir, ".zshrc")
	if err := os.WriteFile(source, []byte("# source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(source, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	if _, realPath, err := ensureZshBlock(link, bin); err != nil {
		t.Fatalf("ensure via symlink: %v", err)
	} else if realPath != source {
		// EvalSymlinks may prepend /private on macOS; compare bases as a fallback.
		if filepath.Base(realPath) != "dot_zshrc" {
			t.Errorf("realPath = %q, want the source %q", realPath, source)
		}
	}

	// The link is still a link, and its target now holds the block.
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("writing should not have replaced the symlink (mode=%v err=%v)", fi.Mode(), err)
	}
	data, _ := os.ReadFile(source)
	if !strings.Contains(string(data), zshBeginMarker) {
		t.Error("block should have been written into the symlink target")
	}
}
