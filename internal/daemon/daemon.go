// Package daemon implements the `claude-status daemon` subcommand — the
// long-lived reconciler. It maintains an in-memory niri window/workspace model
// (from the niri event stream), polls the sessions DB, advances decay buckets,
// reaps dead sessions, and is the sole mutator of niri workspace names.
//
// Concurrency model (the trickiest part, ported from niri-topic-namer's
// single-threaded loop): one ACTOR goroutine owns all mutable state — the niri
// Model, the slot allocator, and the `managed` map (workspace id -> the name we
// last set). Three feeder goroutines never touch that state; they only send on
// channels the actor selects over:
//
//	A: niri.StreamEvents  -> Event channel       (topology changes)
//	B: db.LoadLive  every ~250ms -> []Session    (the source of truth)
//	C: a 1s ticker  -> tick                       (decay recompute + GC)
//
// The actor folds events/snapshots into state, marks itself dirty, and after a
// 150ms debounce runs the reconciler: aggregate desired per-workspace state,
// diff against `managed`, and emit niri renames ONLY where the name changed
// (redundant-IPC suppression). Tick C additionally runs GC. This single-mutator
// design means no locks and faithfully reproduces the Python's behavior.

package daemon

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/zor/claude-status/internal/db"
	"github.com/zor/claude-status/internal/niri"
	"github.com/zor/claude-status/internal/state"
)

const (
	// dbPollInterval is goroutine B's cadence: re-read the session set. WAL makes
	// this cheap; SQLite has no change-notify, so we poll.
	dbPollInterval = 250 * time.Millisecond
	// tickInterval is goroutine C's cadence: drives decay-bucket recompute and
	// GC. Decay buckets are minutes wide, so 1s is ample resolution.
	tickInterval = 1 * time.Second
	// debounce is how long the actor waits, after going dirty, for a burst of
	// events to settle before reconciling. Matches the Python's 150ms.
	debounce = 150 * time.Millisecond
)

// Run executes the daemon subcommand. args is os.Args[2:]. It blocks until the
// process is signaled (SIGINT/SIGTERM), then shuts down cleanly.
func Run(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	dbPath := fs.String("db", db.DefaultDBPath(), "path to the sessions SQLite database")
	if err := fs.Parse(args); err != nil {
		return err
	}

	database, err := db.Open(*dbPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer database.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	d := &daemon{
		db:    database,
		model: niri.NewModel(),
		act:   niri.IPCActuator{},
		slots: newSlotAllocator(),
		// managed mirrors the Python's State.managed: ws id -> the name we set.
		managed: make(map[int]string),
	}
	return d.run(ctx)
}

// daemon holds the actor-owned state plus its dependencies. All fields are
// touched ONLY from run() (the actor goroutine) after construction.
type daemon struct {
	db    *db.DB
	model *niri.Model
	act   niri.Actuator
	slots *slotAllocator

	// managed maps a workspace id to the name we most recently set on it. Absent
	// => we have not named it. Used for redundant-IPC suppression and to know
	// whether to use the index (first) or name (subsequent) reference.
	managed map[int]string

	// sessions is the latest DB snapshot from goroutine B.
	sessions []db.Session
	// adopted guards the one-time startup adoption of pre-existing names, done
	// the first time the model has a workspace snapshot.
	adopted bool
}

// run is the actor goroutine: it owns all mutable state and is the sole mutator
// of niri names. It launches feeders A/B/C and selects over their channels.
func (d *daemon) run(ctx context.Context) error {
	// Goroutine A: niri event stream.
	events, err := niri.StreamEvents(ctx)
	if err != nil {
		return fmt.Errorf("subscribe niri event-stream: %w", err)
	}

	// Goroutine B: DB poller. The channel is bidirectional here so the poller can
	// drop a stale buffered snapshot in favor of a fresh one (coalescing).
	snapshots := make(chan []db.Session, 1)
	go d.pollDB(ctx, snapshots)

	// Goroutine C: tick.
	ticks := make(chan struct{}, 1)
	go d.pollTick(ctx, ticks)

	// Debounce timer: created stopped; armed when we first go dirty.
	debTimer := time.NewTimer(time.Hour)
	if !debTimer.Stop() {
		<-debTimer.C
	}
	dirty := false
	markDirty := func() {
		if !dirty {
			dirty = true
			debTimer.Reset(debounce)
		}
	}

	for {
		select {
		case <-ctx.Done():
			// Match the Python: it leaves the names in place on exit (only frees
			// slots in-process). We do the same — names persist so a daemon
			// restart re-adopts them. Stop the stream child via ctx (CommandContext).
			return nil

		case ev, ok := <-events:
			if !ok {
				// niri exited / stream closed. Nothing more to reconcile.
				return fmt.Errorf("niri event-stream closed")
			}
			if d.applyEvent(ev) {
				markDirty()
			}

		case snap := <-snapshots:
			d.sessions = snap
			markDirty()

		case <-ticks:
			// Tick drives GC and forces a reconcile so decay buckets advance even
			// with no niri/DB events (the bucket-crossing rename path).
			d.gc()
			markDirty()

		case <-debTimer.C:
			dirty = false
			d.reconcile()
		}
	}
}

// applyEvent folds a niri event into the model and performs the one-time
// startup adoption once workspaces are known. Returns whether a reconcile may
// be needed.
func (d *daemon) applyEvent(ev niri.Event) bool {
	changed := d.model.ApplyEvent(ev)
	if !d.adopted && ev.Kind == niri.KindWorkspacesChanged {
		d.adoptExistingNames()
		d.adopted = true
	}
	return changed
}

// adoptExistingNames reclaims slots from workspace names left by a previous run
// (or the Python daemon) so we neither reuse those slots nor orphan the names.
// Ports the Python's startup loop, using state.ParseName as the adopt regex.
func (d *daemon) adoptExistingNames() {
	for id, w := range d.model.Workspaces() {
		slot, _, _, ok := state.ParseName(w.Name)
		if !ok {
			continue
		}
		d.slots.adopt(id, slot)
		d.managed[id] = w.Name
	}
}

// pollDB is goroutine B: every dbPollInterval, snapshot the session set and send
// it (coalescing: a stale snapshot in the buffer is dropped for the fresh one).
func (d *daemon) pollDB(ctx context.Context, out chan []db.Session) {
	t := time.NewTicker(dbPollInterval)
	defer t.Stop()
	// Prime immediately so the first reconcile has data without a 250ms wait.
	d.sendSnapshot(ctx, out)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.sendSnapshot(ctx, out)
		}
	}
}

func (d *daemon) sendSnapshot(ctx context.Context, out chan []db.Session) {
	sessions, err := d.db.LoadLive()
	if err != nil {
		logf("db poll: %v", err)
		return
	}
	select {
	case out <- sessions:
	case <-ctx.Done():
	default:
		// Buffer full with an unconsumed snapshot; drop the stale one and enqueue
		// the fresh one so the actor always sees the latest.
		select {
		case <-out:
		default:
		}
		select {
		case out <- sessions:
		default:
		}
	}
}

// pollTick is goroutine C: emit a tick every tickInterval (coalesced).
func (d *daemon) pollTick(ctx context.Context, out chan<- struct{}) {
	t := time.NewTicker(tickInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			select {
			case out <- struct{}{}:
			default: // a tick is already pending; one is enough
			}
		}
	}
}

// gc reaps dead sessions from the DB using the live model. Runs on the actor
// goroutine (reads the model safely). The reaped rows vanish from the next DB
// snapshot, which clears their dots via the normal reconcile path.
func (d *daemon) gc() {
	pred := deadPredicate(d.model, db.Now())
	if n, err := d.db.ReapDead(pred); err != nil {
		logf("gc: %v", err)
	} else if n > 0 {
		logf("gc: reaped %d dead session(s)", n)
	}
}

// reconcile is the sole mutator of niri names. It computes desired per-workspace
// state from the latest DB snapshot + live model, diffs against `managed`, and
// emits niri renames only where the name changed. Ports State.reconcile, with
// the DB-sourced aggregation replacing title-scraping.
func (d *daemon) reconcile() {
	want := aggregate(d.sessions, d.model, db.Now())

	// 1) Clear names for workspaces we manage that are no longer wanted, or that
	//    have vanished from niri entirely.
	for wsID, curName := range d.managed {
		if _, stillWanted := want[wsID]; stillWanted {
			continue
		}
		if _, exists := d.model.Workspace(wsID); !exists {
			// Workspace gone — its name vanished with it; just free the slot.
			d.slots.free(wsID)
			delete(d.managed, wsID)
			continue
		}
		// Workspace exists but has no Claude session now: unset by name reference
		// (works on any output).
		if err := d.act.UnsetWorkspaceName(curName); err != nil {
			logf("unset %q (ws %d): %v", curName, wsID, err)
			continue
		}
		d.model.SetName(wsID, "")
		d.slots.free(wsID)
		delete(d.managed, wsID)
	}

	// 2) Set/update names for wanted workspaces, in deterministic order.
	for _, wsID := range sortedWorkspaceIDs(want) {
		dz := want[wsID]
		ws, exists := d.model.Workspace(wsID)
		if !exists {
			continue // raced away; next reconcile handles it
		}

		if curName, named := d.managed[wsID]; named {
			// Steady state: update by NAME reference (any output).
			slot, ok := d.slots.slotOf(wsID)
			if !ok {
				// Defensive: managed without a slot (shouldn't happen). Re-adopt.
				slot, ok = d.slots.assign(wsID)
				if !ok {
					continue
				}
			}
			newName := state.Encode(dz.status, slot, dz.level)
			if newName == curName {
				continue // redundant-IPC suppression
			}
			if err := d.act.SetWorkspaceNameByName(curName, newName); err != nil {
				logf("rename %q->%q (ws %d): %v", curName, newName, wsID, err)
				continue
			}
			d.model.SetName(wsID, newName)
			d.managed[wsID] = newName
			continue
		}

		// First name for this workspace: must use an INDEX reference, which niri
		// resolves only on the FOCUSED OUTPUT. Other outputs wait until focused
		// (the focus-invariant bootstrap). Once named, all future updates go
		// through the name-reference branch above and work on any output.
		if ws.Output != d.model.FocusedOutput() {
			continue
		}
		slot, ok := d.slots.assign(wsID)
		if !ok {
			continue // slot exhaustion: no dot until one frees
		}
		newName := state.Encode(dz.status, slot, dz.level)
		if err := d.act.SetWorkspaceNameByIndex(ws.Idx, newName); err != nil {
			logf("bootstrap name %q on ws %d (idx %d): %v", newName, wsID, ws.Idx, err)
			d.slots.free(wsID)
			continue
		}
		d.model.SetName(wsID, newName)
		d.managed[wsID] = newName
	}
}

// logf writes a daemon diagnostic to stderr. The daemon runs under niri's
// spawn-at-startup, so stderr lands wherever niri logs its children.
func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "claude-status daemon: "+format+"\n", args...)
}
