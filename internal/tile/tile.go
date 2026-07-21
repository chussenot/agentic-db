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
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"image/png"
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

// Payload is the pwetty `claude` tile data object for ONE niri desktop. The tile
// is data-driven (contract: ~/Perso/pwetty-box-rs/tiles/claude/schema.json): a
// Claude desktop carries `sessions` (1 -> single layout, 2 -> stacked dual), an
// ordinary window carries is_claude=false + app/app_icon/title. Fields use
// omitempty so an absent value falls back to the tile's schema default.
type Payload struct {
	Shortcut int           `json:"shortcut"`
	Active   bool          `json:"active,omitempty"`
	Sessions []SessionTile `json:"sessions,omitempty"`
	IsClaude *bool         `json:"is_claude,omitempty"`
	App      string        `json:"app,omitempty"`
	AppIcon  string        `json:"app_icon,omitempty"`
	Title    string        `json:"title,omitempty"` // is_claude=false (window) only
}

// SessionTile is one Claude session within a desktop's `sessions` array. State is
// required (drives the indicator); the rest fall back to schema defaults when
// omitted. Two sessions sharing one niri window (e.g. two kitty tabs) share that
// window's Title — niri exposes no per-tab title.
type SessionTile struct {
	State     string `json:"state"`
	Folder    string `json:"folder,omitempty"`
	Title     string `json:"title,omitempty"`
	Unpushed  int    `json:"unpushed,omitempty"`
	IdleLevel int    `json:"idle_level,omitempty"`
	IdleAgo   string `json:"idle_ago,omitempty"`
}

// maxSessionsPerTile caps the sessions array at the contract's maxItems (2). When
// a desktop has more, statePriority ordering keeps the most salient two (a prompt
// is never dropped) so the tile's "any session prompt -> whole tile pulses" alert
// still fires.
const maxSessionsPerTile = 2

// Key is the tile-cache key for a desktop: "<output>:<idx>". Indexes repeat
// across outputs, so the output qualifies them.
func Key(output string, idx int) string { return output + ":" + strconv.Itoa(idx) }

// CachePath is where the daemon writes the precomputed tiles, next to the DB.
func CachePath(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), "tiles.json")
}

// emptyPayload is the placeholder for an absent/empty desktop: the shortcut on a
// fully-faded idle bar, expressed as a single dimmed idle session so it satisfies
// the data-driven `sessions` contract. (Polishing this to the bundled `empty`
// tile is beaded.)
func emptyPayload(idx int, active bool) Payload {
	return Payload{
		Shortcut: idx,
		Active:   active,
		Sessions: []SessionTile{{State: string(state.Idle), IdleLevel: state.DecayLevels - 1}},
	}
}

// statePriority orders sessions for the dual layout and the maxSessionsPerTile
// cap: prompt first (never dropped — it drives the tile pulse), then working,
// shell, idle. Unknown states sort last.
func statePriority(s string) int {
	switch state.Status(s) {
	case state.Prompt:
		return 0
	case state.Working:
		return 1
	case state.Shell:
		return 2
	case state.Idle:
		return 3
	default:
		return 4
	}
}

// sessionTile builds one SessionTile from a DB session and its window title.
func sessionTile(s db.Session, title string, now time.Time) SessionTile {
	st := SessionTile{State: s.State, Title: title}
	if s.Cwd.Valid {
		st.Folder = filepath.Base(s.Cwd.String)
	}
	if state.Status(s.State) == state.Idle && s.LastTalkTS.Valid {
		elapsed := now.Sub(time.UnixMilli(s.LastTalkTS.Int64))
		st.IdleLevel = state.DecayLevel(elapsed)
		st.IdleAgo = fmtAgo(elapsed)
	}
	return st
}

// PayloadFor computes the tile payload for one workspace from already-gathered
// state: the workspace, the windows currently on it, sessions indexed by their
// cached window id, and the clock. PURE (no IPC/DB) so both the daemon and the
// CLI fallback share it and it is unit-testable.
//
// An empty desktop -> emptyPayload. A desktop with one or more tracked sessions
// -> the Claude layout: every session on the desktop as a `sessions[]` entry
// (capped at maxSessionsPerTile, prompt-priority ordered). Co-resident sessions
// that share one niri window (two kitty tabs) BOTH appear — they are grouped by
// workspace, not window, so neither is masked. A desktop with windows but no
// tracked session -> the app layout (leftmost window's app + icon). Note: the
// daemon passes its first-party overlaid sessions, so each State is the effective
// state, not raw hooks.
func PayloadFor(ws niri.Workspace, winsOnWs []niri.Window, sessionsOnWs []db.Session, now time.Time) Payload {
	p := Payload{Shortcut: ws.Idx, Active: ws.IsFocused}

	if len(sessionsOnWs) > 0 {
		// Title comes from the session's niri window (sessions carry no title of
		// their own); co-window sessions share it.
		titleOf := make(map[int]string, len(winsOnWs))
		for _, w := range winsOnWs {
			titleOf[w.ID] = w.Title
		}
		// Deterministic, prompt-first order so the cap keeps the salient sessions
		// and the layout doesn't flip between cache rebuilds. Tie-break on window
		// then session id (both stable).
		ss := append([]db.Session(nil), sessionsOnWs...)
		sort.Slice(ss, func(i, j int) bool {
			pi, pj := statePriority(ss[i].State), statePriority(ss[j].State)
			if pi != pj {
				return pi < pj
			}
			wi, wj := ss[i].WindowID.Int64, ss[j].WindowID.Int64
			if wi != wj {
				return wi < wj
			}
			return ss[i].SessionID < ss[j].SessionID
		})
		if len(ss) > maxSessionsPerTile {
			ss = ss[:maxSessionsPerTile]
		}
		for _, s := range ss {
			title := ""
			if s.WindowID.Valid {
				title = titleOf[int(s.WindowID.Int64)]
			}
			p.Sessions = append(p.Sessions, sessionTile(s, title, now))
		}
		return p
	}

	if len(winsOnWs) == 0 {
		return emptyPayload(ws.Idx, ws.IsFocused)
	}

	// No tracked session on this desktop: the app layout for the leftmost window.
	// Stable order: the daemon groups windows from a map (randomized iteration),
	// so without this the chosen window flips on every cache rebuild — a desktop
	// with 2+ windows visibly blinks between them.
	w0 := winsOnWs[0]
	for _, w := range winsOnWs[1:] {
		if w.ID < w0.ID {
			w0 = w
		}
	}
	isClaude := false
	p.IsClaude = &isClaude
	p.App = cleanAppLabel(w0.AppID)
	p.AppIcon = resolveAppIcon(w0.AppID)
	p.Title = w0.Title
	return p
}

// iconMemo caches app_id -> resolved app_icon so repeated cache rebuilds don't
// re-stat the icon themes. Concurrency-safe (the CLI and daemon are separate
// processes; within the daemon only the actor goroutine touches it).
var iconMemo sync.Map

// iconThemeBases are the hicolor icon-theme roots searched for app icons, in
// preference order — including the flatpak export trees (e.g. Slack only
// ships its SVG under /var/lib/flatpak/exports).
func iconThemeBases() []string {
	var bases []string
	if home, err := os.UserHomeDir(); err == nil {
		bases = append(bases,
			filepath.Join(home, ".local/share/icons/hicolor"),
			filepath.Join(home, ".local/share/flatpak/exports/share/icons/hicolor"),
		)
	}
	return append(bases,
		"/usr/share/icons/hicolor",
		"/var/lib/flatpak/exports/share/icons/hicolor",
		"/usr/local/share/icons/hicolor",
	)
}

// appIconDirs are the scalable (SVG) app-icon directories searched, in
// preference order. pwetty's <icon src> is SVG-only (resvg), so a raw PNG
// can't render — sized PNGs are handled by findAppPNG + wrapPNGAsSVG instead.
func appIconDirs() []string {
	var dirs []string
	for _, b := range iconThemeBases() {
		dirs = append(dirs, filepath.Join(b, "scalable/apps"))
	}
	return append(dirs, "/usr/share/icons/Papirus/scalable/apps")
}

// resolveAppIcon maps a niri app_id to the tile's app_icon: an absolute .svg
// path (drawn as the big hero icon) when a matching freedesktop icon exists —
// a real SVG, or a sized PNG wrapped into a cached SVG shim (pwetty's resvg
// renders embedded raster images) — else the bundled generic "app" glyph.
// Memoized.
func resolveAppIcon(appID string) string {
	if appID == "" {
		return "app"
	}
	if v, ok := iconMemo.Load(appID); ok {
		return v.(string)
	}
	res := findAppSVG(appID)
	if res == "" {
		if p := findAppPNG(appID); p != "" {
			if svg, err := wrapPNGAsSVG(appID, p); err == nil {
				res = svg
			}
		}
	}
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

// pngIconSizes are the sized hicolor dirs tried for the PNG fallback, biggest
// first (the tile hero icon rasterizes well under 512px).
var pngIconSizes = []string{"512x512", "256x256", "192x192", "128x128", "96x96", "64x64", "48x48"}

// findAppPNG finds the largest sized hicolor PNG for an app_id (e.g. Chrome
// ships only PNGs), falling back to /usr/share/pixmaps.
func findAppPNG(appID string) string {
	for _, base := range iconThemeBases() {
		for _, size := range pngIconSizes {
			for _, c := range iconCandidates(appID) {
				p := filepath.Join(base, size, "apps", c+".png")
				if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
					return p
				}
			}
		}
	}
	for _, c := range iconCandidates(appID) {
		p := filepath.Join("/usr/share/pixmaps", c+".png")
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return ""
}

// wrapPNGAsSVG embeds a PNG as a base64 <image> in a minimal SVG under
// ~/.cache/claude-status/icons/, so pwetty's SVG-only <icon src> (resvg, built
// with raster-images) can draw apps that ship no vector icon. The shim is
// written once and reused across runs.
func wrapPNGAsSVG(appID, pngPath string) (string, error) {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(cacheDir, "claude-status", "icons")
	name := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, appID)
	out := filepath.Join(dir, name+".svg")
	if fi, err := os.Stat(out); err == nil && !fi.IsDir() {
		return out, nil
	}
	raw, err := os.ReadFile(pngPath)
	if err != nil {
		return "", err
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	svg := fmt.Sprintf(
		`<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink" width="%d" height="%d"><image width="%d" height="%d" xlink:href="data:image/png;base64,%s"/></svg>`,
		cfg.Width, cfg.Height, cfg.Width, cfg.Height, base64.StdEncoding.EncodeToString(raw))
	// Atomic write (unique tmp + rename): the daemon and a cacheless CLI run
	// can resolve the same app concurrently.
	tmp, err := os.CreateTemp(dir, name+".*.tmp")
	if err != nil {
		return "", err
	}
	if _, err := tmp.WriteString(svg); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	if err := os.Rename(tmp.Name(), out); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return out, nil
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
	winByID := make(map[int]niri.Window, len(windows))
	winsByWs := make(map[int][]niri.Window)
	for _, w := range windows {
		winByID[w.ID] = w
		winsByWs[w.WorkspaceID] = append(winsByWs[w.WorkspaceID], w)
	}
	// Group sessions by WORKSPACE (via their window's workspace), not by window:
	// two sessions can share one window (kitty tabs), and both must surface.
	sessByWs := make(map[int][]db.Session)
	for _, s := range sessions {
		if !s.WindowID.Valid {
			continue
		}
		if w, ok := winByID[int(s.WindowID.Int64)]; ok {
			sessByWs[w.WorkspaceID] = append(sessByWs[w.WorkspaceID], s)
		}
	}
	out := make(map[string]Payload, len(workspaces))
	for _, ws := range workspaces {
		out[Key(ws.Output, ws.Idx)] = PayloadFor(ws, winsByWs[ws.ID], sessByWs[ws.ID], now)
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
	winByID := make(map[int]niri.Window, len(onWs))
	for _, w := range onWs {
		winByID[w.ID] = w
	}
	var sessOnWs []db.Session
	for _, s := range sessions {
		if !s.WindowID.Valid {
			continue
		}
		if _, ok := winByID[int(s.WindowID.Int64)]; ok {
			sessOnWs = append(sessOnWs, s)
		}
	}
	return PayloadFor(*ws, onWs, sessOnWs, db.Now())
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
