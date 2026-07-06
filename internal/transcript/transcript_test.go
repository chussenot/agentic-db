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

func TestAskTruncation(t *testing.T) {
	dir := t.TempDir()
	id := "long"
	long := strings.Repeat("x", 400)
	writeTranscript(t, dir, "p", id, []string{
		`{"type":"user","message":{"role":"user","content":"` + long + `"}}`,
	})
	info, _ := Meta(dir, id)
	if len([]rune(info.Ask)) > askMax+1 { // +1 for the ellipsis rune
		t.Errorf("Ask not truncated: %d runes", len([]rune(info.Ask)))
	}
	if info.Ask[len(info.Ask)-len("…"):] != "…" {
		t.Errorf("truncated Ask should end with ellipsis, got %q", info.Ask[len(info.Ask)-4:])
	}
}
