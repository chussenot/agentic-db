// Package state is the single source of truth for the cross-package "grammar"
// of claude-status: the workspace-name encoding consumed by waybar, the decay
// table that maps idle elapsed time to a brightness level, the mapping from
// Claude Code hook events to session state, and the glyph/color render tables.
//
// Every other package (hook, daemon, waybar) imports this package and must not
// re-derive any of these constants locally. If a number changes, it changes
// here.
package state

import (
	"regexp"
	"strconv"
	"time"
)

// MaxSlots is the number of globally-unique workspace "slots" the daemon's
// allocator can hand out (slots are numbered 1..MaxSlots). Bumping this means
// regenerating the waybar fragments (gen-waybar enumerates per slot).
const MaxSlots = 20

// DecayLevels is the number of idle decay buckets. Levels run 0..DecayLevels-1
// (0 = brightest, just stopped; DecayLevels-1 = dimmest, stale). There are
// DecayLevels-1 finite thresholds plus one open-ended final bucket; see
// DecayThresholds.
const DecayLevels = 7

// Status is the per-session (and per-workspace, after aggregation) activity
// state. It is string-backed so it can be stored verbatim in the SQLite
// sessions.state column and validated on read.
type Status string

const (
	// Working means Claude is actively taking a turn (rendered as a blinking
	// orange dot). Set on UserPromptSubmit and PostToolUse.
	Working Status = "working"
	// Prompt means Claude is waiting for the user — a permission request or an
	// idle nudge (rendered as a blinking yellow "?"). Set on a filtered
	// Notification.
	Prompt Status = "prompt"
	// Idle means Claude is not working and not waiting; the decay bar encodes how
	// long ago it last talked. Set on SessionStart and Stop.
	Idle Status = "idle"
	// Shell means Claude is passively monitoring a background shell/command
	// (rendered as a pulsating cyan dot). It comes only from Claude Code's
	// first-party status (the "shell" value); our hooks never set it.
	Shell Status = "shell"
)

// Valid reports whether s is one of the known statuses.
func (s Status) Valid() bool {
	switch s {
	case Working, Prompt, Idle, Shell:
		return true
	default:
		return false
	}
}

// nameRE matches the waybar workspace-name grammar. Group 1 is the status sigil
// (w|p|i|s), group 2 is the slot number, group 3 (optional, idle only) is the
// decay level. This is also the "adopt" regex the daemon uses to reclaim slots
// from pre-existing workspace names on startup.
var nameRE = regexp.MustCompile(`^c(w|p|i|s)(\d+)(?:l(\d+))?$`)

// EncodeWorking returns the workspace name for a working session in the given
// slot, e.g. EncodeWorking(3) == "cw3".
func EncodeWorking(slot int) string {
	return "cw" + strconv.Itoa(slot)
}

// EncodePrompt returns the workspace name for a prompting session in the given
// slot, e.g. EncodePrompt(3) == "cp3".
func EncodePrompt(slot int) string {
	return "cp" + strconv.Itoa(slot)
}

// EncodeIdle returns the workspace name for an idle session in the given slot at
// the given decay level, e.g. EncodeIdle(3, 2) == "ci3l2".
func EncodeIdle(slot, level int) string {
	return "ci" + strconv.Itoa(slot) + "l" + strconv.Itoa(level)
}

// EncodeShell returns the workspace name for a shell-monitoring session in the
// given slot, e.g. EncodeShell(3) == "cs3".
func EncodeShell(slot int) string {
	return "cs" + strconv.Itoa(slot)
}

// Encode returns the workspace name for the given status, slot, and (idle-only)
// decay level. level is ignored for Working, Prompt, and Shell. It is the
// inverse of ParseName.
func Encode(status Status, slot, level int) string {
	switch status {
	case Working:
		return EncodeWorking(slot)
	case Prompt:
		return EncodePrompt(slot)
	case Shell:
		return EncodeShell(slot)
	default:
		return EncodeIdle(slot, level)
	}
}

// ParseName decodes a waybar workspace name produced by the Encode* functions.
// It returns the slot (1..MaxSlots by construction), the status, the decay
// level (0 for working/prompt, which carry no level), and ok=false if name does
// not match the grammar. It is the inverse of Encode and the daemon's slot-
// adoption matcher.
func ParseName(name string) (slot int, status Status, level int, ok bool) {
	m := nameRE.FindStringSubmatch(name)
	if m == nil {
		return 0, "", 0, false
	}
	slot, err := strconv.Atoi(m[2])
	if err != nil {
		return 0, "", 0, false
	}
	switch m[1] {
	case "w":
		status = Working
	case "p":
		status = Prompt
	case "s":
		status = Shell
	case "i":
		status = Idle
		if m[3] != "" {
			level, err = strconv.Atoi(m[3])
			if err != nil {
				return 0, "", 0, false
			}
		}
	}
	return slot, status, level, true
}

// DecayThresholds holds the upper bound (exclusive at the top) of each finite
// idle decay bucket, in order. There are DecayLevels-1 entries; an elapsed time
// at or beyond the last threshold falls into the final, open-ended level
// (DecayLevels-1). The 60-minute window from the design doc:
//
//	level 0: 0..1m      level 4: 20..35m
//	level 1: 1..4m      level 5: 35..60m
//	level 2: 4..10m     level 6: >60m
//	level 3: 10..20m
var DecayThresholds = []time.Duration{
	1 * time.Minute,
	4 * time.Minute,
	10 * time.Minute,
	20 * time.Minute,
	35 * time.Minute,
	60 * time.Minute,
}

// DecayLevel maps elapsed time since a session last talked to a decay level in
// 0..DecayLevels-1. Negative elapsed clamps to 0. The boundaries are inclusive
// at the lower edge: exactly 1m is level 1, exactly 60m is level 6.
func DecayLevel(elapsed time.Duration) int {
	for i, t := range DecayThresholds {
		if elapsed < t {
			return i
		}
	}
	return DecayLevels - 1
}

// Transition is the outcome of applying a Claude Code hook event to a session.
// It is the return value of MapEvent.
type Transition struct {
	// NewStatus is the status the session should move to. Valid only when
	// ChangeStatus is true.
	NewStatus Status
	// ChangeStatus reports whether NewStatus should be written. Some events
	// (SubagentStop) bump timestamps without changing the status.
	ChangeStatus bool
	// BumpTalk reports whether last_talk_ts should be set to now (the event
	// represents Claude having just finished talking, which (re)starts decay).
	BumpTalk bool
	// Delete reports whether the session row should be deleted (SessionEnd).
	Delete bool
	// Known reports whether the event name was recognized. For unknown events
	// all other fields are zero and the hook should treat it as a no-op
	// heartbeat (still bumping last_seen_ts at the call site).
	Known bool
}

// MapEvent maps a Claude Code hook_event_name to the resulting session
// transition, per the design doc's event->state table:
//
//	SessionStart     -> idle,    bump last_talk_ts (also resolves window_id)
//	UserPromptSubmit -> working
//	PostToolUse      -> working  (heartbeat so long turns don't look stale)
//	Notification     -> prompt   (caller filters which notifications qualify)
//	Stop             -> idle,    bump last_talk_ts (decay starts)
//	SubagentStop     -> (status unchanged), bump last_talk_ts
//	SessionEnd       -> delete the row
//
// Callers are responsible for bumping last_seen_ts (the liveness heartbeat) on
// every event regardless of the returned Transition, and for the Notification
// payload filtering that decides whether a Notification actually means Prompt.
func MapEvent(event string) Transition {
	switch event {
	case "SessionStart":
		return Transition{NewStatus: Idle, ChangeStatus: true, BumpTalk: true, Known: true}
	case "UserPromptSubmit":
		return Transition{NewStatus: Working, ChangeStatus: true, Known: true}
	case "PostToolUse":
		return Transition{NewStatus: Working, ChangeStatus: true, Known: true}
	case "Notification":
		return Transition{NewStatus: Prompt, ChangeStatus: true, Known: true}
	case "Stop":
		return Transition{NewStatus: Idle, ChangeStatus: true, BumpTalk: true, Known: true}
	case "SubagentStop":
		return Transition{BumpTalk: true, Known: true}
	case "SessionEnd":
		return Transition{Delete: true, Known: true}
	default:
		return Transition{Known: false}
	}
}

// ---- Render tables (consumed by the waybar generator) --------------------

// Color constants for the working and prompt blink animations. Idle colors are
// per-level; see IdleColor.
const (
	// WorkingColor is the orange used for the working blink.
	WorkingColor = "#d97757"
	// PromptColor is the yellow used for the prompt blink.
	PromptColor = "#ebcb8b"
	// ShellColor is the cyan used for the shell-monitoring pulse.
	ShellColor = "#5ccfe6"
)

// idleGlyphs is the two-cell shade bar per decay level (the shape axis).
var idleGlyphs = [DecayLevels]string{
	" ██", " █▓", " ▓▓", " ▓▒", " ▒▒", " ▒░", " ░░",
}

// idleColors is the per-level color (the brightness axis), bright white fading
// to dim grey.
var idleColors = [DecayLevels]string{
	"#ffffff", "#d8d8d8", "#b0b0b0", "#888888", "#686868", "#505050", "#3a3a3a",
}

// WorkingIcon returns the waybar glyph for a working session.
func WorkingIcon() string { return " ●" }

// PromptIcon returns the waybar glyph for a prompting session.
func PromptIcon() string { return " ?" }

// ShellIcon returns the waybar glyph for a shell-monitoring session (a dot, like
// working — the cyan pulse distinguishes it).
func ShellIcon() string { return " ●" }

// IdleIcon returns the waybar two-cell shade glyph for the given decay level.
// Out-of-range levels clamp to the nearest valid level.
func IdleIcon(level int) string {
	return idleGlyphs[clampLevel(level)]
}

// IdleColor returns the hex color for the given idle decay level. Out-of-range
// levels clamp to the nearest valid level.
func IdleColor(level int) string {
	return idleColors[clampLevel(level)]
}

func clampLevel(level int) int {
	if level < 0 {
		return 0
	}
	if level >= DecayLevels {
		return DecayLevels - 1
	}
	return level
}

// String implements fmt.Stringer for Status.
func (s Status) String() string { return string(s) }
