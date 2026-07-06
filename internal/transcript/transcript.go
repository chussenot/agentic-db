// Package transcript reads Claude Code session transcripts — the per-session
// JSONL files under ~/.claude/projects/<munged-cwd>/<session_id>.jsonl — to
// recover, for a given session id, what the session was about (Claude's own
// ai-title), what was first asked of it (the opening user prompt), and where it
// ran (cwd + git branch). The recap aggregator joins this onto the event log,
// which supplies the effort stats but none of the content.
//
// The session id is globally unique, so a session is located by globbing its
// filename across all project directories rather than needing the cwd up front.
// Decoding is tolerant: unknown line types and fields are ignored, so future
// Claude Code additions never break the reader.
package transcript

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Info is what a transcript yields about one session. Any field may be empty
// when the transcript lacks it (e.g. a session with no ai-title yet).
type Info struct {
	SessionID string
	Title     string // Claude's latest ai-title for the session (the topic)
	Ask       string // the opening user prompt (truncated), i.e. the intent
	Cwd       string // working directory the session ran in
	Branch    string // git branch at the session's start
}

// askMax bounds the stored opening prompt: enough to convey intent in a recap
// without piping a wall of text (or much of the conversation) to the model.
const askMax = 240

// DefaultDir returns Claude Code's transcripts root. It honors CLAUDE_CONFIG_DIR
// (as internal/clauded does for the sessions dir) so a relocated Claude config
// is found, and otherwise uses ~/.claude/projects, falling back to
// ".claude/projects" under the cwd if $HOME is unset.
func DefaultDir() string {
	if base := os.Getenv("CLAUDE_CONFIG_DIR"); base != "" {
		return filepath.Join(base, "projects")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".claude", "projects")
	}
	return filepath.Join(home, ".claude", "projects")
}

// find locates the transcript file for sessionID under dir, or "" if absent.
func find(dir, sessionID string) string {
	matches, err := filepath.Glob(filepath.Join(dir, "*", sessionID+".jsonl"))
	if err != nil || len(matches) == 0 {
		return ""
	}
	return matches[0]
}

// Meta reads the transcript for sessionID under dir and returns its Info. ok is
// false when no transcript exists. It scans the whole file so the LATEST
// ai-title wins (titles are refined as a session goes on); the opening prompt,
// cwd and branch are taken from the first qualifying lines. A read error after
// the file is found yields whatever was parsed so far with ok=true — best
// effort, never fatal to a recap.
func Meta(dir, sessionID string) (Info, bool) {
	path := find(dir, sessionID)
	if path == "" {
		return Info{}, false
	}
	f, err := os.Open(path)
	if err != nil {
		return Info{}, false
	}
	defer f.Close()

	info := Info{SessionID: sessionID}
	r := bufio.NewReader(f)
	for {
		// Transcript lines can be far larger than bufio.Scanner's default token
		// cap (big assistant turns), so read by delimiter with no size limit.
		lineBytes, rerr := r.ReadBytes('\n')
		if len(lineBytes) > 0 {
			scanLine(lineBytes, &info)
		}
		if rerr != nil {
			break // io.EOF or a read error: return what we have
		}
	}
	return info, true
}

// tLine is the tolerant decode of one transcript line — only the fields recap
// needs. message.content is left raw because it is polymorphic (a plain string
// for a typed prompt, or an array of blocks for tool results / rich content).
type tLine struct {
	Type      string `json:"type"`
	AiTitle   string `json:"aiTitle"`
	Cwd       string `json:"cwd"`
	GitBranch string `json:"gitBranch"`
	Message   *struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

func scanLine(b []byte, info *Info) {
	var l tLine
	if err := json.Unmarshal(b, &l); err != nil {
		return
	}
	if l.AiTitle != "" {
		info.Title = l.AiTitle // last one wins
	}
	if info.Cwd == "" && l.Cwd != "" {
		info.Cwd = l.Cwd
	}
	if info.Branch == "" && l.GitBranch != "" {
		info.Branch = l.GitBranch
	}
	if info.Ask == "" && l.Type == "user" && l.Message != nil {
		if text := userText(l.Message.Content); isRealAsk(text) {
			info.Ask = truncate(text, askMax)
		}
	}
}

// userText extracts the text of a user message. content is either a JSON string
// (a typed prompt) or an array of blocks; for the array we join the text of any
// "text" blocks and ignore tool_result / image / other blocks.
func userText(content json.RawMessage) string {
	if len(content) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(content, &s) == nil {
		return strings.TrimSpace(s)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
				parts = append(parts, strings.TrimSpace(b.Text))
			}
		}
		return strings.Join(parts, " ")
	}
	return ""
}

// isRealAsk filters out user "messages" that are not a human prompt: empty
// (tool-result-only turns), slash-command wrappers, and the injected caveat/
// system-reminder preamble Claude Code prepends. The first line that survives
// is the session's genuine opening ask.
func isRealAsk(text string) bool {
	if text == "" {
		return false
	}
	switch {
	case strings.HasPrefix(text, "<command-"),
		strings.HasPrefix(text, "<local-command"),
		strings.HasPrefix(text, "Caveat:"),
		strings.HasPrefix(text, "<system-reminder"):
		return false
	}
	return true
}

// truncate caps s at n runes, appending an ellipsis when it cuts.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n])) + "…"
}
