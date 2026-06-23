// Package tile implements `claude-status tile-data <index>` — the producer that
// bridges claude-status-db + niri to the pwetty `claude` waybar tile — plus the
// shared payload model and the daemon-written tile cache.
//
// It emits, on stdout, the JSON data object the bundled pwetty `claude` tile
// expects (contract: ~/Perso/pwetty-box-rs/tiles/claude/schema.json) for ONE
// niri desktop, identified by its per-output workspace index.
//
// HOT PATH: a waybar `cffi/pwetty#i` module reruns this every interval, once per
// tile. To keep that cheap (a regression once hammered niri with `niri msg` IPC
// and stuttered desktop switches), the long-lived daemon — which already holds
// the niri model + sessions live — precomputes every desktop's payload and
// writes them to a cache file (WriteCache) on its tick. `tile-data` then just
// reads that file (ReadCache): no `niri msg` spawn, no DB open. The live path
// (BuildLive) remains only as a fallback when the cache is absent (no daemon).
//
// Like the hook, the CLI ALWAYS prints valid JSON and returns nil.
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
// (indexes repeat across outputs). Overridable with --output.
const defaultOutput = "HDMI-A-1"

// Payload is the pwetty `claude` tile data object. Fields use omitempty so an
// absent value falls back to the tile's schema default. See schema.json for the
// REAL (from claude-status-db) vs MOCK (synthesized) provenance of each field.
type Payload struct {
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

// Key is the tile-cache key for a desktop: "<output>:<idx>". Indexes repeat
// across outputs, so the output qualifies them.
func Key(output string, idx int) string { return output + ":" + strconv.Itoa(idx) }

// CachePath is where the daemon writes the precomputed tiles, next to the DB.
func CachePath(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), "tiles.json")
}

// emptyPayload is the placeholder for an absent/empty desktop: the shortcut on a
// fully-faded idle bar. (Polishing this to the bundled `empty` tile is beaded.)
func emptyPayload(idx int, active bool) Payload {
	return Payload{Shortcut: idx, State: string(state.Idle), IdleLevel: state.DecayLevels - 1, Active: active}
}

// PayloadFor computes the tile payload for one workspace from already-gathered
// state: the workspace, the windows currently on it, sessions indexed by their
// cached window id, and the clock. PURE (no IPC/DB) so both the daemon and the
// CLI fallback share it and it is unit-testable.
//
// An empty desktop -> emptyPayload. A desktop with a window mapping to a tracked
// session -> the Claude layout (state/folder/decay/title). Otherwise -> the app
// layout (leftmost window's app + icon). Note: the daemon passes its first-party
// overlaid sessions, so the cached State is the effective state, not raw hooks.
func PayloadFor(ws niri.Workspace, winsOnWs []niri.Window, byWin map[int]db.Session, now time.Time) Payload {
	if len(winsOnWs) == 0 {
		return emptyPayload(ws.Idx, ws.IsFocused)
	}
	p := Payload{Shortcut: ws.Idx, Active: ws.IsFocused}
	for _, w := range winsOnWs {
		s, ok := byWin[w.ID]
		if !ok {
			continue
		}
		p.State = s.State
		if s.Cwd.Valid {
			p.Folder = filepath.Base(s.Cwd.String)
		}
		p.Title = w.Title
		if state.Status(s.State) == state.Idle && s.LastTalkTS.Valid {
			elapsed := now.Sub(time.UnixMilli(s.LastTalkTS.Int64))
			p.IdleLevel = state.DecayLevel(elapsed)
			p.IdleAgo = fmtAgo(elapsed)
		}
		return p
	}
	// No tracked session on this desktop: the app layout for the leftmost window.
	isClaude := false
	p.IsClaude = &isClaude
	w0 := winsOnWs[0]
	p.App = w0.AppID     // MOCK: a human label would be nicer (bead r1d)
	p.AppIcon = w0.AppID // MOCK: needs app_id -> icon mapping (bead r1d)
	p.Title = w0.Title
	return p
}

// BuildAll computes payloads for every workspace, keyed by Key(output, idx). The
// daemon calls this with its in-memory model + sessions (no IPC) and writes the
// result via WriteCache.
func BuildAll(workspaces map[int]niri.Workspace, windows []niri.Window, sessions []db.Session, now time.Time) map[string]Payload {
	byWin := make(map[int]db.Session, len(sessions))
	for _, s := range sessions {
		if s.WindowID.Valid {
			byWin[int(s.WindowID.Int64)] = s
		}
	}
	winsByWs := make(map[int][]niri.Window)
	for _, w := range windows {
		winsByWs[w.WorkspaceID] = append(winsByWs[w.WorkspaceID], w)
	}
	out := make(map[string]Payload, len(workspaces))
	for _, ws := range workspaces {
		out[Key(ws.Output, ws.Idx)] = PayloadFor(ws, winsByWs[ws.ID], byWin, now)
	}
	return out
}

// WriteCache atomically writes the tile cache to path (temp file + rename, so a
// concurrent reader never sees a half-written file).
func WriteCache(path string, tiles map[string]Payload) error {
	data, err := json.Marshal(tiles)
	if err != nil {
		return err
	}
	return WriteCacheBytes(path, data)
}

// WriteCacheBytes atomically writes pre-marshaled cache bytes to path. The
// daemon marshals once so it can dedupe (skip the write when nothing changed)
// before calling this.
func WriteCacheBytes(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReadCache loads the tile cache written by the daemon.
func ReadCache(path string) (map[string]Payload, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var tiles map[string]Payload
	if err := json.Unmarshal(data, &tiles); err != nil {
		return nil, err
	}
	return tiles, nil
}

// Run executes the tile-data subcommand. args is os.Args[2:]. It reads the
// daemon's cache for the desktop (the cheap hot path) and, only if the cache is
// missing, falls back to a live niri+DB query. It ALWAYS prints valid JSON and
// returns nil.
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
		idx = 0
	}

	var p Payload
	if tiles, rerr := ReadCache(CachePath(dbPath)); rerr == nil {
		// Cache hit path: a single file read, no niri/DB. Missing key (a desktop
		// the daemon didn't see) -> empty placeholder.
		var ok bool
		if p, ok = tiles[Key(output, idx)]; !ok {
			p = emptyPayload(idx, false)
		}
	} else {
		// No cache (daemon not running): degrade to a live query.
		p = BuildLive(dbPath, output, idx)
	}
	_ = json.NewEncoder(os.Stdout).Encode(p)
	return nil
}

// BuildLive gathers state directly from niri + the DB for one desktop. It is the
// fallback when the daemon's cache is unavailable; the steady state uses the
// cache instead (BuildAll + WriteCache in the daemon).
func BuildLive(dbPath, output string, idx int) Payload {
	wss, err := niri.ListWorkspaces()
	if err != nil {
		return emptyPayload(idx, false)
	}
	var ws *niri.Workspace
	for i := range wss {
		if wss[i].Output == output && wss[i].Idx == idx {
			ws = &wss[i]
			break
		}
	}
	if ws == nil {
		return emptyPayload(idx, false)
	}

	wins, _ := niri.ListWindows()
	var onWs []niri.Window
	for _, w := range wins {
		if w.WorkspaceID == ws.ID {
			onWs = append(onWs, w)
		}
	}

	var sessions []db.Session
	if database, derr := db.Open(dbPath); derr == nil {
		defer database.Close()
		sessions, _ = database.LoadLive()
	}
	byWin := make(map[int]db.Session, len(sessions))
	for _, s := range sessions {
		if s.WindowID.Valid {
			byWin[int(s.WindowID.Int64)] = s
		}
	}
	return PayloadFor(*ws, onWs, byWin, db.Now())
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
