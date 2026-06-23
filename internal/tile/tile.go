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
	"sort"
	"strconv"
	"strings"
	"sync"
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
	// Stable order: the daemon groups windows from a map (randomized iteration),
	// so without this the chosen window (and thus the app-layout tile) flips on
	// every cache rebuild — a desktop with 2+ windows visibly blinks between
	// them. Sort by the stable window id so the pick is deterministic.
	if len(winsOnWs) > 1 {
		sort.Slice(winsOnWs, func(i, j int) bool { return winsOnWs[i].ID < winsOnWs[j].ID })
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
	p.App = cleanAppLabel(w0.AppID)
	p.AppIcon = resolveAppIcon(w0.AppID)
	p.Title = w0.Title
	return p
}

// iconMemo caches app_id -> resolved app_icon so repeated cache rebuilds don't
// re-stat the icon themes. Concurrency-safe (the CLI and daemon are separate
// processes; within the daemon only the actor goroutine touches it).
var iconMemo sync.Map

// appIconDirs are the scalable (SVG) app-icon directories searched, in
// preference order. pwetty's <icon src> is SVG-only (resvg), so only .svg is
// considered — a sized PNG can't render.
func appIconDirs() []string {
	var dirs []string
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".local/share/icons/hicolor/scalable/apps"))
	}
	return append(dirs,
		"/usr/share/icons/hicolor/scalable/apps",
		"/usr/share/icons/Papirus/scalable/apps",
		"/usr/local/share/icons/hicolor/scalable/apps",
	)
}

// resolveAppIcon maps a niri app_id to the tile's app_icon: an absolute .svg
// path (drawn as the big hero icon) when a matching freedesktop icon exists,
// else the bundled generic "app" glyph. Memoized.
func resolveAppIcon(appID string) string {
	if appID == "" {
		return "app"
	}
	if v, ok := iconMemo.Load(appID); ok {
		return v.(string)
	}
	res := findAppSVG(appID)
	if res == "" {
		res = "app"
	}
	iconMemo.Store(appID, res)
	return res
}

func findAppSVG(appID string) string {
	for _, dir := range appIconDirs() {
		for _, c := range iconCandidates(appID) {
			p := filepath.Join(dir, c+".svg")
			if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
				return p
			}
		}
	}
	return ""
}

// iconCandidates are the icon basenames to try for an app_id, e.g.
// "org.keepassxc.KeePassXC" -> [the full id, lowercased, "KeePassXC",
// "keepassxc"], so a reverse-DNS app_id still finds keepassxc.svg.
func iconCandidates(appID string) []string {
	out := []string{appID, strings.ToLower(appID)}
	if i := strings.LastIndex(appID, "."); i >= 0 && i+1 < len(appID) {
		last := appID[i+1:]
		out = append(out, last, strings.ToLower(last))
	}
	return out
}

// cleanAppLabel turns an app_id into a display label: the last dotted component,
// with an all-lowercase name capitalized ("firefox" -> "Firefox") and a
// mixed-case one left intact ("KeePassXC").
func cleanAppLabel(appID string) string {
	s := appID
	if i := strings.LastIndex(s, "."); i >= 0 && i+1 < len(s) {
		s = s[i+1:]
	}
	if s == "" {
		return appID
	}
	if s == strings.ToLower(s) {
		return strings.ToUpper(s[:1]) + s[1:]
	}
	return s
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

// watchPoll is how often tile-watch re-reads the cache. The cache only changes
// at human cadence (and the daemon writes it immediately on a switch), so a
// short poll here keeps detection latency low at negligible cost (a small file
// read). pwetty then repaints within ~150ms of the printed line.
const watchPoll = 75 * time.Millisecond

// RunWatch implements `claude-status tile-watch <index>` — the STREAMING
// producer for pwetty's `stream: true` mode. It prints the desktop's tile JSON
// line immediately, then prints a fresh newline-delimited JSON line whenever
// this desktop's cached payload changes. One long-lived process per tile (no
// per-refresh process spawn, no niri/DB access), so pwetty pushes updates within
// ~150ms instead of waiting on a 1s poll. Always returns nil after blocking
// forever; on any read error it emits the empty placeholder.
func RunWatch(args []string) error {
	dbPath := db.DefaultDBPath()
	output := defaultOutput
	fs := flag.NewFlagSet("tile-watch", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&dbPath, "db", dbPath, "path to the claude-status sqlite database")
	fs.StringVar(&output, "output", output, "niri output (connector) the tile's desktop lives on")
	_ = fs.Parse(args)
	idx, err := strconv.Atoi(fs.Arg(0))
	if err != nil {
		idx = 0
	}
	key := Key(output, idx)
	path := CachePath(dbPath)

	var last string
	emit := func() {
		p := emptyPayload(idx, false)
		if tiles, rerr := ReadCache(path); rerr == nil {
			if cached, ok := tiles[key]; ok {
				p = cached
			}
		}
		b, merr := json.Marshal(p)
		if merr != nil {
			return
		}
		if s := string(b); s != last {
			last = s
			// One Write of the full line so pwetty's line reader sees it whole.
			_, _ = os.Stdout.Write(append(b, '\n'))
		}
	}

	emit() // initial line so the tile shows immediately
	for {
		time.Sleep(watchPoll)
		emit()
	}
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
