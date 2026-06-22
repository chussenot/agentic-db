package waybar

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mrzor/claude-status/internal/state"
)

// iconsOutput runs the generator in --icons mode and returns its stdout.
func iconsOutput(t *testing.T) string {
	t.Helper()
	var b bytes.Buffer
	if err := run(&b, true, false); err != nil {
		t.Fatalf("run(icons): %v", err)
	}
	return b.String()
}

// cssOutput runs the generator in --css mode and returns its stdout.
func cssOutput(t *testing.T) string {
	t.Helper()
	var b bytes.Buffer
	if err := run(&b, false, true); err != nil {
		t.Fatalf("run(css): %v", err)
	}
	return b.String()
}

// stripComments drops //-prefixed comment lines so the remainder is valid JSON.
func stripJSON(s string) string {
	var keep []string
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		keep = append(keep, line)
	}
	return strings.Join(keep, "\n")
}

func TestIconsValidJSON(t *testing.T) {
	out := iconsOutput(t)
	jsonPart := stripJSON(out)

	var m map[string]string
	if err := json.Unmarshal([]byte(jsonPart), &m); err != nil {
		t.Fatalf("icons output is not valid JSON: %v\n---\n%s", err, jsonPart)
	}

	// Expected keys present.
	wantKeys := []string{"cw20", "cp1", "cs1", "cs20", "ci1l0", "ci20l6", "default"}
	for _, k := range wantKeys {
		if _, ok := m[k]; !ok {
			t.Errorf("missing key %q in icons map", k)
		}
	}

	// Values equal the state.*Icon outputs.
	if got := m["cw20"]; got != state.WorkingIcon() {
		t.Errorf("cw20 = %q, want %q", got, state.WorkingIcon())
	}
	if got := m["cp1"]; got != state.PromptIcon() {
		t.Errorf("cp1 = %q, want %q", got, state.PromptIcon())
	}
	if got := m["cs1"]; got != state.ShellIcon() {
		t.Errorf("cs1 = %q, want %q", got, state.ShellIcon())
	}
	if got := m["ci1l0"]; got != state.IdleIcon(0) {
		t.Errorf("ci1l0 = %q, want %q", got, state.IdleIcon(0))
	}
	if got := m["ci20l6"]; got != state.IdleIcon(6) {
		t.Errorf("ci20l6 = %q, want %q", got, state.IdleIcon(6))
	}
	if got := m["default"]; got != "" {
		t.Errorf("default = %q, want empty", got)
	}

	// Completeness: every generated key for every slot/level is present.
	wantCount := state.MaxSlots*(3+state.DecayLevels) + 1 // working + prompt + shell + levels, + default
	if len(m) != wantCount {
		t.Errorf("icons map has %d keys, want %d", len(m), wantCount)
	}
}

func TestCSSContents(t *testing.T) {
	out := cssOutput(t)

	wantSubstrings := []string{
		"@keyframes claude-working-blink",
		"@keyframes claude-prompt-blink",
		"@keyframes claude-shell-pulse",
		"#niri-workspace-cw20",
		"#niri-workspace-cp20",
		"#niri-workspace-cs20",
		"#niri-workspace-ci1l0",
		"#niri-workspace-ci20l6",
		"animation: claude-working-blink 1.1s ease-in-out infinite;",
		"animation: claude-shell-pulse 1.6s ease-in-out infinite;",
		"@define-color claude-prompt " + state.PromptColor + ";",
		"@define-color claude-shell " + state.ShellColor + ";",
	}
	for _, s := range wantSubstrings {
		if !strings.Contains(out, s) {
			t.Errorf("css output missing %q", s)
		}
	}

	// Every idle level color from state must appear.
	for level := 0; level < state.DecayLevels; level++ {
		c := state.IdleColor(level)
		if !strings.Contains(out, "color: "+c+";") {
			t.Errorf("css output missing idle level %d color %q", level, c)
		}
	}

	// The orange fade target derives from WorkingColor (not hardcoded).
	if !strings.Contains(out, "rgba(217, 119, 87, 0.18)") {
		t.Errorf("css output missing orange fade rgba derived from %s", state.WorkingColor)
	}
}

func TestBothPrintsHeaders(t *testing.T) {
	var b bytes.Buffer
	if err := run(&b, false, false); err != nil {
		t.Fatalf("run(both): %v", err)
	}
	out := b.String()
	if !strings.Contains(out, "format-icons") {
		t.Errorf("both-mode missing format-icons header")
	}
	if !strings.Contains(out, "@keyframes claude-prompt-blink") {
		t.Errorf("both-mode missing css section")
	}
}
