package doctor

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/mrzor/claude-status/internal/clauded"
	"github.com/mrzor/claude-status/internal/db"
	"github.com/mrzor/claude-status/internal/niri"
)

// RunGC executes the `gc` subcommand: a single dead-session reap pass. args is
// os.Args[2:] and accepts --db.
//
// A session is considered dead when it has a resolved niri window_id that is no
// longer present in the live niri window set. (Stage B's daemon uses a richer
// predicate — also /proc liveness and a stale last_seen_ts timeout — but this
// standalone pass covers the common case and proves ReapDead end-to-end.)
func RunGC(args []string) error {
	fs := flag.NewFlagSet("gc", flag.ContinueOnError)
	path := fs.String("db", db.DefaultDBPath(), "path to the SQLite database")
	orphans := fs.Bool("orphans", false, "instead of the window pass, list windowless rows Claude Code no longer lists (dry run unless --yes)")
	sessionsDir := fs.String("sessions-dir", clauded.DefaultDir(), "Claude Code sessions dir (the liveness authority for --orphans)")
	orphanAge := fs.Duration("orphan-age", 24*time.Hour, "with --orphans: only consider rows unseen for at least this long")
	yes := fs.Bool("yes", false, "with --orphans: actually delete the rows it lists")
	if err := fs.Parse(args); err != nil {
		return err
	}

	d, err := db.Open(*path)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer d.Close()

	if *orphans {
		return runOrphans(os.Stdout, d, *sessionsDir, *orphanAge, *yes)
	}

	live := map[int64]bool{}
	windows, err := niri.ListWindows()
	if err != nil {
		return fmt.Errorf("list windows: %w", err)
	}
	for _, w := range windows {
		live[int64(w.ID)] = true
	}

	n, err := d.ReapDead(func(s db.Session) bool {
		return s.WindowID.Valid && !live[s.WindowID.Int64]
	})
	if err != nil {
		return fmt.Errorf("reap: %w", err)
	}
	fmt.Fprintf(os.Stdout, "gc: reaped %d dead session(s)\n", n)
	return nil
}

// orphanProcExists is the /proc seam for the orphan sweep.
var orphanProcExists = func(pid int) bool {
	_, err := os.Stat("/proc/" + strconv.Itoa(pid))
	return err == nil
}

// runOrphans lists (and with confirm=true, deletes) rows that no longer correspond
// to anything running: no niri window, not in Claude Code's live session set, no
// live Claude process, and untouched for at least minAge.
//
// This is the cleanup path for rows that leaked BEFORE the tracker knew what a
// background agent was. Those rows have no window (agents never had one) and no
// claude_pid (the column did not exist when they were written), so no automatic
// check can prove them dead — the daemon deliberately never auto-reaps a windowless
// row it cannot positively identify, because that is also the shape of an ssh or
// tmux session. Hence: manual, opt-in, and a dry run by default.
//
// THE FIRST-PARTY CHECK IS LOAD-BEARING, and age is NOT a substitute for it. The
// first version of this sweep condemned rows on age alone, and pointed at four LIVE
// background agents: a monitoring agent emits no hooks for hours, so its last_seen
// is arbitrarily old while it is perfectly alive. That is precisely the trap the
// daemon's "no last_seen staleness reap" rule exists to avoid. So: anything Claude
// Code still lists is alive by definition, and if the first-party set cannot be read
// at all, this refuses to sweep rather than guess.
func runOrphans(w io.Writer, d *db.DB, sessionsDir string, minAge time.Duration, confirm bool) error {
	rows, err := d.LoadLive()
	if err != nil {
		return fmt.Errorf("load sessions: %w", err)
	}
	live, err := clauded.ReadLive(sessionsDir)
	if err != nil {
		return fmt.Errorf("read first-party sessions (%s): %w", sessionsDir, err)
	}
	if len(live) == 0 {
		return fmt.Errorf("refusing to sweep: no live sessions found under %s, so "+
			"nothing can be proven dead (is Claude Code running? is --sessions-dir right?)", sessionsDir)
	}
	cutoff := db.Now().Add(-minAge).UnixMilli()

	var victims []db.Session
	for _, s := range rows {
		if s.WindowID.Valid || s.TerminalPID.Valid {
			continue // the ordinary window/pid pass owns these
		}
		if _, listed := live[s.SessionID]; listed {
			continue // Claude Code still lists it: alive, however quiet
		}
		if s.ClaudePID.Valid && s.ClaudePID.Int64 > 0 && orphanProcExists(int(s.ClaudePID.Int64)) {
			continue // provably alive
		}
		if s.LastSeenTS > cutoff {
			continue // too fresh to call
		}
		victims = append(victims, s)
	}
	sort.Slice(victims, func(i, j int) bool { return victims[i].LastSeenTS < victims[j].LastSeenTS })

	if len(victims) == 0 {
		fmt.Fprintf(w, "gc --orphans: nothing older than %s to clean up\n", minAge)
		return nil
	}

	verb := "would delete"
	if confirm {
		verb = "deleting"
	}
	fmt.Fprintf(w, "gc --orphans: %s %d windowless row(s) with no live process, unseen for %s+\n",
		verb, len(victims), minAge)
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "  SESSION\tKIND\tNAME\tCWD\tLAST_SEEN_AGE")
	for _, s := range victims {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n",
			shortID(s.SessionID),
			nullStr(s.Kind.String, s.Kind.Valid),
			nullStr(s.SessionName.String, s.SessionName.Valid),
			nullStr(s.Cwd.String, s.Cwd.Valid),
			db.Now().Sub(time.UnixMilli(s.LastSeenTS)).Truncate(time.Second),
		)
	}
	tw.Flush()

	if !confirm {
		fmt.Fprintf(w, "  (dry run — re-run with --yes to delete)\n")
		return nil
	}
	doomed := make(map[string]bool, len(victims))
	for _, s := range victims {
		doomed[s.SessionID] = true
	}
	n, err := d.ReapDead(func(s db.Session) bool { return doomed[s.SessionID] })
	if err != nil {
		return fmt.Errorf("reap: %w", err)
	}
	fmt.Fprintf(w, "gc --orphans: deleted %d row(s)\n", n)
	return nil
}
