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
	"bufio"
	"encoding/base64"
	"encoding/binary"
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

// appIconDirs are the scalable (SVG) app-icon directories searched, in
// preference order. pwetty's <icon src> is SVG-only (resvg), so only .svg is
// considered — a sized PNG can't render.
func appIconDirs() []string {
	var dirs []string
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs,
			filepath.Join(home, ".local/share/icons/hicolor/scalable/apps"),
			// Per-user flatpak exports (basename == reverse-DNS app_id, which niri
			// reports verbatim, e.g. com.slack.Slack.svg).
			filepath.Join(home, ".local/share/flatpak/exports/share/icons/hicolor/scalable/apps"),
		)
	}
	return append(dirs,
		"/usr/share/icons/hicolor/scalable/apps",
		"/usr/share/icons/Papirus/scalable/apps",
		"/usr/local/share/icons/hicolor/scalable/apps",
		// System-wide flatpak exports — where slack/vesktop et al. actually ship
		// their scalable icon.
		"/var/lib/flatpak/exports/share/icons/hicolor/scalable/apps",
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
	res := findIcon(appID)
	if res == "" {
		res = "app"
	}
	iconMemo.Store(appID, res)
	return res
}

// findIcon resolves an app_id to an absolute .svg path — a native scalable icon
// or a generated PNG wrapper — or "". It tries, in order: the app_id itself,
// then the Icon= of a matching freedesktop .desktop entry (which bridges a WM-
// class app_id like "vesktop" to its differently-named icon "dev.vencord.Vesktop").
// For all keys it prefers a native SVG before falling back to wrapping the best
// PNG (e.g. firefox, which ships PNG-only + a monochrome symbolic SVG), so
// pwetty's SVG-only renderer (resvg, which draws embedded raster) shows the real
// logo instead of the generic "app" bubble.
func findIcon(appID string) string {
	keys := []string{appID}
	if icon := desktopEntryIcon(appID); icon != "" && icon != appID {
		keys = append(keys, icon)
	}
	for _, k := range keys {
		if p := iconFromName(k); p != "" {
			return p
		}
	}
	for _, k := range keys {
		if p := wrappedPNGIcon(k); p != "" {
			return p
		}
	}
	return ""
}

// iconFromName resolves an Icon= value (or app_id) to a native .svg: an absolute
// path is honored directly (SVG only — pwetty can't draw a raw PNG path),
// otherwise it is looked up as a freedesktop icon name.
func iconFromName(name string) string {
	if filepath.IsAbs(name) {
		if strings.HasSuffix(name, ".svg") {
			if fi, err := os.Stat(name); err == nil && !fi.IsDir() {
				return name
			}
		}
		return ""
	}
	return findAppSVG(name)
}

// appDirs are the freedesktop application-entry directories searched for a
// matching .desktop file, in precedence order (user, flatpak exports, system).
func appDirs() []string {
	var dirs []string
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".local/share/applications"))
	}
	return append(dirs,
		"/var/lib/flatpak/exports/share/applications",
		"/usr/local/share/applications",
		"/usr/share/applications",
	)
}

// desktopEntryIcon returns the Icon= of the .desktop entry that matches appID —
// first by filename (<appID>.desktop), then by a StartupWMClass= equal to appID
// (case-insensitive). This bridges a Wayland/X11 app_id to a differently named
// icon (e.g. app_id "vesktop" -> Icon "dev.vencord.Vesktop"). "" if none.
func desktopEntryIcon(appID string) string {
	for _, dir := range appDirs() {
		if icon, _ := parseDesktop(filepath.Join(dir, appID+".desktop")); icon != "" {
			return icon
		}
	}
	for _, dir := range appDirs() {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".desktop") {
				continue
			}
			icon, wm := parseDesktop(filepath.Join(dir, e.Name()))
			if icon != "" && wm != "" && strings.EqualFold(wm, appID) {
				return icon
			}
		}
	}
	return ""
}

// parseDesktop reads a .desktop file's Icon= and StartupWMClass= from its
// [Desktop Entry] group (later groups like [Desktop Action …] are ignored).
// Missing file/keys -> empty strings.
func parseDesktop(path string) (icon, wmClass string) {
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer f.Close()
	inEntry := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			if inEntry {
				break // left [Desktop Entry] for the next group
			}
			inEntry = line == "[Desktop Entry]"
			continue
		}
		if !inEntry {
			continue
		}
		if v, ok := cutKey(line, "Icon"); ok && icon == "" {
			icon = v
		} else if v, ok := cutKey(line, "StartupWMClass"); ok && wmClass == "" {
			wmClass = v
		}
	}
	_ = sc.Err() // best-effort parse; a truncated read just yields fewer keys
	return icon, wmClass
}

// cutKey returns the value of a `Key=value` line for exactly key (so "Icon"
// doesn't match "IconName="), trimmed. ok is false when the line isn't that key.
func cutKey(line, key string) (string, bool) {
	if !strings.HasPrefix(line, key) {
		return "", false
	}
	rest := strings.TrimSpace(line[len(key):])
	if !strings.HasPrefix(rest, "=") {
		return "", false
	}
	return strings.TrimSpace(rest[1:]), true
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

// appIconThemeRoots are the hicolor theme roots holding sized (PNG) app icons —
// the same coverage as appIconDirs (user, system, flatpak exports) minus the
// scalable/apps leaf, since PNGs live under per-size subdirs (16x16 … 512x512).
func appIconThemeRoots() []string {
	var roots []string
	if home, err := os.UserHomeDir(); err == nil {
		roots = append(roots,
			filepath.Join(home, ".local/share/icons/hicolor"),
			filepath.Join(home, ".local/share/flatpak/exports/share/icons/hicolor"),
		)
	}
	return append(roots,
		"/usr/share/icons/hicolor",
		"/usr/local/share/icons/hicolor",
		"/var/lib/flatpak/exports/share/icons/hicolor",
	)
}

// sizeDirPixels parses a hicolor size subdir name ("128x128" -> 128). Non-sized
// dirs ("scalable", "symbolic") -> 0 so they never win the largest-PNG pick.
func sizeDirPixels(name string) int {
	i := strings.IndexByte(name, 'x')
	if i <= 0 {
		return 0
	}
	n, err := strconv.Atoi(name[:i])
	if err != nil {
		return 0
	}
	return n
}

// findAppPNG returns the highest-resolution PNG app icon for appID across the
// theme roots (larger is preferred so the wrapped hero icon stays crisp), or ""
// if none. Falls back to unsized /usr/share/pixmaps last.
func findAppPNG(appID string) string {
	best, bestPx := "", 0
	cands := iconCandidates(appID)
	for _, root := range appIconThemeRoots() {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			px := sizeDirPixels(e.Name())
			if px <= bestPx {
				continue
			}
			for _, c := range cands {
				p := filepath.Join(root, e.Name(), "apps", c+".png")
				if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
					best, bestPx = p, px
					break
				}
			}
		}
	}
	if best == "" {
		for _, c := range cands {
			p := filepath.Join("/usr/share/pixmaps", c+".png")
			if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
				return p
			}
		}
	}
	return best
}

// wrappedPNGIcon renders the best PNG icon for name (an icon name or an absolute
// .png path from a desktop entry's Icon=) into a cached wrapper SVG — a single
// <image> with the PNG embedded as a base64 data URI — and returns its path, or
// "" if no PNG exists or the wrap fails. The wrapper is cached under the user
// cache dir and regenerated only when missing or older than its source PNG.
func wrappedPNGIcon(name string) string {
	png := ""
	if filepath.IsAbs(name) && strings.HasSuffix(name, ".png") {
		if fi, err := os.Stat(name); err == nil && !fi.IsDir() {
			png = name
		}
	} else {
		png = findAppPNG(name)
	}
	if png == "" {
		return ""
	}
	dir, err := iconCacheDir()
	if err != nil {
		return ""
	}
	out := filepath.Join(dir, sanitizeIconName(name)+".svg")
	if wrapperFresh(out, png) {
		return out
	}
	svg, err := buildPNGWrapperSVG(png)
	if err != nil {
		return ""
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ""
	}
	tmp := out + ".tmp"
	if err := os.WriteFile(tmp, svg, 0o644); err != nil {
		return ""
	}
	if err := os.Rename(tmp, out); err != nil {
		return ""
	}
	return out
}

// iconCacheDir is where generated PNG-wrapper SVGs live.
func iconCacheDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "claude-status", "app-icons"), nil
}

// wrapperFresh reports whether the cached wrapper is present and no older than
// its source PNG (so an app upgrade that ships a new icon regenerates it).
func wrapperFresh(wrapper, src string) bool {
	wi, err := os.Stat(wrapper)
	if err != nil {
		return false
	}
	si, err := os.Stat(src)
	if err != nil {
		return false
	}
	return !wi.ModTime().Before(si.ModTime())
}

// sanitizeIconName maps an app_id to a filesystem-safe cache basename (reverse-
// DNS ids are already close; this just neutralizes any stray separators).
func sanitizeIconName(appID string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, appID)
}

// buildPNGWrapperSVG embeds a PNG file as a base64 data URI inside a minimal SVG
// sized to the PNG's own pixel dimensions. Both href and xlink:href are omitted
// in favor of xlink:href, which every resvg version accepts.
func buildPNGWrapperSVG(pngPath string) ([]byte, error) {
	data, err := os.ReadFile(pngPath)
	if err != nil {
		return nil, err
	}
	w, h, err := pngDimensions(data)
	if err != nil {
		return nil, err
	}
	enc := base64.StdEncoding.EncodeToString(data)
	svg := fmt.Sprintf(
		`<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink" `+
			`width="%d" height="%d" viewBox="0 0 %d %d">`+
			`<image width="%d" height="%d" xlink:href="data:image/png;base64,%s"/></svg>`,
		w, h, w, h, w, h, enc)
	return []byte(svg), nil
}

// pngDimensions extracts (width, height) from a PNG's IHDR chunk: an 8-byte
// signature, then a 4-byte length + "IHDR", then big-endian 4-byte width and
// height. Enough of the header to size the wrapper's viewBox.
func pngDimensions(data []byte) (int, int, error) {
	const sig = "\x89PNG\r\n\x1a\n"
	if len(data) < 24 || string(data[:8]) != sig || string(data[12:16]) != "IHDR" {
		return 0, 0, fmt.Errorf("not a PNG")
	}
	w := int(binary.BigEndian.Uint32(data[16:20]))
	h := int(binary.BigEndian.Uint32(data[20:24]))
	if w <= 0 || h <= 0 {
		return 0, 0, fmt.Errorf("bad PNG dimensions: %dx%d", w, h)
	}
	return w, h, nil
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
