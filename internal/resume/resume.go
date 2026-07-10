// Package resume implements `claude-status resume` — the data + shell-integration
// backend for resurrecting a recent (dead) Claude Code session.
//
// Claude Code's `claude --resume <id>` is scoped to the directory the session
// started in (resuming from elsewhere errors "No conversation found"), and a
// child process cannot change its parent shell's cwd. So the user-facing command
// is a zsh FUNCTION (see `--init zsh`): the function `cd`s into the session's dir
// — in the interactive shell, where the cd persists — and then runs
// `claude --resume`. This package only serves the function's data:
//
//	--init zsh   print the claude-resume() shell function (+ guarded `cr` alias)
//	--list       emit tab-delimited candidate rows for fzf
//	--show <id>  emit the detail/preview block for one session (fzf --preview)
//	(no mode)    print eval-safe `#` comments explaining how to wire it up
//
// fzf itself runs inside the shell function, not here — this stays a plain
// non-interactive data command, consistent with the rest of the binary.
package resume

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/mrzor/claude-status/internal/clauded"
	"github.com/mrzor/claude-status/internal/db"
	"github.com/mrzor/claude-status/internal/git"
	"github.com/mrzor/claude-status/internal/transcript"
)

// Now is the package clock seam (mirrors db.Now); tests override it for
// deterministic relative-age formatting.
var Now = time.Now

// Run implements `claude-status resume`. args is os.Args[2:].
func Run(args []string) error {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	initShell := fs.String("init", "", "print shell integration for the named shell (only: zsh)")
	listMode := fs.Bool("list", false, "emit tab-delimited candidate rows for a picker")
	showID := fs.String("show", "", "print the detail/preview block for one session id")
	limit := fs.Int("limit", 20, "max sessions to list")
	all := fs.Bool("all", false, "include currently-live sessions (default: only dead ones)")
	here := fs.Bool("here", false, "only sessions from the current directory's repo")
	dbPath := fs.String("db", db.DefaultDBPath(), "path to the claude-status sqlite database")
	tdir := fs.String("transcripts", transcript.DefaultDir(), "Claude Code transcripts root")
	sdir := fs.String("sessions", clauded.DefaultDir(), "Claude Code first-party sessions dir (liveness)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	switch {
	case *initShell != "":
		return runInit(os.Stdout, *initShell)
	case *showID != "":
		return runShow(os.Stdout, *tdir, *showID)
	case *listMode:
		return runList(os.Stdout, os.Stderr, listConfig{
			dbPath: *dbPath, tdir: *tdir, sdir: *sdir,
			limit: *limit, all: *all, here: *here,
		})
	default:
		printHelp(os.Stdout)
		return nil
	}
}

// ---------------------------------------------------------------------------
// candidate model + pure selection
// ---------------------------------------------------------------------------

// Candidate is one resurrectable session, enriched for display and resumption.
type Candidate struct {
	SessionID string
	Cwd       string // where `claude --resume` must run — the cd target
	Repo      string // normalized remote, else worktree root, else "" (no repo)
	Branch    string
	Topic     string // transcript ai-title, else the opening ask's first line
	LastTS    int64  // unix ms of last activity (recency)
}

// drops records why sessions were excluded, surfaced to stderr (never stdout,
// which is the picker feed) so a bounded list never silently hides work.
type drops struct {
	live      int // still running (excluded unless --all)
	noCwd     int // no resolvable directory to cd into — unresurrectable
	elsewhere int // filtered out by --here
}

// selectConfig is the pure-selection knobs.
type selectConfig struct {
	limit   int
	all     bool
	hereKey string // repo identity to keep when --here; "" means no filter
}

// repoRefLookup resolves a session's primary repo ref (element 0), if any.
type repoRefLookup func(sid string) (db.RepoRef, bool)

// metaLookup resolves a session's transcript info, if any.
type metaLookup func(sid string) (transcript.Info, bool)

// selectCandidates is the pure core: given the recency-ranked session list and
// injected lookups, it applies liveness/cwd/here filtering and enrichment,
// newest-first, capping at limit. It returns the kept candidates and a drop
// tally. Order of `recent` is preserved (callers pass it newest-first).
func selectCandidates(recent []db.RecentSession, meta metaLookup, repo repoRefLookup,
	live map[string]bool, cfg selectConfig) ([]Candidate, drops) {
	var out []Candidate
	var d drops
	for _, rs := range recent {
		if cfg.limit > 0 && len(out) >= cfg.limit {
			break
		}
		if !cfg.all && live[rs.SessionID] {
			d.live++
			continue
		}
		info, hasInfo := meta(rs.SessionID)
		ref, hasRef := repo(rs.SessionID)

		// cwd: prefer the transcript's exact cwd, fall back to the repo worktree
		// root. Without either we cannot cd, so `claude --resume` would fail —
		// drop it rather than list a dead-end.
		cwd := ""
		if hasInfo {
			cwd = info.Cwd
		}
		if cwd == "" && hasRef {
			cwd = ref.Root
		}
		if cwd == "" {
			d.noCwd++
			continue
		}

		repoLabel, branch := "", ""
		if hasRef {
			repoLabel = ref.Remote
			if repoLabel == "" {
				repoLabel = ref.Root
			}
			branch = ref.Branch
		}
		if branch == "" && hasInfo {
			branch = info.Branch
		}

		if cfg.hereKey != "" && repoKey(ref, hasRef, cwd) != cfg.hereKey {
			d.elsewhere++
			continue
		}

		topic := ""
		if hasInfo {
			topic = info.Title
			if topic == "" {
				topic = firstLine(info.Ask)
			}
		}
		out = append(out, Candidate{
			SessionID: rs.SessionID,
			Cwd:       cwd,
			Repo:      repoLabel,
			Branch:    branch,
			Topic:     topic,
			LastTS:    rs.LastTS,
		})
	}
	return out, d
}

// repoKey is a session's repo identity for --here comparison: its normalized
// remote if it has one, else its worktree root, else the raw cwd.
func repoKey(ref db.RepoRef, hasRef bool, cwd string) string {
	if hasRef {
		if ref.Remote != "" {
			return ref.Remote
		}
		if ref.Root != "" {
			return ref.Root
		}
	}
	return cwd
}

// ---------------------------------------------------------------------------
// --list
// ---------------------------------------------------------------------------

type listConfig struct {
	dbPath, tdir, sdir string
	limit              int
	all, here          bool
}

func runList(out, errW io.Writer, cfg listConfig) error {
	database, err := db.Open(cfg.dbPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer database.Close()

	// Over-fetch: filtering (live/no-cwd/here) thins the list, so pull more than
	// the display cap to still fill it.
	fetch := max(cfg.limit*4, 40)
	recent, err := database.RecentSessions(fetch)
	if err != nil {
		return fmt.Errorf("recent sessions: %w", err)
	}
	repos, err := database.LoadSessionRepos()
	if err != nil {
		return fmt.Errorf("load session repos: %w", err)
	}

	// Liveness is best-effort: if the first-party dir is unreadable, treat all as
	// dead rather than failing the picker.
	live := map[string]bool{}
	if liveSessions, err := clauded.ReadLive(cfg.sdir); err == nil {
		for id := range liveSessions {
			live[id] = true
		}
	}

	hereKey := ""
	if cfg.here {
		wd, _ := os.Getwd()
		remote, root, _, ok := git.Resolve(wd)
		switch {
		case ok && remote != "":
			hereKey = remote
		case ok && root != "":
			hereKey = root
		default:
			hereKey = wd
		}
	}

	meta := func(sid string) (transcript.Info, bool) { return transcript.Meta(cfg.tdir, sid) }
	repo := func(sid string) (db.RepoRef, bool) {
		refs := repos[sid]
		if len(refs) == 0 {
			return db.RepoRef{}, false
		}
		return refs[0], true
	}

	cands, d := selectCandidates(recent, meta, repo, live,
		selectConfig{limit: cfg.limit, all: cfg.all, hereKey: hereKey})

	writeRows(out, cands, Now())
	reportDrops(errW, len(cands), d, cfg.all, cfg.here)
	return nil
}

// writeRows emits one tab-delimited row per candidate: id \t cwd \t display.
// The picker shows only the third field (fzf --with-nth=3..) and feeds the first
// to `--show`. Newest first (candidates are already ordered).
func writeRows(out io.Writer, cands []Candidate, now time.Time) {
	for _, c := range cands {
		fmt.Fprintf(out, "%s\t%s\t%s\n", c.SessionID, c.Cwd, displayLine(c, now))
	}
}

// displayLine is the human label: "<age>  <repo>@<branch>  <topic>".
func displayLine(c Candidate, now time.Time) string {
	loc := c.Repo
	if loc == "" {
		loc = "(no repo)"
	}
	if c.Branch != "" {
		loc += "@" + c.Branch
	}
	topic := c.Topic
	if topic == "" {
		topic = "(untitled)"
	}
	return fmt.Sprintf("%-4s %-38s %s", relAge(c.LastTS, now), loc, truncate(topic, 72))
}

func reportDrops(errW io.Writer, kept int, d drops, all, here bool) {
	if kept == 0 && d == (drops{}) {
		fmt.Fprintln(errW, "no sessions found")
		return
	}
	var notes []string
	if d.live > 0 && !all {
		notes = append(notes, fmt.Sprintf("%d live (use --all)", d.live))
	}
	if d.noCwd > 0 {
		notes = append(notes, fmt.Sprintf("%d without a resumable directory", d.noCwd))
	}
	if d.elsewhere > 0 && here {
		notes = append(notes, fmt.Sprintf("%d in other repos", d.elsewhere))
	}
	if len(notes) > 0 {
		fmt.Fprintf(errW, "resume: %d shown; skipped %s\n", kept, strings.Join(notes, ", "))
	}
}

// ---------------------------------------------------------------------------
// --show (fzf preview)
// ---------------------------------------------------------------------------

// previewTurns caps how many trailing conversation turns the preview shows.
const previewTurns = 6

func runShow(out io.Writer, tdir, sid string) error {
	info, ok := transcript.Meta(tdir, sid)
	if !ok {
		fmt.Fprintf(out, "no transcript for %s\n", sid)
		return nil
	}
	writeShow(out, info, Now())
	return nil
}

func writeShow(out io.Writer, info transcript.Info, now time.Time) {
	topic := info.Title
	if topic == "" {
		topic = firstLine(info.Ask)
	}
	if topic == "" {
		topic = "(untitled session)"
	}
	fmt.Fprintln(out, topic)

	loc := info.Branch
	if loc != "" {
		loc = "@" + loc
	}
	fmt.Fprintf(out, "%s%s\n", info.Cwd, loc)
	if len(info.Turns) > 0 {
		if last := info.Turns[len(info.Turns)-1].At; !last.IsZero() {
			fmt.Fprintf(out, "last active %s\n", relAge(last.UnixMilli(), now))
		}
	}
	fmt.Fprintln(out, strings.Repeat("─", 40))

	if info.Ask != "" {
		fmt.Fprintf(out, "» %s\n", collapse(info.Ask))
	}

	turns := info.Turns
	if len(turns) > previewTurns {
		fmt.Fprintf(out, "\n… %d earlier turns …\n", len(turns)-previewTurns)
		turns = turns[len(turns)-previewTurns:]
	}
	for _, t := range turns {
		who := "claude"
		if t.Role == "user" {
			who = "you"
		}
		fmt.Fprintf(out, "\n%s> %s\n", who, truncate(collapse(t.Text), 400))
	}
}

// ---------------------------------------------------------------------------
// --init zsh + bare help
// ---------------------------------------------------------------------------

// zshInit is the shell integration printed by `--init zsh`. The command is a
// FUNCTION (not an alias/script) so its `cd` runs in the interactive shell and
// persists after Claude exits. fzf runs here, in the shell, where the user wants
// it; the Go side only serves --list rows and the --show preview.
const zshInit = `# claude-status: fuzzy-pick a recent (dead) Claude session and resume it.
# This is a FUNCTION on purpose: cd runs in your shell, so when Claude exits you
# are left in the resurrected session's project directory.
claude-resume() {
  emulate -L zsh
  local line
  line=$(command claude-status resume --list "$@" \
    | fzf --delimiter=$'\t' --with-nth='3..' --no-multi --reverse --height='80%' \
          --header='resurrect a Claude session' \
          --preview='claude-status resume --show {1}' \
          --preview-window='right,55%,wrap') || return   # Esc/Ctrl-C -> no-op
  [[ -n $line ]] || return
  local id=${line%%$'\t'*}
  local cwd=${${line#*$'\t'}%%$'\t'*}
  cd -- "$cwd" && command claude --resume "$id"   # resume in the session's dir
}

# 'cr' is a convenience alias — but be KIND: only define it if nothing named 'cr'
# already exists, so we never clobber a pre-existing alias/function/command.
command -v cr >/dev/null 2>&1 || alias cr='claude-resume'
`

func runInit(out io.Writer, shell string) error {
	switch shell {
	case "zsh":
		_, err := io.WriteString(out, zshInit)
		return err
	default:
		return fmt.Errorf("resume --init: unsupported shell %q (only: zsh)", shell)
	}
}

// printHelp is the bare-`resume` output: eval-safe `#` comments. Visible if run
// directly in a terminal, a clean no-op if someone `eval`s it by mistake.
func printHelp(out io.Writer) {
	fmt.Fprint(out, `# claude-status resume runs inside a zsh function so it can cd into the session's
# directory before resuming. Install it once with:
#   claude-status install         # adds claude-resume + a guarded 'cr' alias to ~/.zshrc
# or add this line to ~/.zshrc yourself:
#   eval "$(claude-status resume --init zsh)"
# then run:  claude-resume         (or the 'cr' alias)
`)
}

// ---------------------------------------------------------------------------
// small text helpers
// ---------------------------------------------------------------------------

// relAge renders (now - ts) compactly: "just now", "5m", "3h", "2d", "6w".
func relAge(tsMS int64, now time.Time) string {
	d := now.Sub(time.UnixMilli(tsMS))
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	default:
		return fmt.Sprintf("%dw", int(d.Hours()/(24*7)))
	}
}

// firstLine returns the first non-empty line of s, trimmed.
func firstLine(s string) string {
	for ln := range strings.SplitSeq(s, "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			return t
		}
	}
	return ""
}

// collapse flattens whitespace/newlines to single spaces for one-line rendering.
func collapse(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// truncate shortens s to at most n runes, appending an ellipsis when cut.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}
