package doctor

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrzor/claude-status/internal/clauded"
	"github.com/mrzor/claude-status/internal/db"
	"github.com/mrzor/claude-status/internal/state"
)

func TestEffectiveStateMapping(t *testing.T) {
	cases := []struct {
		in   clauded.Status
		want state.Status
		ok   bool
	}{
		{clauded.Busy, state.Working, true},
		{clauded.Waiting, state.Prompt, true},
		{clauded.Idle, state.Idle, true},
		{clauded.Status("compacting"), "", false},
	}
	for _, c := range cases {
		got, ok := effectiveState(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("effectiveState(%q) = (%q,%v), want (%q,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

// TestPrintFirstPartyShowsDriftAndSource points CLAUDE_CONFIG_DIR at a fixture
// dir and checks the side-by-side render: a fresh first-party status overrides
// the hook state (SOURCE=first-party), while a session with no first-party file
// keeps the hook state.
func TestPrintFirstPartyShowsDriftAndSource(t *testing.T) {
	dir := t.TempDir()
	sessDir := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", dir)

	// first-party says waiting; our hook row (below) still says working -> drift.
	if err := os.WriteFile(filepath.Join(sessDir, "1.json"),
		[]byte(`{"pid":1,"sessionId":"drifter","status":"waiting","statusUpdatedAt":1}`), 0o644); err != nil {
		t.Fatal(err)
	}

	sessions := []db.Session{
		{SessionID: "drifter", State: "working"}, // stale hook state
		{SessionID: "remote", State: "prompt"},   // no first-party file
	}

	var buf bytes.Buffer
	printFirstParty(&buf, sessions)
	out := buf.String()

	if !strings.Contains(out, "1 session file(s)") {
		t.Errorf("expected one first-party file reported; got:\n%s", out)
	}
	// The drifting session must resolve to prompt via first-party, not working.
	line := lineContaining(out, "drifter")
	if line == "" {
		t.Fatalf("no drifter row in:\n%s", out)
	}
	if !strings.Contains(line, "waiting") || !strings.Contains(line, "first-party") {
		t.Errorf("drifter row should show waiting + first-party source: %q", line)
	}
	// "remote" has no first-party file, so it isn't listed in this section.
	if strings.Contains(out, "remote") {
		t.Errorf("session without a first-party file should not appear: %q", out)
	}
}

func lineContaining(s, sub string) string {
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, sub) {
			return l
		}
	}
	return ""
}
