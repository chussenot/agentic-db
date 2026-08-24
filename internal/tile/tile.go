// Package tile implements `claude-status tile-data <index>` — the producer that
// bridges claude-status-db + niri to the pwetty `claude` waybar tile — plus the
// shared payload model and the daemon-written tile cache.
//
// It emits, on stdout, the JSON data object the bundled pwetty `claude` tile
// expects (contract: ~/Perso/pwetty-box-rs/tiles/claude/schema.json) for ONE
// niri desktop, identified by its per-output workspace index: a shortcut plus an
// `items` array of 1-2 rows, each row either a Claude session or an ordinary
// window. We only build the rows; their count picks the layout.
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

	"github.com/mrzor/claude-status/internal/clauded"
	"github.com/mrzor/claude-status/internal/db"
	"github.com/mrzor/claude-status/internal/fleet"
	"github.com/mrzor/claude-status/internal/niri"
	"github.com/mrzor/claude-status/internal/state"
)

// defaultOutput is the niri connector the second bar lives on. The producer is
// scoped to one output so a bare desktop index uniquely identifies a workspace
// (indexes repeat across outputs). Overridable with --output.
const defaultOutput = "HDMI-A-1"

// Payload is the pwetty `claude` tile data object for ONE niri desktop. The tile
// is data-driven (contract: ~/Perso/pwetty-box-rs/tiles/claude/schema.json): we
// send the desktop's window(s) as `items` — 1 row -> rich single layout, 2 rows
// -> stacked dual layout with a shared shortcut gutter — and the row count picks
// the layout, never us. Fields use omitempty so an absent value falls back to the
// tile's schema default.
//
// The legacy feeds this replaced (a bare `sessions` array; the flat
// is_claude/app/app_icon/title window fields) still validate upstream, but they
// couldn't express a MIXED desktop, so a plain window sitting next to a Claude
// session was simply dropped from the tile.
type Payload struct {
	Shortcut int    `json:"shortcut"`
	Active   bool   `json:"active,omitempty"`
	Items    []Item `json:"items,omitempty"`
}

// Item is one row of a desktop's `items`: either a Claude SESSION row (Kind
// session, State required — it drives the indicator and the tile-wide prompt
// pulse) or an APP row for an ordinary window (Kind app, App + AppIcon required).
// Kind is inferrable upstream from State vs App, but we send it explicitly: it
// must agree with the body, and stating it keeps the daemon's tiles.json legible.
// Two sessions sharing one niri window (e.g. two kitty tabs) share that window's
// Title — niri exposes no per-tab title, and they share WinID for the same reason.
type Item struct {
	Kind      string `json:"kind"`
	State     string `json:"state,omitempty"`      // session rows
	Folder    string `json:"folder,omitempty"`     // session rows
	Unpushed  int    `json:"unpushed,omitempty"`   // session rows
	IdleLevel int    `json:"idle_level,omitempty"` // session rows, state=idle
	IdleAgo   string `json:"idle_ago,omitempty"`   // session rows, state=idle
	App       string `json:"app,omitempty"`        // app rows
	AppIcon   string `json:"app_icon,omitempty"`   // app rows
	Title     string `json:"title,omitempty"`      // both
	// WinID is the niri window this row stands for — what `tile-toggle` focuses
	// when the tile is clicked. The tile itself ignores it (pwetty's schema is
	// additionalProperties, so an unknown key is inert), but shipping it in the
	// same payload is what keeps a click and the painted rows from ever
	// disagreeing. Omitted (0) when the row has no window to focus: a session
	// whose WindowID we never learned.
	WinID int64 `json:"win_id,omitempty"` // both
	// Fleet, when present, says this row stands for a set of BACKGROUND AGENTS —
	// see FleetInfo. The row stays a plain session row at the top level (Kind
	// session, a truthful State, a Folder and a Title), so a template that knows
	// nothing about fleets renders it exactly as it renders any session; a template
	// that does draws the bead strip instead. That is deliberate: `kind` is a
	// CLOSED enum in the contract and rows are discriminated by `state`, so a
	// `kind: "fleet"` row would be schema-invalid and might not paint at all.
	// Presence of this object is the marker, and an unknown key is inert.
	Fleet *FleetInfo `json:"fleet,omitempty"`
}

// FleetInfo is the background-agent payload hung off a row.
//
// Demands is one entry per agent, worst first, and it is what the strip paints: one
// bead per agent, coloured by what that agent wants. Counts come along so a renderer
// can label the strip without counting beads, and Neediest/Needs name the agent that
// set the worst demand plus its ask verbatim — the difference between "something
// wants you" and "project-manager-7 wants you to approve a git mv".
type FleetInfo struct {
	Total    int `json:"total"`
	Live     int `json:"live"`
	Awaiting int `json:"awaiting"`
	Working  int `json:"working"`
	Blocked  int `json:"blocked"`
	Idle     int `json:"idle,omitempty"`
	Done     int `json:"done,omitempty"`
	// Demands is one bead per agent: "awaits-human" | "working" | "blocked" |
	// "idle" | "done".
	Demands  []string `json:"demands"`
	Neediest string   `json:"neediest,omitempty"`
	Needs    string   `json:"needs,omitempty"`
	// Scope is "whole" for a console desktop (the agent view lists every session on
	// the machine) or "owned" for the desktop that spawned the agents. It tells a
	// reader why this desktop is showing this set.
	Scope string `json:"scope"`
}

// The two row kinds, as the contract's `kind` discriminator spells them. There is
// deliberately no third kind for fleets — see Item.Fleet.
const (
	KindSession = "session"
	KindApp     = "app"
)

// Fleet scopes.
const (
	ScopeWhole = "whole" // a console desktop: every agent on the machine
	ScopeOwned = "owned" // the desktop whose session spawned these agents
)

// maxItemsPerTile caps the items array at the contract's maxItems (2). When a
// desktop has more windows, rowPriority ordering keeps the most salient two (a
// prompt is never dropped) so the tile's "any row prompt -> whole tile pulses"
// alert still fires. Picking the two is the producer's job — the tile won't
// decide for us.
const maxItemsPerTile = 2

// Key is the tile-cache key for a desktop: "<output>:<idx>". Indexes repeat
// across outputs, so the output qualifies them.
func Key(output string, idx int) string { return output + ":" + strconv.Itoa(idx) }

// CachePath is where the daemon writes the precomputed tiles, next to the DB.
func CachePath(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), "tiles.json")
}

// emptyPayload is the placeholder for an absent/empty desktop: the shortcut on a
// fully-faded idle bar, expressed as a single dimmed idle session row — a
// windowless desktop is really a different preset (the bundled `empty` tile), NOT
// a 0-row items array, which the contract forbids. Selecting that preset per
// desktop is claude-status-db-wsh.
func emptyPayload(idx int, active bool) Payload {
	return Payload{
		Shortcut: idx,
		Active:   active,
		Items:    []Item{{Kind: KindSession, State: string(state.Idle), IdleLevel: state.DecayLevels - 1}},
	}
}

// rowPriority orders a desktop's rows for the dual layout and the maxItemsPerTile
// cap: prompt first (never dropped — it drives the tile pulse), then working,
// shell, idle, an unknown session state, and finally app rows. Sessions outrank
// ordinary windows: on a mixed desktop the Claude row is the one that must
// survive the cap.
func rowPriority(it Item) int {
	if it.Kind == KindApp {
		return 5
	}
	switch state.Status(it.State) {
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

// FleetView is everything the tile needs to know about background agents. The
// daemon computes it once per tick and hands it in; the tile does no I/O.
type FleetView struct {
	// ByOwner maps a session id to the agents that session spawned, for the
	// desktop that owns them.
	ByOwner map[string]fleet.Summary
	// Whole is every live agent on the machine, which is what a CONSOLE desktop
	// shows: the agent view lists all sessions, not one owner's children.
	Whole fleet.Summary
	// Consoles is the set of niri window ids running the agent view.
	Consoles map[int64]bool
}

// FleetViewFrom assembles the tile's view of background agents from the pieces the
// daemon already computes on its tick, so the daemon does no extra work and the
// fallback path below shares one implementation with it.
//
// consoles is the daemon's STICKY set of window ids that have been seen running the
// agent view. It is passed in rather than derived from the current titles because the
// view retitles its terminal as you move through the list: deriving it per rebuild
// would make a console desktop flip back to describing its resting host session the
// moment you scrolled.
func FleetViewFrom(children map[string]fleet.Child, rollups map[string]fleet.Rollup, consoles map[int64]bool) FleetView {
	fv := FleetView{
		ByOwner:  make(map[string]fleet.Summary, len(rollups)),
		Whole:    fleet.WholeFleet(children),
		Consoles: consoles,
	}
	for id, r := range rollups {
		fv.ByOwner[id] = fleet.Summarize(r)
	}
	if fv.Consoles == nil {
		fv.Consoles = map[int64]bool{}
	}
	return fv
}

// LiveFleetView reads background-agent state straight off disk. It exists for the
// no-daemon fallback path only: the daemon holds all of this in memory already and
// goes through FleetViewFrom instead.
func LiveFleetView(rows []db.Session, windows []niri.Window, now time.Time) FleetView {
	live, err := clauded.ReadLive(clauded.DefaultDir())
	if err != nil || len(live) == 0 {
		return FleetView{}
	}
	jobIDs := make([]string, 0, len(live))
	for _, s := range live {
		if s.JobID != "" {
			jobIDs = append(jobIDs, s.JobID)
		}
	}
	jobs := fleet.NewReader(fleet.DefaultJobsDir()).ReadJobs(jobIDs)
	children := fleet.Attribute(rows, live, jobs, fleet.NewOwnerResolver(), now)
	// One-shot process: no history, so consoles can only be read off the titles
	// visible right now. A console whose view happens to be showing an agent's name
	// at this instant is therefore missed here — acceptable in a fallback that only
	// runs when no daemon is up, and the reason the daemon keeps a sticky set.
	consoles := map[int64]bool{}
	for _, w := range windows {
		if fleet.IsConsoleTitle(w.Title) {
			consoles[int64(w.ID)] = true
		}
	}
	return FleetViewFrom(children, fleet.Rollups(children), consoles)
}

// fleetItem builds a fleet row: a valid session row carrying a FleetInfo.
//
// The State it reports is what the whole tile reacts to, so it is chosen to be
// honest about who is waiting on whom:
//
//   - any agent awaiting a human -> Prompt, which is what makes the tile pulse. This
//     is the case the tile used to miss entirely.
//   - else any agent working -> Working.
//   - else any agent blocked -> Shell. Shell is the "parked, passively monitoring"
//     indicator, which is exactly what a fleet blocked on ITSELF is; calling it
//     Working would overstate it and Idle would hide it.
//   - else Idle, aged on the fleet's newest activity rather than on the hosting
//     session's last turn. That distinction is the entire point: a console desktop
//     whose session last spoke a day ago is not a day idle if its agents moved a
//     second ago.
func fleetItem(sum fleet.Summary, scope, folder string, winID int64, now time.Time) Item {
	it := Item{
		Kind:   KindSession,
		Folder: folder,
		WinID:  winID,
		Title:  fleetTitle(sum),
		Fleet: &FleetInfo{
			Total: sum.Total, Live: sum.Live, Awaiting: sum.Awaiting,
			Working: sum.Working, Blocked: sum.Blocked, Idle: sum.Idle, Done: sum.Done,
			Demands: demandStrings(sum.Demands), Neediest: sum.Neediest,
			Needs: sum.Needs, Scope: scope,
		},
	}
	switch {
	case sum.Awaiting > 0:
		it.State = string(state.Prompt)
	case sum.Working > 0:
		it.State = string(state.Working)
	case sum.Blocked > 0:
		it.State = string(state.Shell)
	default:
		it.State = string(state.Idle)
		if sum.NewestActivity > 0 {
			since := now.Sub(time.UnixMilli(sum.NewestActivity))
			it.IdleLevel = state.DecayLevel(since)
			it.IdleAgo = fmtAgo(since)
		} else {
			it.IdleLevel = state.DecayLevels - 1
		}
	}
	return it
}

// fleetTitle is the row's human line. It carries the counts and the ask in text as
// well as in FleetInfo, so a template that knows nothing about fleets still says
// something true instead of describing a resting session.
func fleetTitle(sum fleet.Summary) string {
	head := fmt.Sprintf("%d agents", sum.Total)
	if sum.Live != sum.Total {
		head = fmt.Sprintf("%d/%d agents live", sum.Live, sum.Total)
	}
	switch {
	case sum.Needs != "" && sum.Neediest != "":
		return fmt.Sprintf("%s · %d awaiting · %s: %s", head, sum.Awaiting, sum.Neediest, sum.Needs)
	case sum.Awaiting > 0:
		return fmt.Sprintf("%s · %d awaiting you", head, sum.Awaiting)
	case sum.Working > 0:
		return fmt.Sprintf("%s · %d working", head, sum.Working)
	case sum.Blocked > 0:
		return fmt.Sprintf("%s · %d blocked", head, sum.Blocked)
	}
	return head
}

// demandStrings renders the bead strip's demands for the payload.
func demandStrings(ds []fleet.Demand) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.String())
	}
	return out
}

// sessionItem builds one session row from a DB session and its window title.
func sessionItem(s db.Session, title string, now time.Time) Item {
	it := Item{Kind: KindSession, State: s.State, Title: title}
	if s.Cwd.Valid {
		it.Folder = filepath.Base(s.Cwd.String)
	}
	if state.Status(s.State) == state.Idle && s.LastTalkTS.Valid {
		elapsed := now.Sub(time.UnixMilli(s.LastTalkTS.Int64))
		it.IdleLevel = state.DecayLevel(elapsed)
		it.IdleAgo = fmtAgo(elapsed)
	}
	return it
}

// appItem builds one app row from an ordinary niri window.
func appItem(w niri.Window) Item {
	return Item{
		Kind:    KindApp,
		App:     cleanAppLabel(w.AppID),
		AppIcon: resolveAppIcon(w.AppID),
		Title:   w.Title,
	}
}

// PayloadFor computes the tile payload for one workspace from already-gathered
// state: the workspace, the windows currently on it, sessions indexed by their
// cached window id, and the clock. PURE (no IPC/DB) so both the daemon and the
// CLI fallback share it and it is unit-testable.
//
// An empty desktop -> emptyPayload. Otherwise every window on the desktop becomes
// a row: a session row per tracked session, an app row per window that hosts
// none — so a MIXED desktop (a session plus an ordinary window) shows both, which
// the old `sessions`-only feed could not express. Rows are prompt-first ordered
// and capped at maxItemsPerTile. Co-resident sessions that share one niri window
// (two kitty tabs) BOTH appear — they are grouped by workspace, not window, so
// neither is masked. Note: the daemon passes its first-party overlaid sessions, so
// each State is the effective state, not raw hooks.
func PayloadFor(ws niri.Workspace, winsOnWs []niri.Window, sessionsOnWs []db.Session, fv FleetView, now time.Time) Payload {
	rows := desktopRows(winsOnWs, sessionsOnWs, fv, now)
	if len(rows) == 0 {
		return emptyPayload(ws.Idx, ws.IsFocused)
	}
	return Payload{Shortcut: ws.Idx, Active: ws.IsFocused, Items: rows}
}

// row is an Item plus the stable identity used to break ordering ties: the niri
// window it came from, and the session id for session rows (an app row leaves it
// empty). Keeping them beside the Item avoids re-deriving them while sorting.
type row struct {
	item      Item
	winID     int64
	sessionID string
	// isFleet keeps a fleet row ahead of an ordinary session row of the same state
	// when the cap has to choose. A desktop with agents waiting on you must not
	// drop that row in favour of the resting conversation beside it.
	isFleet bool
}

// desktopRows turns a desktop's windows and sessions into the tile's rows, in the
// order the cap should keep, capped at maxItemsPerTile. Ordering is fully
// deterministic — the daemon groups windows and sessions out of maps (randomized
// iteration), so without the tie-breaks the chosen/ordered rows would flip on
// every cache rebuild and the tile would visibly blink between them.
func desktopRows(winsOnWs []niri.Window, sessionsOnWs []db.Session, fv FleetView, now time.Time) []Item {
	// Title comes from the session's niri window (sessions carry no title of their
	// own); co-window sessions share it.
	titleOf := make(map[int]string, len(winsOnWs))
	for _, w := range winsOnWs {
		titleOf[w.ID] = w.Title
	}
	// Does this desktop host an agent view? Decided before building any row, because
	// a console row makes every owned fleet row on the same desktop redundant.
	hasConsole := false
	if !fv.Whole.Empty() {
		for _, w := range winsOnWs {
			if fv.Consoles[int64(w.ID)] {
				hasConsole = true
				break
			}
		}
	}
	hosted := make(map[int]bool, len(sessionsOnWs))
	rows := make([]row, 0, len(sessionsOnWs)+len(winsOnWs))
	for _, s := range sessionsOnWs {
		var wid int64
		title := ""
		if s.WindowID.Valid {
			wid = s.WindowID.Int64
			title = titleOf[int(wid)]
			hosted[int(wid)] = true
		}
		folder := ""
		if s.Cwd.Valid {
			folder = filepath.Base(s.Cwd.String)
		}
		// A CONSOLE window is running the agent view, not a conversation, so the
		// fleet REPLACES its session row rather than sitting beside it. Describing
		// that window by its host session's last turn is precisely the lie this
		// fixes: the session may be a day idle while the dashboard in front of you
		// shows eleven agents moving.
		if wid != 0 && fv.Consoles[wid] && !fv.Whole.Empty() {
			rows = append(rows, row{
				item:      fleetItem(fv.Whole, ScopeWhole, "agents", wid, now),
				winID:     wid,
				sessionID: s.SessionID,
				isFleet:   true,
			})
			continue
		}
		rows = append(rows, row{item: sessionItem(s, title, now), winID: wid, sessionID: s.SessionID})
		// An OWNER desktop keeps its conversation row and gains a second row for the
		// agents it spawned — the two-row layout finally earning its keep.
		//
		// Unless a console row is already on this desktop: the agent view lists
		// EVERY agent, so an owned set is a subset of what that row already shows.
		// Emitting both spent the tile's two rows on the same agents counted twice
		// and pushed the actual conversations off the tile (seen live: one desktop
		// showing "7 agents owned" beside "13 agents whole", with both sessions
		// dropped).
		if hasConsole {
			continue
		}
		if sum, ok := fv.ByOwner[s.SessionID]; ok && !sum.Empty() {
			rows = append(rows, row{
				item:      fleetItem(sum, ScopeOwned, folder, wid, now),
				winID:     wid,
				sessionID: s.SessionID,
				isFleet:   true,
			})
		}
	}
	// Windows with no tracked session are ordinary apps. A window hosting one is
	// already represented by its session row(s) — an app row for it would be a
	// duplicate of the same window.
	for _, w := range winsOnWs {
		if hosted[w.ID] {
			continue
		}
		rows = append(rows, row{item: appItem(w), winID: int64(w.ID)})
	}
	sort.Slice(rows, func(i, j int) bool {
		pi, pj := rowPriority(rows[i].item), rowPriority(rows[j].item)
		if pi != pj {
			return pi < pj
		}
		if rows[i].isFleet != rows[j].isFleet {
			return rows[i].isFleet // a fleet row survives the cap first
		}
		if rows[i].winID != rows[j].winID {
			return rows[i].winID < rows[j].winID // leftmost/oldest window first
		}
		return rows[i].sessionID < rows[j].sessionID
	})
	if len(rows) > maxItemsPerTile {
		rows = rows[:maxItemsPerTile]
	}
	// The winID the sort just used as a tie-break is also what a click on this row
	// should focus, so publish it rather than making tile-toggle re-derive it.
	items := make([]Item, 0, len(rows))
	for _, r := range rows {
		it := r.item
		it.WinID = r.winID
		items = append(items, it)
	}
	return items
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
func BuildAll(workspaces map[int]niri.Workspace, windows []niri.Window, sessions []db.Session, fv FleetView, now time.Time) map[string]Payload {
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
		out[Key(ws.Output, ws.Idx)] = PayloadFor(ws, winsByWs[ws.ID], sessByWs[ws.ID], fv, now)
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
		// the daemon didn't see) -> empty placeholder. A rowless payload means the
		// same, or a cache still in the pre-items shape written by an older daemon
		// mid-upgrade — either way the placeholder beats emitting an items-less
		// object, which the contract rejects.
		if cached, ok := tiles[Key(output, idx)]; ok && len(cached.Items) > 0 {
			p = cached
		} else {
			p = emptyPayload(idx, cached.Active)
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
			// Same rowless guard as Run: never stream an items-less object.
			if cached, ok := tiles[key]; ok && len(cached.Items) > 0 {
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
	now := db.Now()
	return PayloadFor(*ws, onWs, sessOnWs, LiveFleetView(sessions, onWs, now), now)
}

// fmtAgo renders an elapsed duration as the tile's idle_ago: minutes under an
// hour ("12m"), hours under a day ("2h"), then days ("3d"), clamped at "99d".
//
// The shape is load-bearing — it dodges two filed renderer bugs that the producer
// can avoid entirely:
//   - It MUST be <integer><suffix>. Anything else ("1.5h", "1h30m", "2 hours") is
//     silently parsed as a couple of minutes and the label renders bright, i.e.
//     "just idled" — the exact opposite of the truth (pwetty-box-rs-00i).
//   - It must stay ~3 characters or it overruns its slot and paints over the
//     folder name; "120m" already collides. Hence the rollover to hours then days
//     rather than ever-growing minutes (pwetty-box-rs-9l7).
func fmtAgo(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch m := int(d.Minutes()); {
	case m < 60:
		return fmt.Sprintf("%dm", m)
	case m < 24*60:
		return fmt.Sprintf("%dh", m/60)
	default:
		if days := m / (24 * 60); days <= 99 {
			return fmt.Sprintf("%dd", days)
		}
		return "99d"
	}
}
