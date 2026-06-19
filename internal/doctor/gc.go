package doctor

import (
	"flag"
	"fmt"
	"os"

	"github.com/zor/claude-status/internal/db"
	"github.com/zor/claude-status/internal/niri"
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
	if err := fs.Parse(args); err != nil {
		return err
	}

	d, err := db.Open(*path)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer d.Close()

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
