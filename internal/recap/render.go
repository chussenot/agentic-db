package recap

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// Markdown renders a Digest as a compact standup-ready document. It is
// dual-use: readable as-is, and clean to pipe into an LLM as context (fewer
// tokens than verbose JSON). Sessions are grouped by project in active-time
// order; the top-N detail is followed by a one-line summary of the remainder.
func Markdown(w io.Writer, d Digest) {
	fmt.Fprintf(w, "# Claude recap — %s → %s\n\n",
		d.From.Format("Mon Jan 2 15:04"), d.To.Format("Mon Jan 2 15:04"))
	fmt.Fprintf(w, "**%d session%s · %s active · %d prompts · %d turns · %d project%s**\n",
		d.Totals.Sessions, plural(d.Totals.Sessions), fmtDur(d.Totals.Active),
		d.Totals.Questions, d.Totals.Turns, len(d.Projects), plural(len(d.Projects)))

	if len(d.Sessions) == 0 {
		fmt.Fprintln(w, "\n_No Claude sessions in this window._")
		return
	}

	fmt.Fprintln(w, "\n## What I worked on")
	for _, p := range d.Projects {
		detailed := sessionsIn(d.Sessions, p.Name)
		if len(detailed) == 0 {
			continue // this project only appears among the summarized remainder
		}
		fmt.Fprintf(w, "\n### %s — %s\n", p.Name, fmtDur(p.Active))
		for _, s := range detailed {
			topic := s.Topic
			if topic == "" {
				topic = "(untitled session)"
			}
			branch := ""
			if s.Branch != "" {
				branch = fmt.Sprintf(" (%s)", s.Branch)
			}
			fmt.Fprintf(w, "- **%s**%s — %s, %d turns, %d prompts\n",
				topic, branch, fmtDur(s.Active), s.Turns, s.Questions)
			if s.Ask != "" {
				fmt.Fprintf(w, "  ↳ %q\n", oneLine(s.Ask))
			}
		}
	}
	if d.MoreSessions > 0 {
		fmt.Fprintf(w, "\n_+%d more session%s across %d project%s (below the detail cutoff)._\n",
			d.MoreSessions, plural(d.MoreSessions), d.MoreProjects, plural(d.MoreProjects))
	}

	if len(d.Streaks) > 0 {
		fmt.Fprintln(w, "\n## Focus streaks")
		for _, s := range d.Streaks {
			fmt.Fprintf(w, "- %s — %s → %s\n", fmtDur(s.Dur),
				s.Start.Format("Mon 15:04"), s.End.Format("15:04"))
		}
	}
}

// MetricsMarkdown renders the deterministic "## Metrics" and "## Project metrics"
// sections. They are kept separate from Markdown so they can be appended to the
// daily doc verbatim (exact figures the LLM never touches) or omitted from the
// digest fed to the LLM. All values are over the recap window; "total" is union
// wall-clock (the heartbeat model), and the per-session figures summarize the
// individual session active times.
func MetricsMarkdown(w io.Writer, d Digest) {
	fmt.Fprintln(w, "## Metrics")
	fmt.Fprintln(w)
	n := d.Totals.Sessions
	if n == 0 {
		fmt.Fprintln(w, "- No activity in this window.")
		return
	}
	fmt.Fprintf(w, "- Total time clauding: %s (wall-clock)\n", fmtDur(d.Totals.Active))
	fmt.Fprintf(w, "- Per session: min %s · avg %s · max %s (%d session%s)\n",
		fmtDur(d.Totals.MinActive), fmtDur(d.Totals.AvgActive), fmtDur(d.Totals.MaxActive),
		n, plural(n))
	fmt.Fprintf(w, "- Prompts sent: %d\n", d.Totals.Prompts)
	fmt.Fprintf(w, "- Permission prompts: %d\n", d.Totals.Questions)
	fmt.Fprintf(w, "- Work vs wait: %s working · %s waiting on me · %s on permission\n",
		fmtDur(d.Totals.Working), fmtDur(d.Totals.WaitingUser), fmtDur(d.Totals.WaitingPermission))

	projectMetricsMarkdown(w, d.Projects)
}

// projectMetricsMarkdown renders per-project git context (remote + window commit
// count), one line per project that resolves to a git repo (projects sorted by
// active desc). Projects whose cwd isn't a live work tree — a removed temp
// worktree, a non-repo directory like $HOME — are dropped rather than shown as an
// empty "(no remote) · 0" line. The branch is shown only when it isn't
// main/master (i.e. the count came off a feature branch — worth flagging).
func projectMetricsMarkdown(w io.Writer, projects []Project) {
	var repos []Project
	for _, p := range projects {
		if p.HasRepo {
			repos = append(repos, p)
		}
	}
	if len(repos) == 0 {
		return
	}
	fmt.Fprintln(w, "\n## Project metrics")
	fmt.Fprintln(w)
	for _, p := range repos {
		remote := p.Remote
		if remote == "" {
			remote = "(no remote)"
		}
		branch := ""
		if p.Branch != "" && p.Branch != "main" && p.Branch != "master" {
			branch = fmt.Sprintf(" (%s)", p.Branch)
		}
		fmt.Fprintf(w, "- %s — %s · %d commit%s%s\n",
			p.Name, remote, p.Commits, plural(p.Commits), branch)
	}
}

// JSON writes the Digest as indented JSON for programmatic consumers.
func JSON(w io.Writer, d Digest) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(d)
}

func sessionsIn(ss []Session, project string) []Session {
	var out []Session
	for _, s := range ss {
		if s.Project == project {
			out = append(out, s)
		}
	}
	return out
}

func fmtDur(d time.Duration) string {
	s := int(d.Seconds())
	h, r := s/3600, s%3600
	m, sec := r/60, r%60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%02dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm", m)
	default:
		return fmt.Sprintf("%ds", sec)
	}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
