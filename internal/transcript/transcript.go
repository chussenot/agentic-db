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
	"time"
)

// Info is what a transcript yields about one session. Any field may be empty
// when the transcript lacks it (e.g. a session with no ai-title yet).
type Info struct {
	SessionID string
	Title     string // Claude's latest ai-title for the session (the topic)
	Ask       string // the opening user prompt (full, untruncated), i.e. the intent
	Cwd       string // working directory the session ran in
	Branch    string // git branch at the session's start
	// Turns is the session's main-thread conversational arc: every genuine user
	// prompt interleaved with the assistant's turn-ending message that preceded
	// the next prompt (the "recap before my prompt"), in chronological order.
	// Text is never truncated — the consumer decides what to elide. Sidechain
	// (subagent) messages are excluded; this is the top-level thread only.
	Turns []Turn
}

// Turn is one message in the arc: a genuine user prompt (Role "user") or an
// assistant turn-ending message (Role "assistant"). Text is the full message
// text. At is the transcript line's timestamp (zero if unparseable).
type Turn struct {
	Role string
	Text string
	At   time.Time
}

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

	sc := scanner{info: &Info{SessionID: sessionID}}
	r := bufio.NewReader(f)
	for {
		// Transcript lines can be far larger than bufio.Scanner's default token
		// cap (big assistant turns), so read by delimiter with no size limit.
		lineBytes, rerr := r.ReadBytes('\n')
		if len(lineBytes) > 0 {
			sc.line(lineBytes)
		}
		if rerr != nil {
			break // io.EOF or a read error: return what we have
		}
	}
	sc.done()
	return *sc.info, true
}

// tLine is the tolerant decode of one transcript line — only the fields recap
// needs. message.content is left raw because it is polymorphic (a plain string
// for a typed prompt, or an array of blocks for tool results / rich content).
type tLine struct {
	Type        string `json:"type"`
	AiTitle     string `json:"aiTitle"`
	Cwd         string `json:"cwd"`
	GitBranch   string `json:"gitBranch"`
	Timestamp   string `json:"timestamp"`
	IsSidechain bool   `json:"isSidechain"`
	Message     *struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// scanner accumulates Info across a transcript's lines. It carries the pending
// assistant summary — the last assistant text seen since the previous user
// prompt — which is flushed into the arc just before the next user prompt (so
// it reads as the "recap before my prompt") and, at EOF, as the session's final
// outcome.
type scanner struct {
	info     *Info
	pendText string    // last assistant text since the previous user turn
	pendAt   time.Time // its timestamp
}

func (sc *scanner) line(b []byte) {
	var l tLine
	if err := json.Unmarshal(b, &l); err != nil {
		return
	}
	if l.AiTitle != "" {
		sc.info.Title = l.AiTitle // last one wins
	}
	if sc.info.Cwd == "" && l.Cwd != "" {
		sc.info.Cwd = l.Cwd
	}
	if sc.info.Branch == "" && l.GitBranch != "" {
		sc.info.Branch = l.GitBranch
	}
	// The arc is the top-level thread only — subagent (sidechain) messages are
	// their own conversations, not what I typed to this session.
	if l.IsSidechain || l.Message == nil {
		return
	}
	switch l.Message.Role {
	case "assistant":
		if text := strings.TrimSpace(contentText(l.Message.Content)); text != "" {
			sc.pendText = text // last assistant text before the next prompt wins
			sc.pendAt = parseTS(l.Timestamp)
		}
	case "user":
		text := contentText(l.Message.Content)
		if !isRealAsk(text) {
			return // a tool_result-only turn, a slash-command wrapper, or a preamble
		}
		if sc.pendText != "" {
			sc.info.Turns = append(sc.info.Turns, Turn{Role: "assistant", Text: sc.pendText, At: sc.pendAt})
			sc.pendText = ""
		}
		sc.info.Turns = append(sc.info.Turns, Turn{Role: "user", Text: text, At: parseTS(l.Timestamp)})
		if sc.info.Ask == "" {
			sc.info.Ask = text // the opening ask, full and untruncated
		}
	}
}

// done flushes the trailing assistant summary — the last thing Claude said with
// no user prompt after it, i.e. the session's final outcome.
func (sc *scanner) done() {
	if sc.pendText != "" {
		sc.info.Turns = append(sc.info.Turns, Turn{Role: "assistant", Text: sc.pendText, At: sc.pendAt})
	}
}

// parseTS parses a transcript RFC3339 timestamp, returning the zero time when it
// is absent or malformed (never the caller's concern — At is best-effort).
func parseTS(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// contentText extracts the text of a message. content is either a JSON string
// (a typed prompt) or an array of blocks; for the array we join the text of any
// "text" blocks and ignore thinking / tool_use / tool_result / image blocks.
func contentText(content json.RawMessage) string {
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
		strings.HasPrefix(text, "<system-reminder"),
		strings.HasPrefix(text, "<task-notification"):
		// Harness-injected pseudo-prompts (slash-command wrappers, the caveat/
		// system-reminder preamble, background task-completion notifications) —
		// not something I typed, so they never enter the arc.
		return false
	}
	return true
}
