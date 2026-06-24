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
	"sort"
	"text/tabwriter"
	"time"

	"github.com/mrzor/claude-status/internal/clauded"
	"github.com/mrzor/claude-status/internal/db"
	"github.com/mrzor/claude-status/internal/niri"
	"github.com/mrzor/claude-status/internal/state"
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

	printFirstParty(w, sessions)

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

// printFirstParty dumps Claude Code's first-party per-session status alongside
// our hook-derived state, so drift between the two is visible at a glance. This
// is the live view of the signal the daemon overlays (see
// daemon.overlayFirstParty); EFFECTIVE is what the daemon actually uses.
func printFirstParty(w io.Writer, sessions []db.Session) {
	dir := clauded.DefaultDir()
	fmt.Fprintf(w, "\nfirst-party status (%s):\n", dir)
	fp, err := clauded.Read(dir)
	if err != nil {
		fmt.Fprintf(w, "  ERROR: %v\n", err)
		return
	}
	fmt.Fprintf(w, "  %d session file(s)\n", len(fp))
	if len(fp) == 0 {
		return
	}

	hookState := make(map[string]string, len(sessions))
	hookSeen := make(map[string]int64, len(sessions))
	for _, s := range sessions {
		hookState[s.SessionID] = s.State
		hookSeen[s.SessionID] = s.LastSeenTS
	}

	ids := make([]string, 0, len(fp))
	for id := range fp {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "  SESSION\tPID\tFIRST_PARTY\tAGE\tHOOK_STATE\tEFFECTIVE\tSOURCE")
	for _, id := range ids {
		f := fp[id]
		hs, inDB := hookState[id]
		if !inDB {
			hs = "—" // first-party file with no live DB row (no window resolved yet)
		}
		effective, source := hs, "hook"
		if st, ok := effectiveState(f, hookSeen[id]); ok {
			effective, source = string(st), "first-party"
		}
		fmt.Fprintf(tw, "  %s\t%d\t%s\t%s\t%s\t%s\t%s\n",
			shortID(id), f.PID, string(f.Status), statusAge(f.StatusUpdatedAt),
			hs, effective, source)
	}
	tw.Flush()
}

// effectiveState mirrors daemon.firstPartyState's mapping for display (incl. the
// busy freshness gate against hookLastSeen). The daemon is the runtime authority;
// this copy keeps doctor free of a daemon import. busy->working UNLESS the busy
// file is older than the last hook (stale -> defer to hook); idle->idle;
// shell->shell. `waiting` is deferred to the hook state (it is overloaded; see
// firstPartyState) UNLESS waitingFor names a genuine permission prompt
// (Session.IsUserPrompt), which maps to prompt and shows SOURCE=first-party.
// Unknown -> not ok.
func effectiveState(s clauded.Session, hookLastSeen int64) (state.Status, bool) {
	switch s.Status {
	case clauded.Busy:
		if !s.StatusUpdatedAt.IsZero() && s.StatusUpdatedAt.UnixMilli() < hookLastSeen {
			return "", false // stale busy: defer to the newer hook state
		}
		return state.Working, true
	case clauded.Idle:
		return state.Idle, true
	case clauded.Shell:
		return state.Shell, true
	case clauded.Waiting:
		if s.IsUserPrompt() {
			return state.Prompt, true
		}
		return "", false // internal wait: defer to hook state
	default: // unrecognized values
		return "", false
	}
}

// statusAge renders how long ago the first-party status was updated.
func statusAge(t time.Time) string {
	if t.IsZero() {
		return "?"
	}
	return db.Now().Sub(t).Truncate(time.Second).String()
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
