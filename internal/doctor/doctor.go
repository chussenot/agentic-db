// Package doctor implements the `claude-status doctor` and `gc` subcommands —
// the Stage A end-to-end acceptance tools. doctor opens (creating) the DB and
// dumps its rows, then lists the live niri windows, proving the foundation
// works. gc runs a single dead-session reap pass.
package doctor

import (
	"flag"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/zor/claude-status/internal/db"
	"github.com/zor/claude-status/internal/niri"
)

// Run executes the doctor subcommand. args is os.Args[2:]. It accepts an
// optional --db override of the database path.
func Run(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	path := fs.String("db", db.DefaultDBPath(), "path to the SQLite database")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return run(os.Stdout, *path)
}

func run(w io.Writer, path string) error {
	fmt.Fprintf(w, "claude-status doctor\n")
	fmt.Fprintf(w, "database: %s\n\n", path)

	d, err := db.Open(path)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer d.Close()

	sessions, err := d.LoadLive()
	if err != nil {
		return fmt.Errorf("load sessions: %w", err)
	}
	fmt.Fprintf(w, "sessions: %d row(s)\n", len(sessions))
	if len(sessions) > 0 {
		tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "SESSION_ID\tSTATE\tWINDOW\tTERM_PID\tCWD\tLAST_TALK\tLAST_SEEN")
		for _, s := range sessions {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%d\n",
				s.SessionID, s.State,
				nullInt(s.WindowID.Int64, s.WindowID.Valid),
				nullInt(s.TerminalPID.Int64, s.TerminalPID.Valid),
				nullStr(s.Cwd.String, s.Cwd.Valid),
				nullInt(s.LastTalkTS.Int64, s.LastTalkTS.Valid),
				s.LastSeenTS,
			)
		}
		tw.Flush()
	}

	fmt.Fprintf(w, "\nniri windows:\n")
	windows, err := niri.ListWindows()
	if err != nil {
		fmt.Fprintf(w, "  ERROR: %v\n", err)
		return nil // doctor reports rather than fails on a niri-less host
	}
	fmt.Fprintf(w, "  %d window(s)\n", len(windows))
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "  ID\tWS\tPID\tAPP_ID\tTITLE")
	for _, win := range windows {
		fmt.Fprintf(tw, "  %d\t%d\t%d\t%s\t%s\n",
			win.ID, win.WorkspaceID, win.PID, win.AppID, truncate(win.Title, 50))
	}
	tw.Flush()
	return nil
}

func nullInt(v int64, valid bool) string {
	if !valid {
		return "NULL"
	}
	return fmt.Sprintf("%d", v)
}

func nullStr(v string, valid bool) string {
	if !valid {
		return "NULL"
	}
	return v
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
