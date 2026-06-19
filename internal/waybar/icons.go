package waybar

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/zor/claude-status/internal/state"
)

// writeIcons emits the complete niri/workspaces format-icons map as a
// pretty-printed (2-space) JSON object, generated for every slot 1..MaxSlots
// and decay level 0..DecayLevels-1, plus the preserved "default": "" key.
//
// It is laid out by hand (rather than json.MarshalIndent of a map, which would
// alphabetize keys) so the grouping mirrors the existing config.jsonc: per slot
// the working name, then the prompt name, then the seven idle decay levels.
func writeIcons(w io.Writer) error {
	// A leading comment line showing the surrounding context the user pastes into.
	if _, err := fmt.Fprintln(w, `// "niri/workspaces": { "format": "{index}{icon}", "format-icons": <this> }`); err != nil {
		return err
	}

	var b strings.Builder
	b.WriteString("{\n")
	for slot := 1; slot <= state.MaxSlots; slot++ {
		writeIconLine(&b, state.EncodeWorking(slot), state.WorkingIcon())
		writeIconLine(&b, state.EncodePrompt(slot), state.PromptIcon())
		for level := 0; level < state.DecayLevels; level++ {
			writeIconLine(&b, state.EncodeIdle(slot, level), state.IdleIcon(level))
		}
	}
	// Preserve the existing "default" key (no trailing comma — last entry).
	b.WriteString("  " + jsonString("default") + ": " + jsonString("") + "\n")
	b.WriteString("}\n")

	_, err := io.WriteString(w, b.String())
	return err
}

// writeIconLine appends one `  "key": "value",` entry (2-space indent, trailing
// comma since "default" always follows).
func writeIconLine(b *strings.Builder, key, value string) {
	b.WriteString("  " + jsonString(key) + ": " + jsonString(value) + ",\n")
}

// jsonString returns the JSON-encoded (quoted, escaped) form of s.
func jsonString(s string) string {
	out, _ := json.Marshal(s)
	return string(out)
}
