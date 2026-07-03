package daemon

import (
	"database/sql"
	"testing"

	"github.com/mrzor/claude-status/internal/db"
	"github.com/mrzor/claude-status/internal/niri"
)

func sessWithWindow(id string, windowID int) db.Session {
	return db.Session{SessionID: id, WindowID: sql.NullInt64{Int64: int64(windowID), Valid: true}}
}

func TestTitleChangeEvents(t *testing.T) {
	windows := map[int]niri.Window{
		7: {ID: 7, Title: "build-the-thing"},
		9: {ID: 9, Title: "ci-base"},
	}

	t.Run("first observation emits one row per session and seeds lastTitle", func(t *testing.T) {
		last := map[string]string{}
		sessions := []db.Session{sessWithWindow("a", 7), sessWithWindow("b", 9)}
		rows := titleChangeEvents(windows, sessions, last, 1000)
		if len(rows) != 2 {
			t.Fatalf("want 2 rows, got %d: %+v", len(rows), rows)
		}
		for _, e := range rows {
			if e.Event != "TitleChanged" || e.NewState != "unchanged" || !e.WindowTitle.Valid {
				t.Errorf("malformed row: %+v", e)
			}
		}
		if last["a"] != "build-the-thing" || last["b"] != "ci-base" {
			t.Errorf("lastTitle not seeded: %+v", last)
		}
	})

	t.Run("unchanged title emits nothing", func(t *testing.T) {
		last := map[string]string{"a": "build-the-thing"}
		rows := titleChangeEvents(windows, []db.Session{sessWithWindow("a", 7)}, last, 1000)
		if len(rows) != 0 {
			t.Fatalf("unchanged title should emit no rows, got %+v", rows)
		}
	})

	t.Run("changed title emits exactly one row with the new title", func(t *testing.T) {
		last := map[string]string{"a": "old-title"}
		rows := titleChangeEvents(windows, []db.Session{sessWithWindow("a", 7)}, last, 1234)
		if len(rows) != 1 {
			t.Fatalf("want 1 row, got %d", len(rows))
		}
		if rows[0].WindowTitle.String != "build-the-thing" || rows[0].TS != 1234 {
			t.Errorf("row = %+v", rows[0])
		}
		if last["a"] != "build-the-thing" {
			t.Errorf("lastTitle not updated: %+v", last)
		}
	})

	t.Run("empty title is ignored (no row, no overwrite of last meaningful title)", func(t *testing.T) {
		wins := map[int]niri.Window{7: {ID: 7, Title: ""}}
		last := map[string]string{"a": "build-the-thing"}
		rows := titleChangeEvents(wins, []db.Session{sessWithWindow("a", 7)}, last, 1000)
		if len(rows) != 0 {
			t.Fatalf("empty title should emit nothing, got %+v", rows)
		}
		if last["a"] != "build-the-thing" {
			t.Errorf("empty title clobbered last meaningful title: %+v", last)
		}
	})

	t.Run("session without a resolved window is skipped", func(t *testing.T) {
		last := map[string]string{}
		rows := titleChangeEvents(windows, []db.Session{{SessionID: "remote"}}, last, 1000)
		if len(rows) != 0 || len(last) != 0 {
			t.Fatalf("windowless session should be ignored, rows=%+v last=%+v", rows, last)
		}
	})

	t.Run("co-window sessions are tracked independently", func(t *testing.T) {
		last := map[string]string{}
		sessions := []db.Session{sessWithWindow("a", 7), sessWithWindow("b", 7)}
		rows := titleChangeEvents(windows, sessions, last, 1000)
		if len(rows) != 2 {
			t.Fatalf("both co-window sessions should get a row, got %d", len(rows))
		}
	})

	t.Run("animated spinner prefix does not churn rows", func(t *testing.T) {
		last := map[string]string{}
		// Frame 1: spinner ⠐ + topic. Frame 2: spinner advances to ⠂, same topic.
		f1 := map[int]niri.Window{7: {ID: 7, Title: "⠐ Check window title logging"}}
		f2 := map[int]niri.Window{7: {ID: 7, Title: "⠂ Check window title logging"}}
		sessions := []db.Session{sessWithWindow("a", 7)}

		r1 := titleChangeEvents(f1, sessions, last, 1000)
		if len(r1) != 1 || r1[0].WindowTitle.String != "Check window title logging" {
			t.Fatalf("first frame should emit one normalized row, got %+v", r1)
		}
		r2 := titleChangeEvents(f2, sessions, last, 1001)
		if len(r2) != 0 {
			t.Fatalf("spinner advance must not emit a row, got %+v", r2)
		}
	})

	t.Run("reaped session is pruned from lastTitle", func(t *testing.T) {
		last := map[string]string{"a": "build-the-thing", "gone": "stale"}
		// Only "a" is live this pass; "gone" has been reaped.
		titleChangeEvents(windows, []db.Session{sessWithWindow("a", 7)}, last, 1000)
		if _, ok := last["gone"]; ok {
			t.Errorf("reaped session not pruned from lastTitle: %+v", last)
		}
		if last["a"] != "build-the-thing" {
			t.Errorf("live session dropped: %+v", last)
		}
	})
}

func TestNormalizeTitle(t *testing.T) {
	cases := map[string]string{
		"⠐ Check window title logging": "Check window title logging",
		"⠂ add-daemon-systray-mode":    "add-daemon-systray-mode",
		"✳ Reroute banner to stderr":   "Reroute banner to stderr",
		"plain-no-prefix":              "plain-no-prefix",
		"~/taf/ezmm":                   "~/taf/ezmm", // ASCII punctuation preserved
		"glab ci view":                 "glab ci view",
		"  spaced  ":                   "spaced",
		"✳   extra spaces after glyph": "extra spaces after glyph",
		"":                             "",
		"✳ ":                           "", // glyph-only collapses to empty
	}
	for in, want := range cases {
		if got := normalizeTitle(in); got != want {
			t.Errorf("normalizeTitle(%q) = %q, want %q", in, got, want)
		}
	}
}
