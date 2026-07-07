package recap

import (
	"database/sql"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/mrzor/claude-status/internal/db"
	"github.com/mrzor/claude-status/internal/git"
	"github.com/mrzor/claude-status/internal/transcript"
)

// defaultTopN caps how many sessions are shown in full detail; the rest fold
// into the "+N more" summary and the project roll-ups.
const defaultTopN = 25

// enrichProjects fills the git fields (remote, branch, window commit count) for
// every project, marking HasRepo when the cwd resolves to a live git work tree.
// The render then lists only the repos and drops the rest (a removed temp
// worktree, a non-repo cwd like $HOME), which is why we enrich all rather than
// capping: the cap is applied by "does it have a repo?", not by count.
func enrichProjects(projects []Project, from, to time.Time) {
	for i := range projects {
		for _, dir := range projects[i].Dirs {
			info, ok := git.Describe(dir, from, to)
			if !ok {
				continue // a since-removed temp worktree / non-repo cwd — try the next
			}
			projects[i].HasRepo = true
			projects[i].Remote = info.Remote
			projects[i].Branch = info.Branch
			projects[i].Commits = info.Commits
			break
		}
	}
}

// periodDur maps a --period keyword to a trailing window length.
func periodDur(period string) (time.Duration, bool) {
	switch period {
	case "day":
		return 24 * time.Hour, true
	case "week":
		return 7 * 24 * time.Hour, true
	case "quarter":
		return 90 * 24 * time.Hour, true
	default:
		return 0, false
	}
}

// RunRecap implements `claude-status recap`. It resolves the [from,to] window
// from --period/--since/--until, aggregates the event log joined with session
// transcripts, and prints markdown (default) or JSON.
func RunRecap(args []string) error {
	fs := flag.NewFlagSet("recap", flag.ContinueOnError)
	dbPath := fs.String("db", db.DefaultDBPath(), "path to the claude-status sqlite database")
	tdir := fs.String("transcripts", transcript.DefaultDir(), "Claude Code transcripts root")
	period := fs.String("period", "day", "window: day|week|quarter (trailing)")
	since := fs.String("since", "", "override start: a duration (e.g. 48h) or a date (YYYY-MM-DD)")
	until := fs.String("until", "", "end date (YYYY-MM-DD); defaults to now")
	asJSON := fs.Bool("json", false, "emit JSON instead of markdown")
	metrics := fs.String("metrics", "full", "metrics section: full (append) | none (omit) | only")
	top := fs.Int("top", defaultTopN, "max sessions shown in detail")
	if err := fs.Parse(args); err != nil {
		return err
	}
	switch *metrics {
	case "full", "none", "only":
	default:
		return fmt.Errorf("--metrics %q: want full|none|only", *metrics)
	}

	to, err := resolveUntil(*until)
	if err != nil {
		return err
	}
	from, err := resolveFrom(*since, *period, to)
	if err != nil {
		return err
	}

	database, err := db.Open(*dbPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer database.Close()

	events, err := database.EventsBetween(from.UnixMilli(), to.UnixMilli())
	if err != nil {
		return fmt.Errorf("read events: %w", err)
	}

	lookup := func(sid string) (Meta, bool) {
		info, ok := transcript.Meta(*tdir, sid)
		if !ok {
			return Meta{}, false
		}
		return Meta{Title: info.Title, Ask: info.Ask, Cwd: info.Cwd, Branch: info.Branch}, true
	}

	// Repo captured at hook time (normalized). The primary repo is element 0 (see
	// LoadSessionRepos ordering). A missing entry makes Build fall back to the
	// transcript-cwd heuristic for that session.
	sessionRepos, err := database.LoadSessionRepos()
	if err != nil {
		return fmt.Errorf("load session repos: %w", err)
	}
	repoLookup := func(sid string) (db.RepoRef, bool) {
		refs := sessionRepos[sid]
		if len(refs) == 0 {
			return db.RepoRef{}, false
		}
		return refs[0], true
	}

	d := Build(events, lookup, repoLookup, from, to, *top)
	enrichProjects(d.Projects, from, to)
	if *asJSON {
		return JSON(os.Stdout, d)
	}
	if *metrics == "only" {
		MetricsMarkdown(os.Stdout, d)
		return nil
	}
	Markdown(os.Stdout, d)
	if *metrics == "full" {
		fmt.Fprintln(os.Stdout)
		MetricsMarkdown(os.Stdout, d)
	}
	return nil
}

// RunRepoBackfill implements `claude-status repo-backfill`. It walks every
// session id in the event log, and for those without a stored repo yet, runs the
// same heuristics recap used before normalization — transcript-glob for the cwd,
// then git for the repo identity — and persists the result (repos + session_repos).
// A session whose cwd no longer resolves to a work tree (a since-removed temp
// worktree, a non-repo dir) is left unresolved: NULL where the heuristic falls
// short is acceptable. It is idempotent: already-linked sessions are skipped, so
// it is safe to re-run.
func RunRepoBackfill(args []string) error {
	fs := flag.NewFlagSet("repo-backfill", flag.ContinueOnError)
	dbPath := fs.String("db", db.DefaultDBPath(), "path to the claude-status sqlite database")
	tdir := fs.String("transcripts", transcript.DefaultDir(), "Claude Code transcripts root")
	if err := fs.Parse(args); err != nil {
		return err
	}

	database, err := db.Open(*dbPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer database.Close()

	ids, err := database.DistinctEventSessionIDs()
	if err != nil {
		return fmt.Errorf("enumerate sessions: %w", err)
	}
	existing, err := database.LoadSessionRepos()
	if err != nil {
		return fmt.Errorf("load session repos: %w", err)
	}

	now := db.Now().UnixMilli()
	var resolved, unresolved, skipped int
	for _, sid := range ids {
		if len(existing[sid]) > 0 {
			skipped++
			continue
		}
		info, ok := transcript.Meta(*tdir, sid)
		if !ok || info.Cwd == "" {
			unresolved++
			continue
		}
		remote, root, branch, ok := git.Resolve(info.Cwd)
		if !ok {
			unresolved++
			continue
		}
		id, err := database.UpsertRepo(remote, root, now)
		if err != nil {
			unresolved++
			continue
		}
		// Prefer the transcript's recorded branch (the branch at that session's
		// time); fall back to git's current branch for the checkout.
		b := info.Branch
		if b == "" {
			b = branch
		}
		if err := database.LinkSessionRepo(sid, id, nullString(b), now); err != nil {
			return fmt.Errorf("link %s: %w", sid, err)
		}
		resolved++
	}

	fmt.Fprintf(os.Stdout, "repo-backfill: %d resolved, %d unresolved, %d already-linked (of %d sessions)\n",
		resolved, unresolved, skipped, len(ids))
	return nil
}

// nullString maps "" to a NULL string for the backfill's branch field.
func nullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// RunPrompt implements `claude-status recap-prompt`: it prints the period-tuned
// LLM instructions to stdout. The recap data itself is NOT included — it reaches
// the model via stdin in the pipeline:
//
//	claude-status recap --period day | claude -p "$(claude-status recap-prompt --period day)"
func RunPrompt(args []string) error {
	fs := flag.NewFlagSet("recap-prompt", flag.ContinueOnError)
	period := fs.String("period", "day", "report shape: day|week|quarter")
	note := fs.String("note", "", "revision feedback to steer a regenerated recap")
	if err := fs.Parse(args); err != nil {
		return err
	}
	_, err := io.WriteString(os.Stdout, Prompt(*period, *note))
	return err
}

// resolveUntil parses --until (a YYYY-MM-DD date, taken through the end of that
// day) or defaults to now.
func resolveUntil(until string) (time.Time, error) {
	if until == "" {
		return time.Now(), nil
	}
	d, err := time.ParseInLocation("2006-01-02", until, time.Local)
	if err != nil {
		return time.Time{}, fmt.Errorf("--until %q: want YYYY-MM-DD", until)
	}
	return d.AddDate(0, 0, 1), nil // inclusive through the given day
}

// resolveFrom parses --since (a Go duration meaning "trailing", or a
// YYYY-MM-DD date) or falls back to the trailing --period window ending at to.
func resolveFrom(since, period string, to time.Time) (time.Time, error) {
	if since == "" {
		dur, ok := periodDur(period)
		if !ok {
			return time.Time{}, fmt.Errorf("--period %q: want day|week|quarter", period)
		}
		return to.Add(-dur), nil
	}
	if dur, err := time.ParseDuration(since); err == nil {
		return to.Add(-dur), nil
	}
	if d, err := time.ParseInLocation("2006-01-02", since, time.Local); err == nil {
		return d, nil
	}
	return time.Time{}, fmt.Errorf("--since %q: want a duration (e.g. 48h) or a date (YYYY-MM-DD)", since)
}
