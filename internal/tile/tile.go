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
	"unicode"
	"unicode/utf8"

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
	// Unknown counts agents present but unclassifiable (herdr's `unknown`, which is
	// explicitly NOT "finished"). omitempty, so a claude-code fleet's payload is
	// byte-identical to before — that path has no such rung.
	Unknown int `json:"unknown,omitempty"`
	Done    int `json:"done,omitempty"`
	// Demands is one bead per agent: "awaits-human" | "working" | "blocked" |
	// "idle" | "unknown" | "done". Worst first — the user's choice over
	// stable-by-agent, so the alarm bead is always leftmost and a bead's POSITION
	// does not encode which agent it is.
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
	// ScopeHerd: one herdr WORKSPACE's agents. Neither of the other two fits — the
	// agents were not spawned by the session on this desktop (they are its
	// SIBLINGS, panes of the same multiplexer) and the set is not machine-wide.
	ScopeHerd = "herd"
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

// emptyPayload is the placeholder for an absent/empty desktop: the shortcut
// over a dim hollow "empty" ring (pwetty's `draw_status` state="empty"), as a
// single session row so it satisfies the data-driven `items` contract — a
// windowless desktop is NOT a 0-row items array, which the contract forbids.
// Distinct from a real `idle` session: a desktop with nothing open never shows
// the idle decay bar. (Selecting the bundled `empty` preset per desktop instead
// is claude-status-db-wsh.)
func emptyPayload(idx int, active bool) Payload {
	return Payload{
		Shortcut: idx,
		Active:   active,
		Items:    []Item{{Kind: KindSession, State: "empty"}},
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
	// Consoles is the set of niri window ids running the agent view — CANDIDATES:
	// a marker can outlive the session that set it, which desktopRows filters.
	Consoles map[int64]bool
	// Herds is the herdr provider's groups, one per herdr workspace, keyed by
	// workspace id. A window is matched to one of these through its sessions'
	// herdr_workspace_id column, never by its title: a herdr host window's title is
	// herdr's own, shared by every pane behind it, and the title premise is exactly
	// what console.go just had to be gated on.
	//
	// Empty on the no-daemon fallback path, on purpose — see LiveFleetView.
	Herds map[string]fleet.Group
	// HerdWindows binds a herd's workspace id to the niri window hosting it, and it
	// is REMEMBERED by the daemon rather than derived from the rows on hand.
	//
	// Without it a herd row exists only while one of its panes has a live session
	// row carrying the workspace id — and a herdr pane's sessions are as short as
	// whatever drives them (median 81s here). Measured after deploying: the row
	// appeared when a fresh pane's session started and vanished the moment it ended,
	// so the herd blinked in and out every couple of minutes. That is the same
	// display churn this whole mechanism exists to remove, arriving by a new route.
	HerdWindows map[string]int64
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
//
// Stickiness means the set also holds windows whose console DIED, so membership here
// is a candidate, not a verdict: desktopRows gates each marker on its host session
// still being alive. Both this path and LiveFleetView below go through it, so the
// gate covers the daemon and the no-daemon fallback alike.
func FleetViewFrom(children map[string]fleet.Child, rollups map[string]fleet.Rollup, consoles map[int64]bool, herds []fleet.Group, herdWindows map[string]int64) FleetView {
	fv := FleetView{
		ByOwner:     make(map[string]fleet.Summary, len(rollups)),
		Whole:       fleet.WholeFleet(children),
		Consoles:    consoles,
		Herds:       fleet.GroupsByKey(herds, fleet.ProviderHerdr),
		HerdWindows: herdWindows,
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
	// NO HERD ON THIS PATH, deliberately. Reading herdr's roster costs a process
	// spawn, and this function runs inside `tile-data`, which waybar re-runs ONCE
	// PER TILE PER INTERVAL — a herdr spawn here would be several a second for data
	// that changes at human speed. The daemon is the only caller that may pay it
	// (see the cost note in package herdr), so with no daemon up a herdr host window
	// degrades to the session rows it shows today.
	// No daemon, so no remembered bindings: bind from the rows on hand. A herd whose
	// panes have no live session right now is therefore invisible on this path —
	// acceptable in a fallback that only runs when no daemon is up, and the reason
	// the daemon keeps the map instead of recomputing it.
	herdWins := map[string]int64{}
	for _, r := range rows {
		if r.HerdrWorkspaceID.Valid && r.HerdrWorkspaceID.String != "" && r.WindowID.Valid {
			herdWins[r.HerdrWorkspaceID.String] = r.WindowID.Int64
		}
	}
	return FleetViewFrom(children, fleet.Rollups(children), consoles, nil, herdWins)
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
//
// herdLabel is what a herd row puts where a session row puts its folder: the
// project AND how many agents are in it.
//
// NEITHER HALF SURVIVES ALONE. The full label loses that contest on width — at the
// real 179px "project-manager" measures about 141px on its own, and that line also
// carries the big shortcut digit and the mascot, so it clipped to "project-…", a stub
// every workspace cut from one checkout shares. But a bare count ("6 agents") is no
// better: it says nothing about WHICH herd, and in the two-row layout there is no
// third line to say it either.
//
// So the project is abbreviated rather than dropped. A multi-word label goes to its
// initials — "project-manager" to "PM" — which is short enough to leave the count
// room and, unlike a head-clip, stays distinct between two projects that begin alike.
// The full spelling is still on the title line whenever the layout has one.
func herdLabel(label string, total int) string {
	suffix := fmt.Sprintf(" ×%d", total)
	// What the label line leaves beside the count and the mascot, measured at 179px.
	const budget = 7
	name := strings.TrimSpace(label)
	switch {
	case name == "":
		return "herd" + suffix
	case len(name) <= budget:
	default:
		if in := initials(name); in != "" {
			name = in
		} else {
			name = name[:budget-1] + "…"
		}
	}
	return name + suffix
}

// initials abbreviates a multi-word label by its word heads, uppercased. Returns
// empty for a single word, which has no initials worth the loss — a one-word label is
// better clipped than reduced to one letter.
func initials(label string) string {
	parts := strings.FieldsFunc(label, func(r rune) bool {
		return r == '-' || r == '_' || r == '.' || r == ' ' || r == '/'
	})
	if len(parts) < 2 {
		return ""
	}
	if len(parts) > 4 {
		parts = parts[:4] // four letters is already at the budget
	}
	var b strings.Builder
	for _, p := range parts {
		r, _ := utf8.DecodeRuneInString(p)
		b.WriteRune(unicode.ToUpper(r))
	}
	return b.String()
}

func fleetItem(sum fleet.Summary, scope, folder string, winID int64, now time.Time) Item {
	label, title := folder, fleetTitle(sum)
	if scope == ScopeHerd {
		label = herdLabel(folder, sum.Total)
		title = herdTitle(folder, sum)
	}
	it := Item{
		Kind:   KindSession,
		Folder: label,
		WinID:  winID,
		Title:  title,
		Fleet: &FleetInfo{
			Total: sum.Total, Live: sum.Live, Awaiting: sum.Awaiting,
			Working: sum.Working, Blocked: sum.Blocked, Idle: sum.Idle,
			Unknown: sum.Unknown, Done: sum.Done,
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
	// NAME THE AGENT even when there is no ask to quote. herdr has no equivalent of
	// a background job's `needs` — nothing here invents one — but it does know WHICH
	// pane is blocked, and "project-manager-7 wants you" is the whole difference
	// between a herd row and the anonymous "something wants you" it replaces.
	case sum.Awaiting > 0 && sum.Neediest != "":
		return fmt.Sprintf("%s · %d awaiting · %s", head, sum.Awaiting, sum.Neediest)
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
	item  Item
	winID int64
	// sessionID is the row's stable tie-break string. For a session row it is the
	// session id; for a HERD row it is the group key ("w1") instead, deliberately —
	// a herdr host window holds several workspaces, so two herd rows can share one
	// winID, and letting a session uuid break that tie would reinstate the exact
	// row lottery this change exists to remove.
	sessionID string
	// summary is the group behind a herd row, kept so herds sharing a window can be
	// merged without re-deriving it from the rendered Item.
	summary fleet.Summary
	// label is a herd row's workspace label — the repository. It is kept apart
	// from item.Folder because a herd row shows its agent COUNT there, so merging
	// several herds must read the repository from here or it would fold counts.
	label string
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
	// Which console markers on this desktop are still WORTH BELIEVING? The marker
	// is sticky per window (see the daemon's noteConsoles) and the agent view sets
	// its terminal title but never unsets it, so both outlive the session that
	// earned them. Measured 2026-08-25: windows 12 and 34 were still titled "claude
	// agents" with their host sessions silent for 188h and 187h — eight-day fossils
	// of a pool that no longer exists. They failed to claim a console row only
	// because the whole-fleet set happened to be empty that day; one background
	// agent anywhere on the machine and a fossil would have taken the row AND
	// suppressed every other fleet row on its desktop (see below), and dropped the
	// session rows of the co-resident conversations with it.
	//
	// A marker is therefore believed only while the window still hosts a live
	// session: iterating the desktop's sessions means a window whose Claude has
	// exited (kitty keeps the stale title) drops out for free, and
	// fleet.ConsoleHostLive judges the ones that remain. This is orthogonal to the
	// stickiness it does not touch — a live console that retitles as you scroll
	// keeps its marker and its row exactly as before.
	consoles := make(map[int64]bool, len(fv.Consoles))
	for _, s := range sessionsOnWs {
		if !s.WindowID.Valid || !fv.Consoles[s.WindowID.Int64] {
			continue
		}
		var lastTalk time.Time
		if s.LastTalkTS.Valid {
			lastTalk = time.UnixMilli(s.LastTalkTS.Int64)
		}
		if fleet.ConsoleHostLive(lastTalk, now) {
			consoles[s.WindowID.Int64] = true
		}
	}
	// Which window on this desktop is THE agent view? Decided before building any
	// row, for two reasons: a console row makes every owned fleet row on the same
	// desktop redundant, and a desktop can hold SEVERAL console windows — whose
	// whole-scope summaries are machine-wide and therefore byte-identical, so
	// emitting one per window fills the tile's two-row budget with the same
	// dashboard twice (seen live) and pushes everything else off. At most one wins,
	// and it is the lowest window id so the choice cannot flip between rebuilds.
	consoleWin := int64(0)
	if !fv.Whole.Empty() {
		for _, w := range winsOnWs {
			if consoles[int64(w.ID)] && (consoleWin == 0 || int64(w.ID) < consoleWin) {
				consoleWin = int64(w.ID)
			}
		}
	}
	hasConsole := consoleWin != 0
	// HERDR HOST WINDOWS. herdr is a multiplexer: N agents share ONE niri window,
	// one terminal pid and (behind splits) one title, so before this the desktop
	// contributed N indistinguishable session rows and the two-row cap resolved them
	// by winID and then LEXICOGRAPHICALLY BY SESSION UUID — which agent you saw was
	// random and reshuffled whenever a tick recycled. Measured live on desktop 3:
	// two rows out of five candidates, three of four herdr agents simply absent, and
	// the two survivors byte-identical (herdr's shared window title, and a folder
	// that was a worktree basename).
	//
	// A WINDOW IS A HERDR HOST IFF THE SESSIONS RESOLVING TO IT CARRY A
	// herdr_workspace_id. No title sniffing and no process-tree walking: the title
	// is herdr's own, fanned out across every pane, and the fragile title premise is
	// precisely what console.go just had to be gated on. The column is written by
	// the hook from the pane's environment, so it is first-hand.
	//
	// Grouped by WORKSPACE and not by window, because one herdr window holds several
	// workspaces — w1, w9 and wA were all live behind niri window 33 — and the
	// workspace is the level bound to a git checkout, which is what a human means by
	// "that herd".
	herds := map[string]row{}
	winOnDesktop := make(map[int64]bool, len(winsOnWs))
	for _, w := range winsOnWs {
		winOnDesktop[int64(w.ID)] = true
	}
	hosted := make(map[int]bool, len(sessionsOnWs))
	rows := make([]row, 0, len(sessionsOnWs)+len(winsOnWs))
	// An OWNER desktop keeps its own row and GAINS one for the agents it spawned —
	// the two-row layout finally earning its keep. A closure because a herdr pane is
	// an ordinary interactive session and may perfectly well have spawned background
	// agents of its own: its session row is replaced by the herd row, but an ask from
	// a background fleet it owns must not disappear with it. Unobserved in practice,
	// and not worth being wrong about — a hidden attention signal is the one failure
	// this whole epic is about.
	addOwned := func(s db.Session, wid int64, folder string) {
		// Unless a console row is already on this desktop: the agent view lists EVERY
		// agent, so an owned set is a subset of what that row already shows. Emitting
		// both spent the tile's two rows on the same agents counted twice and pushed
		// the actual conversations off the tile (seen live: one desktop showing "7
		// agents owned" beside "13 agents whole", with both sessions dropped).
		if hasConsole {
			return
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
		// A LOSING console window gets no row at all. Falling back to its session
		// row would put the "idle 22h" reading this epic exists to remove right back
		// on the tile, and the winning row already describes the same dashboard.
		if wid != 0 && wid != consoleWin && consoles[wid] && hasConsole {
			continue
		}
		if wid != 0 && wid == consoleWin {
			rows = append(rows, row{
				item:      fleetItem(fv.Whole, ScopeWhole, "agents", wid, now),
				winID:     wid,
				sessionID: s.SessionID,
				isFleet:   true,
			})
			continue
		}
		// One pane of a herd gets NO row of its own — the herd gets one, and the
		// pane's siblings collapse into it. The row is built once, from the group,
		// so it says the same thing however many panes resolve here and whichever of
		// them the daemon happened to iterate first. That is what makes it survive a
		// tick recycling underneath: nothing about this row is derived from a member.
		//
		// If there is no group for the workspace — no daemon, herdr asleep, or a
		// workspace herdr no longer reports — the session falls through and the
		// desktop renders exactly as it does today. The column alone is not enough;
		// a herd has to actually be there.
		if key, g, ok := herdOf(s, fv.Herds); ok {
			h, seen := herds[key]
			if !seen {
				sum := g.Summarize()
				h = row{item: fleetItem(sum, ScopeHerd, g.Label, wid, now), label: g.Label,
					winID: wid, sessionID: key, summary: sum, isFleet: true}
			} else if wid != 0 && (h.winID == 0 || wid < h.winID) {
				// Lowest window id wins, so a click lands somewhere stable when the
				// herd's panes are spread over more than one window.
				h.winID = wid
			}
			herds[key] = h
			addOwned(s, wid, folder)
			continue
		}
		rows = append(rows, row{item: sessionItem(s, title, now), winID: wid, sessionID: s.SessionID})
		addOwned(s, wid, folder)
	}
	// A herd bound to a window ON THIS DESKTOP gets its row even when none of its
	// panes currently has a session row — see FleetView.HerdWindows. The group is
	// the whole truth about the herd; a member row was only ever how we FOUND it.
	for ws, win := range fv.HerdWindows {
		if _, already := herds[ws]; already {
			continue
		}
		if !winOnDesktop[win] {
			continue
		}
		g, ok := fv.Herds[ws]
		if !ok || len(g.Members) == 0 {
			continue
		}
		hosted[int(win)] = true
		sum := g.Summarize()
		herds[ws] = row{item: fleetItem(sum, ScopeHerd, g.Label, win, now), label: g.Label,
			winID: win, sessionID: ws, summary: sum, isFleet: true}
	}
	// Herds sharing ONE WINDOW collapse into a single row. A herdr window hosts many
	// workspaces (five here) and most hold exactly one agent, so a row per workspace
	// spent the whole tile on single-bead rows whose labels all clipped to the same
	// string — rendered at the real width, "project-manager-8" and
	// "project-manager-9" both read "project-…" — while the 2-row cap silently
	// dropped the rest. One row carrying every agent's bead says more in less space.
	// A window with a single herd keeps its own label and is untouched.
	for _, h := range mergeHerdsPerWindow(herds, now) {
		rows = append(rows, h)
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

// herdOf reports the herd a session belongs to, if any: its herdr_workspace_id, and
// the group the daemon built for that workspace.
//
// THE WORKSPACE ID IS THE ONLY THING ASKED FOR. Not the pane id, which churns under
// a live agent (w1:pCD -> w1:pCH -> w1:pCJ observed for one agent across ~20
// minutes), and not the window title, which is herdr's and is shared by every pane
// behind it.
//
// An empty group is treated as no herd: it would render a row saying "0 agents",
// which is strictly worse than the session row it replaced.
func herdOf(s db.Session, herds map[string]fleet.Group) (string, fleet.Group, bool) {
	if len(herds) == 0 || !s.HerdrWorkspaceID.Valid || s.HerdrWorkspaceID.String == "" {
		return "", fleet.Group{}, false
	}
	g, ok := herds[s.HerdrWorkspaceID.String]
	if !ok || len(g.Members) == 0 {
		return "", fleet.Group{}, false
	}
	return s.HerdrWorkspaceID.String, g, true
}

// mergeHerdsPerWindow folds the herds of one window into one row each.
func mergeHerdsPerWindow(herds map[string]row, now time.Time) []row {
	byWin := map[int64][]string{}
	for key, h := range herds {
		byWin[h.winID] = append(byWin[h.winID], key)
	}
	out := make([]row, 0, len(byWin))
	for win, keys := range byWin {
		if len(keys) == 1 {
			out = append(out, herds[keys[0]])
			continue
		}
		sort.Strings(keys) // deterministic label and identity
		sums := make([]fleet.Summary, 0, len(keys))
		labels := make([]string, 0, len(keys))
		for _, k := range keys {
			h := herds[k]
			if h.item.Fleet == nil {
				continue
			}
			sums = append(sums, h.summary)
			labels = append(labels, h.label)
		}
		merged := fleet.MergeSummaries(sums)
		shared := sharedLabel(labels)
		out = append(out, row{
			item:  fleetItem(merged, ScopeHerd, shared, win, now),
			label: shared,
			winID: win,
			// Identity is the window's herds, so the row is stable while they are.
			sessionID: strings.Join(keys, "+"),
			isFleet:   true,
		})
	}
	return out
}

// sharedLabel names a set of herds with what they have in common, which for herdr
// workspaces cut from one checkout is the repository: {project-manager,
// project-manager-6, project-manager-9} -> "project-manager". Falls back to a count
// when they share nothing, because "3 herds" is honest where a truncated common
// prefix would be a different repo's name. Returns EMPTY when they share nothing:
// the caller composes the label, and "3 herds" there would have collided with the
// agent count beside it ("3 herds ×3", which invites reading three as both).
func sharedLabel(labels []string) string {
	if len(labels) == 0 {
		return ""
	}
	prefix := labels[0]
	for _, l := range labels[1:] {
		for !strings.HasPrefix(l, prefix) {
			prefix = prefix[:len(prefix)-1]
			if prefix == "" {
				break
			}
		}
	}
	prefix = strings.TrimRight(prefix, "-_. /")
	if len(prefix) < 3 {
		return ""
	}
	return prefix
}

// herdTitle names the repository and, when something is waiting on a person, the
// agent that is — the two things the label line has no room for.
//
// ORDER IS THE POINT, because this line is clipped to a bit over thirty characters
// and the clip eats the tail. When a human is being waited on, the ask leads and the
// repository is what gets sacrificed; when nothing is waiting, the repository leads,
// since it is then the only thing on the tile that says WHICH herd this is.
func herdTitle(label string, sum fleet.Summary) string {
	// NOTHING IS SAID TWICE. How many are working is already on the tile twice over —
	// the mascot's colour and the lit beads — and spelling it out here was enough to
	// wrap the line onto a fourth row that the 56px bar cuts off. So this line speaks
	// only when a person is being waited on, which no bead can say in words.
	ask := ""
	switch {
	case sum.Needs != "" && sum.Neediest != "":
		ask = sum.Neediest + ": " + sum.Needs
	case sum.Neediest != "" && sum.Awaiting > 0:
		ask = sum.Neediest + " needs you"
	case sum.Awaiting > 0:
		ask = fmt.Sprintf("%d awaiting you", sum.Awaiting)
	}
	parts := []string{}
	if ask != "" {
		parts = append(parts, ask, label) // the ask first: it is why you would look
	} else {
		parts = append(parts, label)
	}
	kept := parts[:0]
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, " · ")
}
