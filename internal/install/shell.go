package install

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// The zsh resume integration is installed as a marker-delimited block appended to
// the user's .zshrc. The markers are the idempotency key: install replaces the
// block if it's already there (in-place upgrade) or appends it once, and
// uninstall deletes exactly that range. Unlike the settings.json hooks, .zshrc is
// typically a git-tracked dotfile (here a chezmoi source) — we edit it anyway (a
// deliberate, user-requested exception to the "don't touch dotfiles" policy),
// keep a .bak, and remind the user to re-sync chezmoi.
const (
	zshBeginMarker = "# >>> claude-status resume >>>"
	zshEndMarker   = "# <<< claude-status resume <<<"
)

// zshBlock is the managed block, built around the two markers. The begin marker
// carries a "managed" note so a human reading .zshrc knows not to hand-edit it.
func zshBlock(bin string) string {
	return zshBeginMarker + "  (managed by `" + bin + " install`; do not edit — `" + bin + " uninstall` removes this)\n" +
		`eval "$(` + bin + ` resume --init zsh)"` + "\n" +
		zshEndMarker + "\n"
}

// applyZshBlock returns text with the managed block present exactly once. If the
// markers already exist it replaces the inclusive range (an in-place upgrade);
// otherwise it appends the block at EOF after a single blank-line separator. It
// reports whether an existing block was replaced (vs freshly appended). Pure —
// no IO — so it is directly unit-tested.
func applyZshBlock(text, bin string) (out string, replaced bool) {
	block := zshBlock(bin)
	if start, end, ok := findBlock(text); ok {
		return text[:start] + block + text[end:], true
	}
	// Append: ensure exactly one blank line separates prior content from the block.
	trimmed := strings.TrimRight(text, "\n")
	if trimmed == "" {
		return block, false
	}
	return trimmed + "\n\n" + block, false
}

// stripZshBlock returns text with the managed block removed, and whether it was
// present. It also swallows the blank-line separator applyZshBlock introduced
// before the block, so an install/uninstall round-trip restores the original
// (modulo a possible single trailing newline). Pure — unit-tested.
func stripZshBlock(text string) (out string, removed bool) {
	start, end, ok := findBlock(text)
	if !ok {
		return text, false
	}
	before := text[:start]
	after := text[end:]
	// Drop the blank-line separator we inserted before the block on append.
	before = strings.TrimRight(before, "\n")
	if before != "" {
		before += "\n"
	}
	after = strings.TrimLeft(after, "\n")
	return before + after, true
}

// findBlock locates the managed block's byte range [start, end) — from the start
// of the begin-marker line through the end of the end-marker line (including its
// trailing newline if present). ok is false when either marker is missing.
func findBlock(text string) (start, end int, ok bool) {
	b := strings.Index(text, zshBeginMarker)
	if b < 0 {
		return 0, 0, false
	}
	e := strings.Index(text[b:], zshEndMarker)
	if e < 0 {
		return 0, 0, false
	}
	end = b + e + len(zshEndMarker)
	if end < len(text) && text[end] == '\n' {
		end++ // consume the end-marker line's newline
	}
	return b, end, true
}

// ensureZshBlock installs (or upgrades) the managed block in the file at path,
// creating the file if absent. It backs up an existing file to path+".bak" first
// (via backupFile), then writes. It returns a short human status for the caller
// to print, and — when the file is a symlink (e.g. a chezmoi source) — the
// resolved real path so the caller can point the user at what actually changed.
func ensureZshBlock(path, bin string) (status, realPath string, err error) {
	text, existed, err := readTextFile(path)
	if err != nil {
		return "", "", err
	}
	out, replaced := applyZshBlock(text, bin)
	if out == text {
		return "already present (unchanged)", resolveReal(path), nil
	}
	if existed {
		if err := backupFile(path); err != nil {
			return "", "", err
		}
	}
	if err := writeTextFile(path, out); err != nil {
		return "", "", err
	}
	if replaced {
		return "updated existing block", resolveReal(path), nil
	}
	return "added block", resolveReal(path), nil
}

// removeZshBlock deletes the managed block from the file at path. A missing file
// or a file without the block is a no-op (removed=false). It backs up before
// writing, mirroring ensureZshBlock.
func removeZshBlock(path string) (removed bool, realPath string, err error) {
	text, existed, err := readTextFile(path)
	if err != nil {
		return false, "", err
	}
	if !existed {
		return false, path, nil
	}
	out, hadBlock := stripZshBlock(text)
	if !hadBlock {
		return false, resolveReal(path), nil
	}
	if err := backupFile(path); err != nil {
		return false, "", err
	}
	if err := writeTextFile(path, out); err != nil {
		return false, "", err
	}
	return true, resolveReal(path), nil
}

// readTextFile reads path as a string. A missing file yields ("", false, nil).
func readTextFile(path string) (text string, existed bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("reading %s: %w", path, err)
	}
	return string(data), true, nil
}

// writeTextFile writes text to path. When path is a symlink (a chezmoi-managed
// dotfile is one), a plain os.WriteFile follows it and rewrites the link target
// in place — which is what we want (the live file IS the source). We only need
// to preserve that behaviour, so we write through the path as given.
func writeTextFile(path, text string) error {
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// resolveReal returns the fully-resolved path (following symlinks) for reporting,
// falling back to the input if resolution fails.
func resolveReal(path string) string {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	return path
}

// defaultZshrc returns the target zshrc: $ZDOTDIR/.zshrc when ZDOTDIR is set,
// else ~/.zshrc.
func defaultZshrc() (string, error) {
	if z := os.Getenv("ZDOTDIR"); z != "" {
		return filepath.Join(z, ".zshrc"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home dir: %w", err)
	}
	return filepath.Join(home, ".zshrc"), nil
}

// printChezmoiReminder prints the post-edit follow-up: sync chezmoi (the live
// ~/.zshrc is a symlink to a chezmoi source here, so re-add keeps chezmoi's view
// consistent) and reload the shell to pick up `claude-resume` now.
func printChezmoiReminder(out io.Writer, realPath string) {
	fmt.Fprintf(out, "  edited %s\n", realPath)
	fmt.Fprintf(out, "  if this file is managed by chezmoi, run:  chezmoi re-add %q\n", "~/.zshrc")
	fmt.Fprintf(out, "  then reload your shell:  exec zsh   (or open a new terminal)\n")
}
