// Package tile implements `claude-status tile-data <index>` — the producer that
// bridges claude-status-db + niri to the pwetty `claude` waybar tile.
//
// It emits, on stdout, the JSON data object the bundled pwetty `claude` tile
// expects (contract: ~/Perso/pwetty-box-rs/tiles/claude/schema.json) for ONE
// niri desktop, identified by its per-output workspace index. A waybar
// `cffi/pwetty#i` module configured `exec: claude-status tile-data i` then
// renders a rich per-desktop tile, replacing the old niri/workspaces glyph.
//
// Like the hook, this is on a user-facing path (waybar reruns it every
// interval): it ALWAYS prints valid JSON and returns nil, degrading to a
// minimal payload rather than failing the bar.
package tile

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/mrzor/claude-status/internal/db"
	"github.com/mrzor/claude-status/internal/niri"
	"github.com/mrzor/claude-status/internal/state"
)

// defaultOutput is the niri connector the second bar lives on. The producer is
// scoped to one output so a bare desktop index uniquely identifies a workspace
// (indexes repeat across outputs). Overridable with --output. Multi-output
// support is tracked separately (see the pwetty epic).
const defaultOutput = "HDMI-A-1"

// payload is the pwetty `claude` tile data object. Fields use omitempty so an
// absent value falls back to the tile's schema default. See schema.json for the
// REAL (from claude-status-db) vs MOCK (synthesized) provenance of each field.
type payload struct {
	Shortcut  int    `json:"shortcut"`
	State     string `json:"state,omitempty"`
	IdleLevel int    `json:"idle_level,omitempty"`
	IdleAgo   string `json:"idle_ago,omitempty"`
	Folder    string `json:"folder,omitempty"`
	Title     string `json:"title,omitempty"`
	Unpushed  int    `json:"unpushed,omitempty"`
	Active    bool   `json:"active,omitempty"`
	IsClaude  *bool  `json:"is_claude,omitempty"`
	App       string `json:"app,omitempty"`
	AppIcon   string `json:"app_icon,omitempty"`
}

// Run executes the tile-data subcommand. args is os.Args[2:]. It parses an
// optional --output/--db and a single positional desktop index, builds the
// payload, prints it as JSON, and ALWAYS returns nil (a bad arg or any error
// degrades to a minimal payload; the bar must never break).
func Run(args []string) error {
	dbPath := db.DefaultDBPath()
	output := defaultOutput
	fs := flag.NewFlagSet("tile-data", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&dbPath, "db", dbPath, "path to the claude-status sqlite database")
	fs.StringVar(&output, "output", output, "niri output (connector) the tile's desktop lives on")
	_ = fs.Parse(args)

	idx, err := strconv.Atoi(fs.Arg(0))
	if err != nil {
		// No usable index: emit a harmless empty tile rather than fail.
		idx = 0
	}

	p := build(dbPath, output, idx)
	_ = json.NewEncoder(os.Stdout).Encode(p)
	return nil
}

// build assembles the tile payload for desktop `idx` on `output`. It never
// errors: any failed lookup degrades to the minimal/empty payload.
func build(dbPath, output string, idx int) payload {
	empty := payload{Shortcut: idx, State: string(state.Idle), IdleLevel: state.DecayLevels - 1}

	wss, err := niri.ListWorkspaces()
	if err != nil {
		return empty
	}
	var ws *niri.Workspace
	for i := range wss {
		if wss[i].Output == output && wss[i].Idx == idx {
			ws = &wss[i]
			break
		}
	}
	if ws == nil {
		return empty // no such desktop on this output
	}

	p := payload{Shortcut: idx, Active: ws.IsFocused}

	// Windows currently on this workspace.
	wins, _ := niri.ListWindows()
	var onWs []niri.Window
	for _, w := range wins {
		if w.WorkspaceID == ws.ID {
			onWs = append(onWs, w)
		}
	}
	if len(onWs) == 0 {
		empty.Active = ws.IsFocused
		return empty // existing but empty desktop
	}

	// Index live sessions by their cached window id.
	byWin := map[int]db.Session{}
	if database, derr := db.Open(dbPath); derr == nil {
		defer database.Close()
		if sessions, lerr := database.LoadLive(); lerr == nil {
			for _, s := range sessions {
				if s.WindowID.Valid {
					byWin[int(s.WindowID.Int64)] = s
				}
			}
		}
	}

	// A Claude desktop: the first window on it that maps to a tracked session.
	for _, w := range onWs {
		s, ok := byWin[w.ID]
		if !ok {
			continue
		}
		p.State = s.State // REAL hook state (first-party overlay TODO: bead)
		if s.Cwd.Valid {
			p.Folder = filepath.Base(s.Cwd.String)
		}
		p.Title = w.Title
		if state.Status(s.State) == state.Idle && s.LastTalkTS.Valid {
			elapsed := time.Duration(db.Now().UnixMilli()-s.LastTalkTS.Int64) * time.Millisecond
			p.IdleLevel = state.DecayLevel(elapsed)
			p.IdleAgo = fmtAgo(elapsed)
		}
		return p
	}

	// An ordinary (non-Claude) desktop: show the leftmost window's app + icon.
	isClaude := false
	p.IsClaude = &isClaude
	w0 := onWs[0]
	p.App = w0.AppID     // MOCK: a human label would be nicer (bead)
	p.AppIcon = w0.AppID // MOCK: needs app_id -> icon mapping (bead)
	p.Title = w0.Title
	return p
}

// fmtAgo renders an elapsed duration as the tile's "time since active" string:
// "<60m" as minutes ("12m"), otherwise hours ("2h").
func fmtAgo(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	m := int(d.Minutes())
	if m < 60 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dh", m/60)
}
