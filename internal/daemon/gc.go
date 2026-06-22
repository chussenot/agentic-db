package daemon

import (
	"os"
	"strconv"

	"github.com/mrzor/claude-status/internal/clauded"
	"github.com/mrzor/claude-status/internal/db"
)

// firstPartyMissThreshold is how many consecutive GC ticks a local session must
// be absent from the (available) first-party set before it is reaped. The GC
// tick is 1s, so this is ~a few seconds of confirmed absence. The debounce does
// two jobs: it rides out the brief SessionStart->Claude-writes-its-file race (so
// a just-started session is never reaped before its file appears), and it
// absorbs a transient unreadable/mid-rewrite first-party file. A false reap is
// self-healing regardless — the live session's next hook recreates the row — so
// a small threshold is safe.
const firstPartyMissThreshold = 3

// windowPresence answers whether the niri model currently knows a window id.
// The live *niri.Model satisfies it; tests inject a fake.
type windowPresence interface {
	HasWindow(windowID int) bool
}

// procExists reports whether /proc/<pid> exists (the process is alive). It is a
// package var so tests can stub it without spawning real processes.
var procExists = func(pid int) bool {
	_, err := os.Stat("/proc/" + strconv.Itoa(pid))
	return err == nil
}

// deadPredicate builds the ReapDead predicate from the live window model and the
// set of sessions confirmed dead by first-party absence (fpDead, see
// firstPartyDead). Every session we track is LOCAL — the hook always resolves a
// niri window + terminal (kitty) pid — so liveness is ground truth: is the
// terminal still there? A session is dead if ANY of:
//
//   - its cached window_id is absent from the niri model (the terminal window
//     closed), OR
//   - its terminal_pid no longer has a /proc/<pid> (the kitty process died), OR
//   - it is in fpDead: Claude Code no longer lists it among its first-party
//     sessions even though its terminal is still alive (exited back to a shell,
//     or the terminal was reused by a different session) — the case the
//     window/pid liveness checks cannot see. See firstPartyDead.
//
// Window presence is the authoritative liveness signal (niri owns the window
// list); the /proc check is cheap backup for the brief interval where niri might
// still list a just-closed window. Closing the terminal trips both.
//
// There is deliberately NO heartbeat/last_seen staleness reap: an idle Claude
// emits no hooks, so a timeout would delete it mid-decay. Idle sessions instead
// persist (fading to dim ░░ and resting there) until their terminal closes,
// until SessionEnd deletes the row on a clean exit, or until first-party absence
// confirms the process is gone. last_seen_ts remains in the schema purely as a
// heartbeat/debug field, not a reap trigger.
func deadPredicate(model windowPresence, fpDead map[string]bool) func(db.Session) bool {
	return func(s db.Session) bool {
		if s.WindowID.Valid && !model.HasWindow(int(s.WindowID.Int64)) {
			return true
		}
		if s.TerminalPID.Valid && !procExists(int(s.TerminalPID.Int64)) {
			return true
		}
		if fpDead[s.SessionID] {
			return true
		}
		return false
	}
}

// firstPartyDead folds this tick's first-party observation into the daemon's
// per-session miss counter (d.fpMiss) and returns the set of session ids that
// have now been absent long enough to reap. It MUST run on the actor goroutine
// (it mutates d.fpMiss).
//
// A local session (window_id resolved) that is missing from Claude Code's live
// first-party set is a candidate for death: a live Claude — even one idle for
// many minutes — keeps its ~/.claude/sessions/<pid>.json, so absence means the
// process is gone while its terminal lingers (exited to a shell, or the terminal
// was reused). We require firstPartyMissThreshold consecutive misses before
// reaping (see that const for why), and reset the counter the moment a session
// reappears.
//
// Gating: when first-party data is not available (fpAvailable=false — the
// overlay is disabled, the dir is missing/unreadable, or simply empty) we cannot
// distinguish "Claude isn't writing files" from "every session ended", so we
// reap nothing here and clear all counters. That keeps old Claude versions (no
// session files) and the overlay-disabled mode on the liveness-only path.
// Non-local sessions (NULL window_id: remote/ssh/tmux) have no local file by
// design and are never tracked. Counters for sessions no longer in the snapshot
// are pruned so the map can't grow unbounded.
func (d *daemon) firstPartyDead(sessions []db.Session, fp map[string]clauded.Session, fpAvailable bool) map[string]bool {
	if !fpAvailable {
		if len(d.fpMiss) > 0 {
			d.fpMiss = make(map[string]int)
		}
		return nil
	}

	seen := make(map[string]bool, len(sessions))
	var dead map[string]bool
	for _, s := range sessions {
		if !s.WindowID.Valid {
			continue // non-local: no first-party file expected, never tracked
		}
		seen[s.SessionID] = true
		if _, live := fp[s.SessionID]; live {
			delete(d.fpMiss, s.SessionID) // reappeared (or never left): reset
			continue
		}
		d.fpMiss[s.SessionID]++
		if d.fpMiss[s.SessionID] >= firstPartyMissThreshold {
			if dead == nil {
				dead = make(map[string]bool)
			}
			dead[s.SessionID] = true
		}
	}
	// Prune counters for sessions that have left the snapshot (already reaped or
	// deleted) so the map tracks only live rows.
	for id := range d.fpMiss {
		if !seen[id] {
			delete(d.fpMiss, id)
		}
	}
	return dead
}
