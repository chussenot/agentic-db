package daemon

import (
	"sort"
	"time"

	"github.com/mrzor/claude-status/internal/clauded"
	"github.com/mrzor/claude-status/internal/db"
	"github.com/mrzor/claude-status/internal/state"
)

// firstPartyState maps Claude Code's first-party status to our activity state.
// ok is false for an unrecognized OR deliberately-deferred value, in which case
// the caller keeps the hook-derived state. Mapping (see package clauded): busy =
// taking a turn -> working; idle = finished turn at rest -> idle (decays); shell
// = background-monitor -> shell.
//
// `waiting` is DELIBERATELY NOT mapped to Prompt. It looked like the genuine
// "?" signal, but observation proved it overloaded: it fires whenever the main
// loop is suspended on ANYTHING — a subagent/tool wait as much as a real
// user-prompt — so mapping it to "?" produced false positives (e.g. a `/btw`
// turn sat at `waiting` for minutes with no question; see memory
// first-party-status-waiting-is-not-a-reliable). There is no finer first-party
// field to disambiguate. So we defer `waiting` to the hook state: the genuine
// "?" comes from the permission-Notification hook path (precise), plus prose-
// question detection on Stop (see the transcript-parse work). Returning ok=false
// here means a `waiting` session keeps whatever the hooks last derived.
func firstPartyState(s clauded.Status) (state.Status, bool) {
	switch s {
	case clauded.Busy:
		return state.Working, true
	case clauded.Idle:
		return state.Idle, true
	case clauded.Shell:
		return state.Shell, true
	default: // waiting (deferred to hooks) and unrecognized values
		return "", false
	}
}

// overlayFirstParty refines each session's State with Claude Code's first-party
// status when available, in place. First-party status is authoritative for the
// activity state because it updates on Claude's own cadence, independent of our
// hooks: it fixes stale hook state (e.g. a missed Stop leaving a session stuck
// "working", or the idle-nudge "?" defect) and makes the "?"/decay decision
// language-independent rather than reliant on Notification text matching.
//
// Sessions absent from fp (remote/ssh/tmux, or no file yet) keep their
// hook-derived State — so the (now-fixed) notification path remains the fallback.
// Unrecognized first-party status values are left to the hook state as well.
// Only State is overridden; window_id, last_talk_ts (decay level), etc. are
// untouched, so the decay bar still reads its timing from the DB row.
func overlayFirstParty(sessions []db.Session, fp map[string]clauded.Session) {
	for i := range sessions {
		f, ok := fp[sessions[i].SessionID]
		if !ok {
			continue
		}
		if st, ok := firstPartyState(f.Status); ok {
			sessions[i].State = string(st)
		}
	}
}

// workspaceResolver maps a niri window id to the workspace it currently sits on.
// The live *niri.Model satisfies this; tests inject a map-backed fake so the
// aggregation logic can be exercised without a compositor.
type workspaceResolver interface {
	WindowWorkspace(windowID int) (workspaceID int, ok bool)
}

// desired is the target visual state for one workspace: a status plus (for idle)
// a decay level. It encodes to a workspace name via state.Encode.
type desired struct {
	status state.Status
	level  int // meaningful only when status == state.Idle
}

// aggregate computes the desired per-workspace state from the live session set.
// It is the pure heart of the reconciler — no IPC, no clock except the injected
// now — so it is unit-tested directly.
//
// For each session it resolves window_id -> workspace_id via resolve, skipping
// sessions with a NULL window_id or a window the model doesn't know (remote, or
// closed). Sessions are then aggregated per workspace with the precedence from
// the design doc:
//
//	any working  -> working
//	else any prompt -> prompt
//	else any shell -> shell (background-monitor pulse)
//	else idle, level = DecayLevel(now - max(last_talk_ts over idle sessions))
//
// "most-recent talk wins" means the brightest (lowest) level: we take the
// MAXIMUM last_talk_ts among the workspace's idle sessions, i.e. the smallest
// elapsed. A workspace with no live Claude sessions simply does not appear in
// the result (its name should be unset).
func aggregate(sessions []db.Session, resolve workspaceResolver, now time.Time) map[int]desired {
	type acc struct {
		hasWorking bool
		hasPrompt  bool
		hasShell   bool
		hasIdle    bool
		// maxIdleTalk is the most recent last_talk_ts (unix ms) across idle
		// sessions on this workspace; 0 if none had one.
		maxIdleTalk int64
		idleTalkSet bool
	}
	accs := make(map[int]*acc)

	for _, s := range sessions {
		if !s.WindowID.Valid {
			continue // remote/unresolved session, no workspace
		}
		wsID, ok := resolve.WindowWorkspace(int(s.WindowID.Int64))
		if !ok {
			continue // window not in the live model
		}
		a := accs[wsID]
		if a == nil {
			a = &acc{}
			accs[wsID] = a
		}
		switch state.Status(s.State) {
		case state.Working:
			a.hasWorking = true
		case state.Prompt:
			a.hasPrompt = true
		case state.Shell:
			a.hasShell = true
		default: // idle (and any unexpected value treated as idle, per precedence floor)
			a.hasIdle = true
			if s.LastTalkTS.Valid {
				if !a.idleTalkSet || s.LastTalkTS.Int64 > a.maxIdleTalk {
					a.maxIdleTalk = s.LastTalkTS.Int64
					a.idleTalkSet = true
				}
			}
		}
	}

	out := make(map[int]desired, len(accs))
	nowMS := now.UnixMilli()
	for wsID, a := range accs {
		switch {
		case a.hasWorking:
			out[wsID] = desired{status: state.Working}
		case a.hasPrompt:
			out[wsID] = desired{status: state.Prompt}
		case a.hasShell:
			out[wsID] = desired{status: state.Shell}
		default:
			level := state.DecayLevels - 1 // no talk timestamp -> dimmest
			if a.idleTalkSet {
				elapsed := time.Duration(nowMS-a.maxIdleTalk) * time.Millisecond
				level = state.DecayLevel(elapsed)
			}
			out[wsID] = desired{status: state.Idle, level: level}
		}
	}
	return out
}

// slotAllocator hands out globally-unique slot numbers 1..MaxSlots and tracks
// which workspace owns which slot. It ports niri-topic-namer's assign_slot /
// free_slot / used_slots, plus an adopt path for reclaiming a slot from a
// pre-existing workspace name on startup.
type slotAllocator struct {
	byWorkspace map[int]int  // workspace id -> slot
	used        map[int]bool // slot -> in use
}

func newSlotAllocator() *slotAllocator {
	return &slotAllocator{
		byWorkspace: make(map[int]int),
		used:        make(map[int]bool),
	}
}

// assign returns the slot for wsID, allocating the lowest free slot on first
// request. ok is false when all MaxSlots slots are taken (more than MaxSlots
// Claude workspaces — no dot until one frees, same as the Python).
func (sa *slotAllocator) assign(wsID int) (slot int, ok bool) {
	if s, exists := sa.byWorkspace[wsID]; exists {
		return s, true
	}
	for s := 1; s <= state.MaxSlots; s++ {
		if !sa.used[s] {
			sa.used[s] = true
			sa.byWorkspace[wsID] = s
			return s, true
		}
	}
	return 0, false
}

// free releases the slot held by wsID (if any), so it can be reused.
func (sa *slotAllocator) free(wsID int) {
	if s, ok := sa.byWorkspace[wsID]; ok {
		delete(sa.used, s)
		delete(sa.byWorkspace, wsID)
	}
}

// adopt claims a specific slot for a workspace on startup, reclaiming it from a
// pre-existing name so it is not handed to a different workspace. Re-adopting
// the same (wsID, slot) is idempotent; adopting a slot already used by another
// workspace is ignored (the existing owner keeps it). Mirrors the Python's
// startup loop that seeds slots/used_slots from matching workspace names.
func (sa *slotAllocator) adopt(wsID, slot int) {
	if slot < 1 || slot > state.MaxSlots {
		return
	}
	if existing, ok := sa.byWorkspace[wsID]; ok {
		if existing == slot {
			return
		}
		// Workspace already owns a different slot; keep the first.
		return
	}
	if sa.used[slot] {
		return // slot taken by another workspace
	}
	sa.used[slot] = true
	sa.byWorkspace[wsID] = slot
}

// slotOf returns the slot currently held by wsID, if any.
func (sa *slotAllocator) slotOf(wsID int) (int, bool) {
	s, ok := sa.byWorkspace[wsID]
	return s, ok
}

// sortedWorkspaceIDs returns the keys of a desired map in ascending order, so
// reconciliation (and tests) process workspaces deterministically.
func sortedWorkspaceIDs(d map[int]desired) []int {
	ids := make([]int, 0, len(d))
	for id := range d {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}
