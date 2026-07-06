package recap

import (
	"strings"
	"testing"
	"time"
)

func TestMetricsMarkdown(t *testing.T) {
	var b strings.Builder
	MetricsMarkdown(&b, Digest{Totals: Totals{
		Sessions:  4,
		Active:    64 * time.Minute,
		MinActive: 2 * time.Minute,
		AvgActive: 16 * time.Minute,
		MaxActive: time.Hour,
		Prompts:   78,
		Questions: 51,
	}})
	out := b.String()
	for _, want := range []string{
		"## Metrics",
		"Total time clauding: 1h04m",
		"Per session: min 2m · avg 16m · max 1h00m (4 sessions)",
		"Prompts sent: 78",
		"Permission prompts: 51",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics output missing %q\n---\n%s", want, out)
		}
	}
}

func TestMetricsMarkdownZeroSessions(t *testing.T) {
	var b strings.Builder
	MetricsMarkdown(&b, Digest{}) // Sessions == 0 must not divide by zero
	if !strings.Contains(b.String(), "No activity") {
		t.Errorf("zero-session metrics should report no activity, got:\n%s", b.String())
	}
}
