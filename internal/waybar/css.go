package waybar

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/mrzor/claude-status/internal/state"
)

// writeCSS emits the three generated style.css sections, all enumerated per
// slot/level (GTK CSS has no wildcard matching):
//
//  1. Working blink (orange) — reproduces the existing claude-working-blink
//     keyframe + rule, so this is a drop-in replacement for the hand-written one.
//  2. Prompt blink (yellow)  — a new claude-prompt-blink keyframe + rule.
//  3. Idle decay (static)    — one rule per decay level (all slots at that
//     level share the level color), DecayLevels rules instead of N*levels.
func writeCSS(w io.Writer) error {
	var b strings.Builder

	// @define-color block: reuse the existing claude-orange, add claude-prompt.
	b.WriteString("@define-color claude-orange " + state.WorkingColor + ";\n")
	b.WriteString("@define-color claude-prompt " + state.PromptColor + ";\n\n")

	// --- Section 1: working blink (orange) ---
	b.WriteString("/* Claude \"actively working\" workspaces (named cw<slot> by the daemon):\n")
	b.WriteString("   blink Claude-orange. Enumerated because GTK CSS matches widget ids exactly,\n")
	b.WriteString("   with no wildcard — keep in sync with the cw1..cwN range in config.jsonc. */\n")
	b.WriteString("@keyframes claude-working-blink {\n")
	b.WriteString("    from { color: @claude-orange; }\n")
	b.WriteString("    50%  { color: " + rgbaFade(state.WorkingColor, 0.18) + "; }\n")
	b.WriteString("    to   { color: @claude-orange; }\n")
	b.WriteString("}\n\n")
	b.WriteString(workspaceSelectors(slotNames(state.EncodeWorking)))
	b.WriteString(" {\n")
	b.WriteString("    color: @claude-orange;\n")
	b.WriteString("    animation: claude-working-blink 1.1s ease-in-out infinite;\n")
	b.WriteString("}\n\n")

	// --- Section 2: prompt blink (yellow) ---
	b.WriteString("/* Claude \"waiting for the user\" workspaces (named cp<slot>): blink yellow.\n")
	b.WriteString("   Permission request or idle nudge from the Notification hook. */\n")
	b.WriteString("@keyframes claude-prompt-blink {\n")
	b.WriteString("    from { color: @claude-prompt; }\n")
	b.WriteString("    50%  { color: " + rgbaFade(state.PromptColor, 0.18) + "; }\n")
	b.WriteString("    to   { color: @claude-prompt; }\n")
	b.WriteString("}\n\n")
	b.WriteString(workspaceSelectors(slotNames(state.EncodePrompt)))
	b.WriteString(" {\n")
	b.WriteString("    color: @claude-prompt;\n")
	b.WriteString("    animation: claude-prompt-blink 1.0s ease-in-out infinite;\n")
	b.WriteString("}\n\n")

	// --- Section 3: idle decay (static per level) ---
	b.WriteString("/* Claude idle workspaces (named ci<slot>l<level>): a static per-level color\n")
	b.WriteString("   encoding recency of the last talk (brightest just-stopped -> dim stale).\n")
	b.WriteString("   Grouped by level: all slots at a level share one color. */\n")
	for level := 0; level < state.DecayLevels; level++ {
		var names []string
		for slot := 1; slot <= state.MaxSlots; slot++ {
			names = append(names, state.EncodeIdle(slot, level))
		}
		b.WriteString("/* level " + fmt.Sprint(level) + " — " + decayRange(level) + " */\n")
		b.WriteString(workspaceSelectors(names))
		b.WriteString(" {\n")
		b.WriteString("    color: " + state.IdleColor(level) + ";\n")
		b.WriteString("}\n")
		if level < state.DecayLevels-1 {
			b.WriteString("\n")
		}
	}

	_, err := io.WriteString(w, b.String())
	return err
}

// slotNames builds the workspace names for slots 1..MaxSlots from an encoder
// that takes only a slot (working/prompt).
func slotNames(enc func(int) string) []string {
	names := make([]string, 0, state.MaxSlots)
	for slot := 1; slot <= state.MaxSlots; slot++ {
		names = append(names, enc(slot))
	}
	return names
}

// workspaceSelectors renders a comma-separated, newline-broken selector list
// (no trailing brace) for the given workspace names:
// `#workspaces button#niri-workspace-<name>` per line.
func workspaceSelectors(names []string) string {
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = "#workspaces button#niri-workspace-" + n
	}
	return strings.Join(parts, ",\n")
}

// rgbaFade converts a #rrggbb hex color to an rgba(r, g, b, alpha) string for
// the keyframe fade target. It avoids hardcoding the channel values so the
// fade tracks the state color constants.
func rgbaFade(hex string, alpha float64) string {
	r, g, b, ok := parseHex(hex)
	if !ok {
		// Fall back to the raw color at full alpha if it isn't #rrggbb.
		return hex
	}
	return fmt.Sprintf("rgba(%d, %d, %d, %s)", r, g, b, strconv.FormatFloat(alpha, 'f', -1, 64))
}

// parseHex parses a #rrggbb string into its three channels.
func parseHex(hex string) (r, g, b int, ok bool) {
	if len(hex) != 7 || hex[0] != '#' {
		return 0, 0, 0, false
	}
	var n int
	if _, err := fmt.Sscanf(hex[1:], "%06x", &n); err != nil {
		return 0, 0, 0, false
	}
	return (n >> 16) & 0xff, (n >> 8) & 0xff, n & 0xff, true
}

// decayRange returns the human-readable elapsed range for a decay level, derived
// from state.DecayThresholds so it stays in sync if the thresholds change.
func decayRange(level int) string {
	thresholds := state.DecayThresholds
	switch {
	case level == 0:
		return "0–" + dur(thresholds[0])
	case level >= len(thresholds):
		// Final open-ended bucket.
		return ">" + dur(thresholds[len(thresholds)-1])
	default:
		return dur(thresholds[level-1]) + "–" + dur(thresholds[level])
	}
}

// dur renders a duration compactly in minutes (the decay table is minute-grained).
func dur(d time.Duration) string {
	return fmt.Sprintf("%dm", int(d/time.Minute))
}
