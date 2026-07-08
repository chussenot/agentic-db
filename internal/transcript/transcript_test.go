package transcript

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTranscript lays down dir/<proj>/<id>.jsonl with the given raw lines,
// mirroring the ~/.claude/projects/<proj>/<session>.jsonl layout.
func writeTranscript(t *testing.T, dir, proj, id string, lines []string) {
	t.Helper()
	pd := filepath.Join(dir, proj)
	if err := os.MkdirAll(pd, 0o755); err != nil {
		t.Fatal(err)
	}
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(filepath.Join(pd, id+".jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMeta(t *testing.T) {
	dir := t.TempDir()
	id := "1b2d6c42-f3c4-4ac6-b19d-8ded8cf84ab8"
	writeTranscript(t, dir, "-home-zor-proj", id, []string{
		`{"type":"last-prompt","sessionId":"x"}`,
		`{"type":"ai-title","aiTitle":"Old working title"}`,
		// a slash-command wrapper — must be skipped as the "ask"
		`{"type":"user","message":{"role":"user","content":"<command-name>/foo</command-name>"}}`,
		// a tool-result-only user turn — must be skipped
		`{"type":"user","cwd":"/home/zor/proj","gitBranch":"master","message":{"role":"user","content":[{"type":"tool_result","content":"ok"}]}}`,
		// the genuine opening ask (string content) — this one wins
		`{"type":"user","cwd":"/home/zor/proj","gitBranch":"master","message":{"role":"user","content":"do we record window titles?"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"yes"}]}}`,
		// a later, refined title — last ai-title wins
		`{"type":"ai-title","aiTitle":"Record window titles in the event log"}`,
	})

	info, ok := Meta(dir, id)
	if !ok {
		t.Fatal("expected ok=true for existing transcript")
	}
	if info.Title != "Record window titles in the event log" {
		t.Errorf("Title = %q, want the latest ai-title", info.Title)
	}
	if info.Ask != "do we record window titles?" {
		t.Errorf("Ask = %q, want the first genuine prompt (command + tool_result skipped)", info.Ask)
	}
	if info.Cwd != "/home/zor/proj" || info.Branch != "master" {
		t.Errorf("cwd/branch = %q/%q", info.Cwd, info.Branch)
	}
}

func TestMetaMissing(t *testing.T) {
	if _, ok := Meta(t.TempDir(), "does-not-exist"); ok {
		t.Error("expected ok=false when no transcript file exists")
	}
}

// The opening ask is stored full — no truncation at the tooling level (the
// consumer decides what to elide).
func TestAskNotTruncated(t *testing.T) {
	dir := t.TempDir()
	id := "long"
	long := strings.Repeat("x", 400)
	writeTranscript(t, dir, "p", id, []string{
		`{"type":"user","message":{"role":"user","content":"` + long + `"}}`,
	})
	info, _ := Meta(dir, id)
	if info.Ask != long {
		t.Errorf("Ask should be the full %d-rune prompt untruncated, got %d runes", len(long), len([]rune(info.Ask)))
	}
}

// The arc interleaves my prompts with the assistant's turn-ending message
// before each next prompt: only the LAST assistant text of a run survives (the
// turn-ending one), sidechain (subagent) messages are excluded, and the final
// assistant message is flushed as the session outcome.
func TestArc(t *testing.T) {
	dir := t.TempDir()
	id := "arc"
	writeTranscript(t, dir, "p", id, []string{
		`{"type":"user","timestamp":"2026-07-07T10:00:00Z","message":{"role":"user","content":"first ask"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"thinking out loud"}]}}`,
		`{"type":"assistant","timestamp":"2026-07-07T10:01:00Z","message":{"role":"assistant","content":[{"type":"text","text":"done the first thing"}]}}`,
		`{"type":"user","isSidechain":true,"message":{"role":"user","content":"subagent prompt"}}`,
		`{"type":"user","timestamp":"2026-07-07T10:05:00Z","message":{"role":"user","content":"second ask"}}`,
		`{"type":"assistant","timestamp":"2026-07-07T10:06:00Z","message":{"role":"assistant","content":[{"type":"text","text":"final outcome"}]}}`,
	})
	info, ok := Meta(dir, id)
	if !ok {
		t.Fatal("expected ok=true")
	}
	want := []Turn{
		{Role: "user", Text: "first ask"},
		{Role: "assistant", Text: "done the first thing"}, // intermediate "thinking out loud" dropped
		{Role: "user", Text: "second ask"},                // sidechain prompt excluded
		{Role: "assistant", Text: "final outcome"},        // flushed at EOF
	}
	if len(info.Turns) != len(want) {
		t.Fatalf("Turns = %d, want %d: %+v", len(info.Turns), len(want), info.Turns)
	}
	for i, w := range want {
		if info.Turns[i].Role != w.Role || info.Turns[i].Text != w.Text {
			t.Errorf("Turns[%d] = {%s %q}, want {%s %q}", i, info.Turns[i].Role, info.Turns[i].Text, w.Role, w.Text)
		}
	}
	if info.Turns[2].At.IsZero() {
		t.Error("expected the second ask's timestamp to be parsed")
	}
}
