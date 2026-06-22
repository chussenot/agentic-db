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
		{clauded.Waiting, "", false}, // deferred to hooks (overloaded), not prompt
		{clauded.Idle, state.Idle, true},
		{clauded.Shell, state.Shell, true},
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
// the hook state (SOURCE=first-party), `waiting` defers to the hook
// (SOURCE=hook, since waiting is overloaded), and a session with no first-party
// file keeps the hook state.
func TestPrintFirstPartyShowsDriftAndSource(t *testing.T) {
	dir := t.TempDir()
	sessDir := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", dir)

	// first-party says idle; our hook row (below) still says prompt -> drift.
	// first-party idle clears the stuck "?" and wins as SOURCE=first-party.
	if err := os.WriteFile(filepath.Join(sessDir, "1.json"),
		[]byte(`{"pid":1,"sessionId":"drifter","status":"idle","statusUpdatedAt":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// first-party says waiting; it is overloaded, so it defers to the hook state
	// (SOURCE=hook), NOT mapped to prompt.
	if err := os.WriteFile(filepath.Join(sessDir, "2.json"),
		[]byte(`{"pid":2,"sessionId":"waiter","status":"waiting","statusUpdatedAt":1}`), 0o644); err != nil {
		t.Fatal(err)
	}

	sessions := []db.Session{
		{SessionID: "drifter", State: "prompt"}, // stale "?" hook state
		{SessionID: "waiter", State: "working"}, // mid-turn per hooks
		{SessionID: "remote", State: "prompt"},  // no first-party file
	}

	var buf bytes.Buffer
	printFirstParty(&buf, sessions)
	out := buf.String()

	if !strings.Contains(out, "2 session file(s)") {
		t.Errorf("expected two first-party files reported; got:\n%s", out)
	}
	// drifter: first-party idle wins over the stale prompt -> SOURCE=first-party.
	line := lineContaining(out, "drifter")
	if line == "" {
		t.Fatalf("no drifter row in:\n%s", out)
	}
	if !strings.Contains(line, "idle") || !strings.Contains(line, "first-party") {
		t.Errorf("drifter row should show idle + first-party source: %q", line)
	}
	// waiter: waiting defers to the hook -> EFFECTIVE working, SOURCE=hook.
	wline := lineContaining(out, "waiter")
	if wline == "" {
		t.Fatalf("no waiter row in:\n%s", out)
	}
	if !strings.Contains(wline, "waiting") || !strings.Contains(wline, "hook") {
		t.Errorf("waiter row should show waiting (first-party col) but SOURCE=hook: %q", wline)
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
