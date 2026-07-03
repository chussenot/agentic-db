package doctor

import (
	"flag"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/mrzor/claude-status/internal/db"
)

// RunEvents implements `claude-status events` — print the audit log,
// newest first. Flags: --db <path>, --limit N (default 50), --session <id> to
// filter to one session. Use it to diagnose state drift (e.g. a session stuck
// in 'prompt': look for the Notification row with matched=true and read its
// message).
func RunEvents(args []string) error {
	fs := flag.NewFlagSet("events", flag.ContinueOnError)
	path := fs.String("db", db.DefaultDBPath(), "path to the SQLite database")
	limit := fs.Int("limit", 50, "max rows to show (newest first)")
	session := fs.String("session", "", "filter to a single session_id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return runEvents(os.Stdout, *path, *limit, *session)
}

func runEvents(w io.Writer, path string, limit int, session string) error {
	d, err := db.Open(path)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer d.Close()

	events, err := d.RecentEvents(limit, session)
	if err != nil {
		return fmt.Errorf("read events: %w", err)
	}
	fmt.Fprintf(w, "events: %d row(s), newest first", len(events))
	if session != "" {
		fmt.Fprintf(w, " (session %s)", session)
	}
	fmt.Fprintln(w)

	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "TIME\tSESSION\tEVENT\tWIN\tMATCH\tNEW_STATE\tMESSAGE")
	for _, e := range events {
		match := ""
		if e.Matched.Valid {
			match = fmt.Sprintf("%t", e.Matched.Bool)
		}
		// A TitleChanged row carries no Notification message; show the window
		// title in the MESSAGE column instead so the audit log reads naturally.
		msg := ""
		switch {
		case e.WindowTitle.Valid:
			msg = truncate(e.WindowTitle.String, 60)
		case e.Message.Valid:
			msg = truncate(e.Message.String, 60)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			time.UnixMilli(e.TS).Format("15:04:05"),
			shortID(e.SessionID), e.Event,
			nullInt(e.WindowID.Int64, e.WindowID.Valid),
			match, e.NewState, msg)
	}
	tw.Flush()
	return nil
}

func shortID(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
