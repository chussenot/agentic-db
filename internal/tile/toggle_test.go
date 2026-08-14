package tile

import "testing"

// rows builds a tile's items from window ids alone — nextWindow only reads WinID,
// and the cap means a real payload never has more than two.
func rows(winIDs ...int64) []Item {
	items := make([]Item, 0, len(winIDs))
	for _, id := range winIDs {
		items = append(items, Item{Kind: KindApp, WinID: id})
	}
	return items
}

func TestNextWindow(t *testing.T) {
	tests := []struct {
		name    string
		items   []Item
		focused int64
		want    int64
		wantOK  bool
	}{{
		name:    "two rows, first focused -> second",
		items:   rows(8, 36), // desktop 2: Slack then Vesktop
		focused: 8,
		want:    36,
		wantOK:  true,
	}, {
		name:    "two rows, second focused -> wraps to first",
		items:   rows(8, 36),
		focused: 36,
		want:    8,
		wantOK:  true,
	}, {
		name:    "focused window is not a row -> the first row",
		items:   rows(5, 9), // the tile's two, on a desktop with more
		focused: 57,         // one of the extra kitties
		want:    5,
		wantOK:  true,
	}, {
		name:    "nothing focused at all -> the first row",
		items:   rows(5, 9),
		focused: 0,
		want:    5,
		wantOK:  true,
	}, {
		name:    "one row, already focused -> nowhere to go",
		items:   rows(24),
		focused: 24,
		wantOK:  false,
	}, {
		name:    "one row, something else focused -> that row",
		items:   rows(24),
		focused: 63,
		want:    24,
		wantOK:  true,
	}, {
		// Two co-resident sessions in one kitty: two rows, one window. Collapsing
		// them matters — otherwise the "toggle" re-focuses the focused window.
		name:    "two rows sharing one window, focused -> nowhere to go",
		items:   rows(7, 7),
		focused: 7,
		wantOK:  false,
	}, {
		name:    "two rows sharing one window, elsewhere -> that window",
		items:   rows(7, 7),
		focused: 63,
		want:    7,
		wantOK:  true,
	}, {
		// A windowless session row can't be focused, so it must not shadow the row
		// that can, nor become a target itself.
		name:    "windowless row is skipped",
		items:   rows(0, 36),
		focused: 63,
		want:    36,
		wantOK:  true,
	}, {
		name:    "windowless row is skipped, leaving one candidate already focused",
		items:   rows(0, 36),
		focused: 36,
		wantOK:  false,
	}, {
		name:    "all rows windowless -> nowhere to go",
		items:   rows(0, 0),
		focused: 63,
		wantOK:  false,
	}, {
		name:    "no rows -> nowhere to go",
		items:   nil,
		focused: 63,
		wantOK:  false,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := nextWindow(tc.items, tc.focused)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (got window %d)", ok, tc.wantOK, got)
			}
			if ok && got != tc.want {
				t.Errorf("window = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestNextWindowCyclesEveryRow(t *testing.T) {
	// The cap makes this a flip today, but the walk is a wrapping cycle rather than
	// a two-way swap, so a future maxItemsPerTile > 2 still reaches every row.
	items := rows(8, 36, 40)
	seen := map[int64]bool{}
	cur := int64(8)
	for range items {
		seen[cur] = true
		next, ok := nextWindow(items, cur)
		if !ok {
			t.Fatalf("cycle stalled at %d", cur)
		}
		cur = next
	}
	if cur != 8 {
		t.Errorf("cycle should return to its start, ended at %d", cur)
	}
	if len(seen) != len(items) {
		t.Errorf("visited %d of %d rows: %v", len(seen), len(items), seen)
	}
}
