package recap

import (
	"strings"
	"testing"
)

func TestPromptNote(t *testing.T) {
	plain := Prompt("day", "")
	if !strings.Contains(plain, "DAILY standup") {
		t.Errorf("day prompt missing its template")
	}
	if strings.Contains(plain, "REVISION") {
		t.Errorf("no note should mean no revision directive")
	}

	note := "keep ci-base and datadog-ci as separate threads"
	rev := Prompt("day", note)
	if !strings.Contains(rev, "REVISION") || !strings.Contains(rev, note) {
		t.Errorf("note should append a revision directive carrying the feedback, got:\n%s", rev)
	}
	// Blank/whitespace notes are ignored.
	if strings.Contains(Prompt("week", "   "), "REVISION") {
		t.Errorf("whitespace-only note must not add a revision directive")
	}
	if !strings.Contains(Prompt("week", ""), "WEEKLY") {
		t.Errorf("week period should still select the weekly template")
	}
}
