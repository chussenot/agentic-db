package tile

// This file implements `claude-status tile-toggle <index>` — the CONSUMER side of
// the tile contract, and the command a pwetty desktop tile's on-click runs.
//
// A tile click used to be `niri msg action focus-workspace <idx>`, which does
// nothing when you are already on that desktop. tile-toggle spends that wasted
// click instead: on the desktop already, it moves focus to the tile's OTHER row.
// Desktop 2 shows Slack and Vesktop, so clicking 2 flips between them.
//
// The choice set is not a policy we invent here — it is exactly the rows the tile
// paints. `items` is capped at maxItemsPerTile (2), so "the next row, wrapping" is
// a two-way toggle and a third window on the desktop is simply not a click target;
// plain niri navigation reaches those. Reading the rows from the same cached
// Payload the tile renders is what guarantees the click and the pixels agree.

import (
	"flag"
	"io"
	"strconv"

	"github.com/mrzor/claude-status/internal/db"
	"github.com/mrzor/claude-status/internal/niri"
)

// RunToggle executes the tile-toggle subcommand. args is os.Args[2:]. Flags mirror
// Run's exactly, so a bar pins its output the same way its tile-watch does
// (`tile-toggle --output eDP-1 3`; the flag must precede the index).
//
// Whenever the second-click target can't be resolved — the daemon is down so there
// is no cache, an older daemon wrote rows without win_id, every row is windowless —
// it falls back to the plain desktop jump. That makes a click never worse than it
// was before this command existed.
func RunToggle(args []string) error {
	dbPath := db.DefaultDBPath()
	output := defaultOutput
	fs := flag.NewFlagSet("tile-toggle", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&dbPath, "db", dbPath, "path to the claude-status sqlite database")
	fs.StringVar(&output, "output", output, "niri output (connector) the tile's desktop lives on")
	_ = fs.Parse(args)

	idx, err := strconv.Atoi(fs.Arg(0))
	if err != nil {
		idx = 0
	}

	// One workspace snapshot answers both questions a click raises: is this a jump
	// or a flip, and — for a jump — which monitor we have to cross to get there.
	focusedOutput, onTarget := desktopFocus(output, idx)

	// Not on this desktop yet -> the click is an ordinary jump. The common path.
	if !onTarget {
		return jump(output, focusedOutput, idx)
	}

	// Already here: flip to the tile's other row. A cheap file read of the cache
	// the daemon refreshes on every window event, never a live DB+niri rebuild — a
	// click must stay snappy.
	tiles, err := ReadCache(CachePath(dbPath))
	if err != nil {
		return jump(output, focusedOutput, idx)
	}
	focused, err := niri.FocusedWindowID()
	if err != nil {
		return jump(output, focusedOutput, idx)
	}
	target, ok := nextWindow(tiles[Key(output, idx)].Items, int64(focused))
	if !ok {
		// Nothing to flip to (one focusable row, or none). Re-focusing the desktop
		// we are already on is a no-op, which is the right answer.
		return jump(output, focusedOutput, idx)
	}
	return niri.FocusWindow(int(target))
}

// desktopFocus returns the output that currently holds focus, and whether the
// focused desktop is the one this tile stands for. Exactly one workspace is
// focused, so "are we on the target" is just "is the focused one the target" —
// which also answers correctly for a desktop that doesn't exist yet. A niri error
// yields ("", false): treat the click as a jump, and don't try to cross outputs
// when we don't know where we are.
func desktopFocus(output string, idx int) (focusedOutput string, onTarget bool) {
	all, err := niri.ListWorkspaces()
	if err != nil {
		return "", false
	}
	for _, w := range all {
		if w.IsFocused {
			focusedOutput = w.Output
			onTarget = w.Output == output && w.Idx == idx
		}
	}
	return focusedOutput, onTarget
}

// jump is the plain desktop move a first click performs.
//
// niri resolves a bare workspace index against the FOCUSED output, and indexes
// repeat across outputs, so `focus-workspace 3` from the eDP-1 bar while HDMI-A-1
// holds focus would land on HDMI-A-1's desktop 3 — the wrong screen. Cross to the
// requested output first when it isn't the focused one.
//
// A failed crossing is not fatal: niri already exits 0 and no-ops for a disabled
// or absent output, and if some other niri ever errors instead, falling through to
// the bare index is exactly what this did before crossing existed. Swallowing the
// click would be worse than landing on the wrong output.
func jump(output, focusedOutput string, idx int) error {
	if crossesOutput(output, focusedOutput) {
		_ = niri.FocusMonitor(output)
	}
	return niri.FocusWorkspaceByIndex(idx)
}

// crossesOutput reports whether reaching output means leaving the focused one.
// An unknown focused output (niri unreachable) is never crossed from: without
// knowing where we are, a monitor switch is as likely to be wrong as right.
func crossesOutput(output, focusedOutput string) bool {
	return output != "" && focusedOutput != "" && output != focusedOutput
}

// nextWindow picks the window a click should focus, given the tile's rows and the
// currently focused window id. Returns ok=false when there is nowhere to move.
//
// Candidates are the rows' windows in row order, minus the rows that cannot be
// focused (WinID 0, a session we never pinned to a window) and minus duplicates:
// two co-resident sessions in one kitty are two rows but one window, so they must
// collapse or the "toggle" would re-focus what is already focused.
//
// From a candidate, we advance one and wrap — with two rows, a flip. From anywhere
// else we jump to the FIRST candidate: standing on one of the extra windows the
// tile cannot show, a click means "take me to what the tile is showing", and row
// order is rowPriority order, so that is the most salient window (a prompting
// Claude session if there is one).
func nextWindow(items []Item, focusedID int64) (int64, bool) {
	candidates := make([]int64, 0, len(items))
	for _, it := range items {
		if it.WinID == 0 {
			continue
		}
		dup := false
		for _, c := range candidates {
			if c == it.WinID {
				dup = true
				break
			}
		}
		if !dup {
			candidates = append(candidates, it.WinID)
		}
	}
	if len(candidates) == 0 {
		return 0, false
	}
	for i, c := range candidates {
		if c != focusedID {
			continue
		}
		if len(candidates) == 1 {
			return 0, false // the only candidate is already focused
		}
		return candidates[(i+1)%len(candidates)], true
	}
	return candidates[0], true
}
