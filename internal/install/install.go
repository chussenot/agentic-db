// Package install implements the `claude-status install` and `uninstall`
// subcommands — idempotently merging our hook entries into
// ~/.claude/settings.json (preserving existing hooks like bd prime), creating
// the state dir, and printing the niri/waybar setup fragments.
package install

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// hookMarker is the substring used to recognise our own hook command. Matching
// on the substring (rather than an exact command string) keeps re-runs
// idempotent regardless of the exact --bin path that was used previously.
const hookMarker = "claude-status hook"

// events lists the Claude Code hook events we register our command under, in a
// stable order so install output / file writes are deterministic.
var events = []string{
	"SessionStart",
	"UserPromptSubmit",
	"PostToolUse",
	"Notification",
	"Stop",
	"SubagentStop",
	"SessionEnd",
}

// Run executes the install subcommand. args is os.Args[2:].
func Run(args []string) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	settingsPath := fs.String("settings", "", "path to settings.json (default ~/.claude/settings.json)")
	bin := fs.String("bin", "claude-status", "command to register (the bin on PATH)")
	dryRun := fs.Bool("dry-run", false, "print what would change, write nothing")
	if err := fs.Parse(args); err != nil {
		return err
	}

	resolvedSettings, err := resolveSettingsPath(*settingsPath)
	if err != nil {
		return err
	}

	out := os.Stdout

	// 1. Merge hooks into settings.json.
	settings, existed, err := loadSettings(resolvedSettings)
	if err != nil {
		return err
	}

	command := *bin + " hook"
	added, alreadyPresent := mergeHooks(settings, command)

	if *dryRun {
		fmt.Fprintf(out, "[dry-run] would merge hooks into %s\n", resolvedSettings)
		if !existed {
			fmt.Fprintf(out, "[dry-run]   settings file does not exist; would create it\n")
		}
		printMergeSummary(out, command, added, alreadyPresent, true)
	} else {
		if err := writeSettings(resolvedSettings, settings, existed); err != nil {
			return err
		}
		fmt.Fprintf(out, "merged hooks into %s\n", resolvedSettings)
		if existed {
			fmt.Fprintf(out, "  backup written to %s.bak\n", resolvedSettings)
		}
		printMergeSummary(out, command, added, alreadyPresent, false)
	}

	// 2. State dir.
	stateDir := stateDir()
	if *dryRun {
		fmt.Fprintf(out, "[dry-run] would mkdir -p %s\n", stateDir)
	} else {
		if err := os.MkdirAll(stateDir, 0o755); err != nil {
			return fmt.Errorf("creating state dir %s: %w", stateDir, err)
		}
		fmt.Fprintf(out, "ensured state dir %s\n", stateDir)
	}

	// 3 & 4. Print manual-edit guidance and reload reminders.
	printSetupGuidance(out)

	return nil
}

// RunUninstall executes the uninstall subcommand. args is os.Args[2:] and may
// carry --purge to also drop the state dir + DB.
func RunUninstall(args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	settingsPath := fs.String("settings", "", "path to settings.json (default ~/.claude/settings.json)")
	purge := fs.Bool("purge", false, "also remove the state dir + DB")
	if err := fs.Parse(args); err != nil {
		return err
	}

	resolvedSettings, err := resolveSettingsPath(*settingsPath)
	if err != nil {
		return err
	}

	out := os.Stdout

	settings, existed, err := loadSettings(resolvedSettings)
	if err != nil {
		return err
	}

	if !existed {
		fmt.Fprintf(out, "settings file %s does not exist; nothing to remove\n", resolvedSettings)
	} else {
		removed := removeHooks(settings)
		if err := writeSettings(resolvedSettings, settings, existed); err != nil {
			return err
		}
		fmt.Fprintf(out, "removed our hook entries from %s\n", resolvedSettings)
		fmt.Fprintf(out, "  backup written to %s.bak\n", resolvedSettings)
		if len(removed) == 0 {
			fmt.Fprintf(out, "  (no %q entries were present)\n", hookMarker)
		} else {
			fmt.Fprintf(out, "  removed our hook from: %s\n", strings.Join(removed, ", "))
		}
		fmt.Fprintf(out, "  (bd prime and any other hooks left untouched)\n")
	}

	printUninstallGuidance(out)

	if *purge {
		dir := stateDir()
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("purging state dir %s: %w", dir, err)
		}
		fmt.Fprintf(out, "purged state dir %s (DB and logs removed)\n", dir)
	}

	return nil
}

// resolveSettingsPath returns the explicit path if given, otherwise the default
// ~/.claude/settings.json.
func resolveSettingsPath(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home dir: %w", err)
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

// stateDir returns $XDG_STATE_HOME/claude-status, falling back to
// ~/.local/state/claude-status.
func stateDir() string {
	if x := os.Getenv("XDG_STATE_HOME"); x != "" {
		return filepath.Join(x, "claude-status")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// Best-effort fallback; should not happen in practice.
		return filepath.Join(".local", "state", "claude-status")
	}
	return filepath.Join(home, ".local", "state", "claude-status")
}

// loadSettings reads the settings file as a generic JSON object. If the file
// does not exist, it returns an empty object and existed=false. A malformed
// JSON file is a hard error (so we never overwrite it).
func loadSettings(path string) (settings map[string]any, existed bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]any{}, false, nil
		}
		return nil, false, fmt.Errorf("reading %s: %w", path, err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		// Treat an empty file as an empty object rather than malformed.
		return map[string]any{}, true, nil
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil, true, fmt.Errorf("%s is not valid JSON (refusing to overwrite): %w", path, err)
	}
	if settings == nil {
		// Valid JSON but not an object (e.g. "null" or an array).
		return nil, true, fmt.Errorf("%s does not contain a JSON object (refusing to overwrite)", path)
	}
	return settings, true, nil
}

// writeSettings writes the merged settings back to path. If the file already
// existed, the original bytes are first copied to path+".bak".
func writeSettings(path string, settings map[string]any, existed bool) error {
	if existed {
		if err := backupFile(path); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling settings: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// backupFile copies path to path+".bak", preserving the original bytes exactly.
func backupFile(path string) error {
	src, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening %s for backup: %w", path, err)
	}
	defer src.Close()
	dst, err := os.Create(path + ".bak")
	if err != nil {
		return fmt.Errorf("creating backup %s.bak: %w", path, err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return fmt.Errorf("writing backup %s.bak: %w", path, err)
	}
	if err := dst.Close(); err != nil {
		return fmt.Errorf("closing backup %s.bak: %w", path, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// map[string]any hooks-tree navigation
//
// The Claude hooks structure unmarshals into nested generic JSON values:
//
//	settings["hooks"]                        -> map[string]any
//	settings["hooks"]["<Event>"]             -> []any
//	settings["hooks"]["<Event>"][i]          -> map[string]any  (a matcher group)
//	  group["matcher"]                       -> string
//	  group["hooks"]                         -> []any
//	    group["hooks"][j]                    -> map[string]any  (a single hook)
//	      hook["type"]                       -> "command"
//	      hook["command"]                    -> string
//
// The helpers below navigate/mutate this tree defensively: any element with an
// unexpected shape is left alone rather than coerced, so we never corrupt
// hand-written config.
// ---------------------------------------------------------------------------

// mergeHooks ensures our command is registered under every event in `events`.
// It returns the events to which we added a new entry, and the events where an
// entry was already present (idempotent no-op).
func mergeHooks(settings map[string]any, command string) (added, alreadyPresent []string) {
	hooks := childMap(settings, "hooks")

	for _, event := range events {
		arr := asSlice(hooks[event])
		group := findEmptyMatcherGroup(arr)
		if group == nil {
			group = map[string]any{"matcher": "", "hooks": []any{}}
			arr = append(arr, group)
		}
		groupHooks := asSlice(group["hooks"])
		if hasOurHook(groupHooks) {
			alreadyPresent = append(alreadyPresent, event)
		} else {
			groupHooks = append(groupHooks, map[string]any{
				"type":    "command",
				"command": command,
			})
			group["hooks"] = groupHooks
			added = append(added, event)
		}
		hooks[event] = arr
	}
	return added, alreadyPresent
}

// removeHooks strips only our hook entries from every event. Returns the list
// of events from which something was removed. It drops a matcher:"" group whose
// hooks array becomes empty, and removes an event key whose array becomes empty
// — but only when we are the reason it emptied (we never delete foreign hooks).
func removeHooks(settings map[string]any) (removed []string) {
	hooksRaw, ok := settings["hooks"].(map[string]any)
	if !ok {
		return nil
	}

	for event := range hooksRaw {
		arr, ok := hooksRaw[event].([]any)
		if !ok {
			continue
		}
		changed := false
		newArr := make([]any, 0, len(arr))
		for _, groupRaw := range arr {
			group, ok := groupRaw.(map[string]any)
			if !ok {
				newArr = append(newArr, groupRaw)
				continue
			}
			groupHooks := asSlice(group["hooks"])
			filtered := make([]any, 0, len(groupHooks))
			for _, hRaw := range groupHooks {
				if hookCommandContains(hRaw, hookMarker) {
					changed = true
					continue
				}
				filtered = append(filtered, hRaw)
			}
			// Drop a now-empty matcher:"" group, but only one we emptied.
			if len(filtered) == 0 && isEmptyMatcher(group) {
				// If the group had no hooks to begin with and we removed
				// nothing, leave it as-is (foreign empty group); otherwise drop.
				if len(groupHooks) > 0 {
					continue
				}
			}
			group["hooks"] = filtered
			newArr = append(newArr, group)
		}
		if changed {
			removed = append(removed, event)
		}
		if len(newArr) == 0 {
			delete(hooksRaw, event)
		} else {
			hooksRaw[event] = newArr
		}
	}

	if len(hooksRaw) == 0 {
		delete(settings, "hooks")
	}

	sort.Strings(removed)
	return removed
}

// childMap returns settings[key] as a map[string]any, creating and storing an
// empty one if the key is missing or not a map.
func childMap(m map[string]any, key string) map[string]any {
	if existing, ok := m[key].(map[string]any); ok {
		return existing
	}
	created := map[string]any{}
	m[key] = created
	return created
}

// asSlice coerces v to []any (nil v yields an empty, non-nil slice). A value of
// an unexpected type yields an empty slice (we then rebuild it cleanly).
func asSlice(v any) []any {
	if s, ok := v.([]any); ok {
		return s
	}
	return []any{}
}

// findEmptyMatcherGroup returns the first group in arr whose matcher is "",
// or nil if none exists.
func findEmptyMatcherGroup(arr []any) map[string]any {
	for _, g := range arr {
		group, ok := g.(map[string]any)
		if !ok {
			continue
		}
		if isEmptyMatcher(group) {
			return group
		}
	}
	return nil
}

// isEmptyMatcher reports whether group has matcher == "" (or no matcher key,
// which Claude treats as matching everything).
func isEmptyMatcher(group map[string]any) bool {
	switch m := group["matcher"].(type) {
	case string:
		return m == ""
	case nil:
		return true
	default:
		return false
	}
}

// hasOurHook reports whether any hook in the slice has a command containing the
// hookMarker substring.
func hasOurHook(hooks []any) bool {
	for _, h := range hooks {
		if hookCommandContains(h, hookMarker) {
			return true
		}
	}
	return false
}

// hookCommandContains reports whether h is a hook object whose "command" string
// contains sub.
func hookCommandContains(h any, sub string) bool {
	hm, ok := h.(map[string]any)
	if !ok {
		return false
	}
	cmd, ok := hm["command"].(string)
	if !ok {
		return false
	}
	return strings.Contains(cmd, sub)
}

// ---------------------------------------------------------------------------
// guidance printing
// ---------------------------------------------------------------------------

func printMergeSummary(out io.Writer, command string, added, alreadyPresent []string, dry bool) {
	prefix := ""
	if dry {
		prefix = "[dry-run] "
	}
	fmt.Fprintf(out, "%shook command: %s\n", prefix, command)
	if len(added) > 0 {
		fmt.Fprintf(out, "%s  added our hook to: %s\n", prefix, strings.Join(added, ", "))
	}
	if len(alreadyPresent) > 0 {
		fmt.Fprintf(out, "%s  already present (left as-is): %s\n", prefix, strings.Join(alreadyPresent, ", "))
	}
	if len(added) == 0 {
		fmt.Fprintf(out, "%s  nothing to do — all events already wired (idempotent)\n", prefix)
	}
}

func printSetupGuidance(out io.Writer) {
	fmt.Fprint(out, `
Next steps (manual edits — this tool does not touch your git-tracked dotfiles):

  niri config.kdl:
    Add (or replace the old niri-topic-namer spawn) with:
      spawn-sh-at-startup "exec ~/.local/bin/claude-status daemon"
    Then remove the old `+"`niri-topic-namer`"+` spawn-at-startup line.

  waybar:
    Run:
      claude-status gen-waybar
    Paste the emitted format-icons JSON into config.jsonc and the CSS into style.css.

  reload:
    killall -SIGUSR2 waybar     # reload CSS (style.css)
    restart waybar              # for config.jsonc changes
    restart niri / the daemon   # to cut over to the new daemon
`)
}

func printUninstallGuidance(out io.Writer) {
	fmt.Fprint(out, `
Revert your dotfiles manually (this tool does not touch git-tracked files):

  niri config.kdl:
    Remove the spawn-sh-at-startup "exec ~/.local/bin/claude-status daemon" line.

  waybar:
    Revert the claude-status format-icons block in config.jsonc and the
    generated CSS sections in style.css.

  reload:
    killall -SIGUSR2 waybar     # reload CSS
    restart waybar              # for config.jsonc changes
`)
}
