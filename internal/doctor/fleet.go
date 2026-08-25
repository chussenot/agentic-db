// This file implements `claude-status fleet` — the read-only view of background
// agents and who they belong to.
//
// It exists because background awareness has no renderer yet, on purpose: the
// tracking had to become correct before any glyph or tile started keying on it. So
// this is where the new data is judged. The column pair to look at is
// OWN_IDLE / FLEET_IDLE: the first is what the bar currently decays on, the second
// is what it would decay on if a session's fleet counted. A session showing
// "19h / 30s" is the bug the whole exercise is about, made visible without changing
// a single pixel.
package doctor

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/mrzor/claude-status/internal/clauded"
	"github.com/mrzor/claude-status/internal/db"
	"github.com/mrzor/claude-status/internal/fleet"
	"github.com/mrzor/claude-status/internal/herdr"
)

// RunFleet executes the fleet subcommand. args is os.Args[2:].
func RunFleet(args []string) error {
	fs := flag.NewFlagSet("fleet", flag.ContinueOnError)
	path := fs.String("db", db.DefaultDBPath(), "path to the SQLite database")
	sessionsDir := fs.String("sessions-dir", clauded.DefaultDir(), "Claude Code sessions directory")
	jobsDir := fs.String("jobs-dir", fleet.DefaultJobsDir(), "Claude Code background-jobs directory")
	rosterPath := fs.String("roster", fleet.DefaultRosterPath(), "background supervisor roster path")
	herdrSession := fs.String("herdr-session", "", "herdr session.json for herd labels (empty uses herdr's own default)")
	asJSON := fs.Bool("json", false, "emit JSON instead of tables")
	crossCheck := fs.Bool("cross-check", false, "also run `claude agents --json` and diff it against our view")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return runFleet(os.Stdout, fleetOpts{
		dbPath:      *path,
		sessionsDir: *sessionsDir,
		jobsDir:     *jobsDir,
		rosterPath:  *rosterPath,
		herdrPath:   *herdrSession,
		asJSON:      *asJSON,
		crossCheck:  *crossCheck,
	})
}

type fleetOpts struct {
	dbPath      string
	sessionsDir string
	jobsDir     string
	rosterPath  string
	herdrPath   string
	// herdrList, when set, replaces herdr's roster call. The seam exists so this
	// command's tests never depend on a live herdr session on the machine running
	// them (herdr IS installed on the development machine, so without it every test
	// through buildFleetView would spawn a process and read real state) — the same
	// reason execCommand is a var in this file.
	herdrList  func(context.Context) ([]herdr.Agent, error)
	asJSON     bool
	crossCheck bool
}

// fleetView is everything the subcommand computes, and the shape of --json.
// Nothing here carries a prompt or a credential: the job reader never decodes
// them, and `detail`/`needs` are the agents' own short status lines.
type fleetView struct {
	Now      time.Time       `json:"now"`
	Agents   []agentView     `json:"agents"`
	Fleets   []rollupView    `json:"fleets"`
	Groups   []groupView     `json:"groups"`
	Roster   *rosterView     `json:"roster,omitempty"`
	Warnings []string        `json:"warnings,omitempty"`
	Diff     *crossCheckDiff `json:"crossCheck,omitempty"`
	children map[string]fleet.Child
}

type agentView struct {
	SessionID   string  `json:"sessionId"`
	Name        string  `json:"name"`
	JobID       string  `json:"jobId"`
	PID         int     `json:"pid"`
	Agent       string  `json:"agent,omitempty"`
	Demand      string  `json:"demand"`
	Lifecycle   string  `json:"lifecycle,omitempty"`
	Tempo       string  `json:"tempo,omitempty"`
	InFlight    int     `json:"inFlight"`
	Queued      int     `json:"queued"`
	Tokens      int     `json:"tokens,omitempty"`
	Owner       string  `json:"owner,omitempty"`
	OwnerSource string  `json:"ownerSource"`
	Console     string  `json:"console,omitempty"`
	IdleSeconds float64 `json:"idleSeconds"`
	Needs       string  `json:"needs,omitempty"`
	Detail      string  `json:"detail,omitempty"`
	Worktree    string  `json:"worktreeBranch,omitempty"`
	// InDB is false when a live background agent has no row of ours — i.e. we are
	// blind to it, which is a tracking gap worth seeing.
	InDB bool `json:"inDb"`
}

type rollupView struct {
	SessionID string `json:"sessionId"`
	Name      string `json:"name,omitempty"`
	Window    *int64 `json:"windowId,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Children  int    `json:"children"`
	Live      int    `json:"live"`
	Awaiting  int    `json:"awaitingHuman"`
	Working   int    `json:"working"`
	Blocked   int    `json:"blocked"`
	Idle      int    `json:"idle"`
	Done      int    `json:"done"`
	Worst     string `json:"worst"`
	Neediest  string `json:"neediest,omitempty"`
	Needs     string `json:"needs,omitempty"`
	// OwnIdleSeconds is what the bar decays on today (last_talk_ts);
	// FleetIdleSeconds is what it would decay on if the fleet counted.
	OwnIdleSeconds   float64 `json:"ownIdleSeconds"`
	FleetIdleSeconds float64 `json:"fleetIdleSeconds"`
}

// groupView is one PROVIDER GROUP: the row shape the tile renders, printed here so
// the two substrates can be read side by side and compared. A claude-code group is
// one owner session's children; a herdr group is one workspace's agents.
type groupView struct {
	Provider string `json:"provider"`
	// Key is the group's stable identity within its provider — a herdr workspace id,
	// or an owning session id. Never a pane id and never one of the churning agent
	// session ids.
	Key      string `json:"key"`
	Label    string `json:"label"`
	Total    int    `json:"total"`
	Live     int    `json:"live"`
	Awaiting int    `json:"awaitingHuman"`
	Working  int    `json:"working"`
	Blocked  int    `json:"blocked"`
	Idle     int    `json:"idle"`
	Unknown  int    `json:"unknown"`
	Done     int    `json:"done"`
	Worst    string `json:"worst"`
	Neediest string `json:"neediest,omitempty"`
	Needs    string `json:"needs,omitempty"`
	// IdleSeconds is the GROUP's age: how long since anything in it moved. This is
	// the churn-resistant number — the max over members, ratcheted — and reading it
	// against the members' own ages is how you see the churn being absorbed.
	IdleSeconds float64      `json:"idleSeconds"`
	Members     []memberView `json:"members"`
}

// memberView is one agent inside a group.
type memberView struct {
	Name string `json:"name,omitempty"`
	// Ref is the provider's lookup handle (a herdr pane id, a job id) and NOT an
	// identity: a pane id churns under a live agent.
	Ref string `json:"ref,omitempty"`
	// SessionID is our row for it, when the join found one. Empty is normal: a herdr
	// pane between two ticks hosts no Claude session and is still an agent.
	SessionID   string  `json:"sessionId,omitempty"`
	Demand      string  `json:"demand"`
	IdleSeconds float64 `json:"idleSeconds"`
	Needs       string  `json:"needs,omitempty"`
}

type rosterView struct {
	SupervisorPID int    `json:"supervisorPid"`
	Workers       int    `json:"workers"`
	UpdatedAt     string `json:"updatedAt,omitempty"`
	// FleetRoot is the session the supervisor's environment points at — one /proc
	// read that answers "whose fleet is this" for the whole fleet.
	FleetRoot string `json:"fleetRoot,omitempty"`
}

// crossCheckDiff reports disagreement with Claude Code's own view. Rows we have
// that it does not are leaks; rows it has that we do not are blind spots.
type crossCheckDiff struct {
	Ours   int      `json:"ours"`
	Theirs int      `json:"theirs"`
	Leaked []string `json:"leaked,omitempty"`
	Missed []string `json:"missed,omitempty"`
	Error  string   `json:"error,omitempty"`
}

func runFleet(w io.Writer, o fleetOpts) error {
	view, err := buildFleetView(o)
	if err != nil {
		return err
	}
	if o.asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(view)
	}
	printFleetView(w, view)
	return nil
}

// buildFleetView reads all three sources and joins them. Read failures degrade to
// warnings rather than errors: a machine that has never run a background agent has
// no jobs directory and no roster, and must still print a useful page.
func buildFleetView(o fleetOpts) (*fleetView, error) {
	d, err := db.Open(o.dbPath)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	defer d.Close()

	rows, err := d.LoadLive()
	if err != nil {
		return nil, fmt.Errorf("load sessions: %w", err)
	}
	view := &fleetView{Now: db.Now()}

	live, err := clauded.ReadLive(o.sessionsDir)
	if err != nil {
		view.Warnings = append(view.Warnings, fmt.Sprintf("first-party status unreadable (%s): %v", o.sessionsDir, err))
		live = map[string]clauded.Session{}
	}

	jobIDs := make([]string, 0, len(live))
	for _, s := range live {
		if s.JobID != "" {
			jobIDs = append(jobIDs, s.JobID)
		}
	}
	jobs := fleet.NewReader(o.jobsDir).ReadJobs(jobIDs)
	if len(jobIDs) > 0 && len(jobs) == 0 {
		view.Warnings = append(view.Warnings,
			fmt.Sprintf("%d background agent(s) live but no job record readable under %s", len(jobIDs), o.jobsDir))
	}

	resolver := fleet.NewOwnerResolver()
	children := fleet.Attribute(rows, live, jobs, resolver, view.Now)
	rollups := fleet.Rollups(children)
	view.children = children

	rowByID := make(map[string]db.Session, len(rows))
	for _, r := range rows {
		rowByID[r.SessionID] = r
	}

	for id, c := range children {
		_, inDB := rowByID[id]
		av := agentView{
			SessionID: id, Name: c.Name, JobID: c.JobID, PID: c.PID,
			Demand: c.Demand.String(), Owner: c.OwnerSessionID,
			OwnerSource: string(c.OwnerSource), Console: c.ConsoleSessionID,
			Needs: c.Needs, Detail: c.Detail, InDB: inDB,
			IdleSeconds: since(view.Now, c.Activity),
		}
		if c.HasJob {
			av.Lifecycle = string(c.Job.Lifecycle)
			av.Tempo = string(c.Job.Tempo)
			av.InFlight = c.Job.InFlight.Tasks
			av.Queued = c.Job.InFlight.Queued
			av.Tokens = c.Job.Tokens
			av.Agent = c.Job.Agent
			av.Worktree = c.Job.WorktreeBranch
		}
		view.Agents = append(view.Agents, av)
	}
	sort.Slice(view.Agents, func(i, j int) bool {
		if view.Agents[i].Demand != view.Agents[j].Demand {
			return view.Agents[i].Demand < view.Agents[j].Demand
		}
		return view.Agents[i].Name < view.Agents[j].Name
	})

	for id, r := range rollups {
		row := rowByID[id]
		rv := rollupView{
			SessionID: id, Children: r.Total, Live: r.Live,
			Awaiting: r.AwaitingHuman, Working: r.Working, Blocked: r.Blocked,
			Idle: r.Idle, Done: r.Done, Worst: r.Worst.String(),
			Neediest: r.Neediest.Name, Needs: r.Neediest.Needs,
			OwnIdleSeconds:   ownIdle(view.Now, row),
			FleetIdleSeconds: since(view.Now, fleet.EffectiveActivity(row, live[id], r)),
		}
		if f, ok := live[id]; ok {
			rv.Name, rv.Kind = f.Name, string(f.Kind)
		}
		if row.WindowID.Valid {
			win := row.WindowID.Int64
			rv.Window = &win
		}
		view.Fleets = append(view.Fleets, rv)
	}
	sort.Slice(view.Fleets, func(i, j int) bool {
		if view.Fleets[i].Worst != view.Fleets[j].Worst {
			return view.Fleets[i].Worst < view.Fleets[j].Worst
		}
		return view.Fleets[i].SessionID < view.Fleets[j].SessionID
	})

	if r, ok := fleet.ReadRoster(o.rosterPath); ok {
		// A PARSEABLE ROSTER IS NOT A LIVE ONE. The file survives its supervisor
		// (see fleet.Roster.SupervisorAlive for the measurement), so reporting one
		// on the strength of the parse alone is how this page came to announce a
		// supervisor that had been dead for 4.8 days.
		//
		// A dead supervisor gets a WARNING rather than silence. Dropping the line
		// with no trace would leave a human reading this page unable to tell "no
		// roster on this machine" from "a roster we chose to disbelieve" — and this
		// command exists to be read while debugging exactly that kind of question.
		// The warning also carries into --json, so no consumer sees the stale pid
		// presented as current.
		if !r.SupervisorAlive() {
			view.Warnings = append(view.Warnings, staleRoster(r))
		} else {
			rv := &rosterView{SupervisorPID: r.SupervisorPID, Workers: len(r.Workers)}
			if !r.UpdatedAt.IsZero() {
				rv.UpdatedAt = r.UpdatedAt.Format(time.RFC3339)
			}
			// The supervisor's environment names the session that caused the pool
			// to exist, so this resolves the fleet root for every worker at once.
			if sp, ok := resolver.Spawn(r.SupervisorPID, ""); ok && sp.SpawnerPID > 0 {
				for id, s := range live {
					if s.PID == sp.SpawnerPID {
						rv.FleetRoot = id
					}
				}
			}
			view.Roster = rv
		}
	}

	view.Groups = buildGroups(o, rows, live, jobs, resolver, view.Now, view)

	if o.crossCheck {
		view.Diff = crossCheckAgents(children)
	}
	return view, nil
}

// buildGroups runs both providers over one Input and flattens the result.
//
// This is the section to read when judging the herdr work: it is the exact shape the
// tile renders, from the exact code path the daemon runs, so a herd that looks wrong
// on the bar looks wrong here too — without a daemon, a niri window or a pixel.
//
// The claude-code provider re-runs Attribute over inputs this function already
// attributed once. That is deliberate and costs nothing measurable in a one-shot CLI:
// it means this page exercises the provider rather than a hand-mirrored copy of it,
// which is the mistake doctor.effectiveState made against daemon.firstPartyState.
func buildGroups(o fleetOpts, rows []db.Session, live map[string]clauded.Session, jobs map[string]fleet.Job,
	resolver *fleet.OwnerResolver, now time.Time, view *fleetView) []groupView {
	in := fleet.Input{Rows: rows, Live: live, Jobs: jobs, Spawn: resolver, Now: now}
	ctx := context.Background()

	herd := fleet.NewHerdrProvider(o.herdrPath)
	if o.herdrList != nil {
		herd = fleet.NewHerdrProviderFrom(o.herdrList, o.herdrPath)
	}
	var groups []fleet.Group
	for _, p := range []fleet.Provider{fleet.NewClaudeCodeProvider(), herd} {
		gs, err := p.Groups(ctx, in)
		if err != nil {
			// A herdr-side refusal names something actionable ("server_not_running"
			// -> start herdr), which is exactly what this page is for. The daemon
			// swallows the same error and shows no herd.
			view.Warnings = append(view.Warnings, fmt.Sprintf("provider %s: %v", p.ID(), err))
			continue
		}
		groups = append(groups, gs...)
	}
	fleet.SortGroups(groups)

	out := make([]groupView, 0, len(groups))
	for _, g := range groups {
		sum := g.Summarize()
		gv := groupView{
			Provider: string(g.Key.Provider), Key: g.Key.Key, Label: g.Label,
			Total: sum.Total, Live: sum.Live, Awaiting: sum.Awaiting,
			Working: sum.Working, Blocked: sum.Blocked, Idle: sum.Idle,
			Unknown: sum.Unknown, Done: sum.Done, Worst: sum.Worst.String(),
			Neediest: sum.Neediest, Needs: sum.Needs,
			IdleSeconds: -1,
		}
		if sum.NewestActivity > 0 {
			gv.IdleSeconds = since(now, time.UnixMilli(sum.NewestActivity))
		}
		members := make([]fleet.Member, len(g.Members))
		copy(members, g.Members)
		fleet.SortMembers(members)
		for _, m := range members {
			gv.Members = append(gv.Members, memberView{
				Name: m.Name, Ref: m.Ref, SessionID: m.SessionID,
				Demand: m.Demand.String(), Needs: m.Needs,
				IdleSeconds: since(now, m.Activity),
			})
		}
		out = append(out, gv)
	}
	return out
}

// staleRoster is the one line a dead supervisor gets instead of a supervisor line.
// It names the pid so the reader can confirm the corpse themselves, and the roster's
// own updatedAt so "how stale" is answered without a second command.
func staleRoster(r fleet.Roster) string {
	msg := fmt.Sprintf("stale roster ignored: supervisor pid %d is gone (%d worker(s) recorded)",
		r.SupervisorPID, len(r.Workers))
	if !r.UpdatedAt.IsZero() {
		msg += ", last written " + r.UpdatedAt.Format(time.RFC3339)
	}
	return msg
}

// ownIdle is how long since the session itself last talked — the number the bar
// decays on today.
func ownIdle(now time.Time, row db.Session) float64 {
	if !row.LastTalkTS.Valid {
		return -1
	}
	return since(now, time.UnixMilli(row.LastTalkTS.Int64))
}

func since(now, t time.Time) float64 {
	if t.IsZero() {
		return -1
	}
	return now.Sub(t).Seconds()
}

func printFleetView(w io.Writer, v *fleetView) {
	fmt.Fprintf(w, "claude-status fleet\n")
	for _, warn := range v.Warnings {
		fmt.Fprintf(w, "warning: %s\n", warn)
	}
	if v.Roster != nil {
		root := v.Roster.FleetRoot
		if root == "" {
			root = "unresolved"
		} else {
			root = shortID(root)
		}
		fmt.Fprintf(w, "supervisor: pid %d, %d worker(s), fleet root %s\n",
			v.Roster.SupervisorPID, v.Roster.Workers, root)
	}

	fmt.Fprintf(w, "\nbackground agents: %d\n", len(v.Agents))
	if len(v.Agents) > 0 {
		tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "  NAME\tJOB\tDEMAND\tLIFECYCLE\tTEMPO\tFAN\tIDLE\tOWNER\tSRC\tCONSOLE\tDB\tNEEDS")
		for _, a := range v.Agents {
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
				dash(a.Name), dash(a.JobID), a.Demand, dash(a.Lifecycle), dash(a.Tempo),
				a.InFlight, ago(a.IdleSeconds), shortOrDash(a.Owner), a.OwnerSource,
				shortOrDash(a.Console), yesNo(a.InDB), clip(dash(a.Needs), 44))
		}
		tw.Flush()
	}

	fmt.Fprintf(w, "\nfleets by session: %d\n", len(v.Fleets))
	if len(v.Fleets) > 0 {
		tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "  SESSION\tNAME\tWIN\tKIND\tKIDS\tLIVE\tAWAIT\tWORK\tBLOCK\tWORST\tOWN_IDLE\tFLEET_IDLE\tNEEDIEST")
		for _, f := range v.Fleets {
			win := "NULL"
			if f.Window != nil {
				win = fmt.Sprintf("%d", *f.Window)
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%d\t%d\t%d\t%d\t%d\t%s\t%s\t%s\t%s\n",
				shortID(f.SessionID), dash(f.Name), win, dash(f.Kind),
				f.Children, f.Live, f.Awaiting, f.Working, f.Blocked, f.Worst,
				ago(f.OwnIdleSeconds), ago(f.FleetIdleSeconds), dash(f.Neediest))
		}
		tw.Flush()
	}

	fmt.Fprintf(w, "\ngroups: %d\n", len(v.Groups))
	if len(v.Groups) > 0 {
		tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "  PROVIDER\tKEY\tLABEL\tAGENTS\tLIVE\tAWAIT\tWORK\tBLOCK\tIDLE\tUNKN\tDONE\tWORST\tGROUP_IDLE\tNEEDIEST")
		for _, g := range v.Groups {
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%s\t%s\t%s\n",
				g.Provider, g.Key, dash(g.Label), g.Total, g.Live, g.Awaiting, g.Working,
				g.Blocked, g.Idle, g.Unknown, g.Done, g.Worst, ago(g.IdleSeconds), dash(g.Neediest))
		}
		tw.Flush()
		// The members are printed under their group because the GROUP's age and its
		// members' ages are the comparison this whole exercise is about: a herd whose
		// members are all seconds old while the group reads minutes means the join is
		// broken, and a herd reading fresh while every member reads "never" means the
		// ratchet is carrying a mark nothing supports any more.
		for _, g := range v.Groups {
			fmt.Fprintf(w, "  %s/%s (%s), group idle %s:\n", g.Provider, g.Key, dash(g.Label), ago(g.IdleSeconds))
			tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "    AGENT\tREF\tSESSION\tDEMAND\tIDLE\tNEEDS")
			for _, m := range g.Members {
				fmt.Fprintf(tw, "    %s\t%s\t%s\t%s\t%s\t%s\n",
					dash(m.Name), dash(m.Ref), shortOrDash(m.SessionID), m.Demand,
					ago(m.IdleSeconds), clip(dash(m.Needs), 44))
			}
			tw.Flush()
		}
	}

	if d := v.Diff; d != nil {
		fmt.Fprintf(w, "\ncross-check against `claude agents --json`: ours=%d theirs=%d\n", d.Ours, d.Theirs)
		if d.Error != "" {
			fmt.Fprintf(w, "  error: %s\n", d.Error)
		}
		for _, id := range d.Leaked {
			fmt.Fprintf(w, "  LEAKED (we track it, Claude does not): %s\n", shortID(id))
		}
		for _, id := range d.Missed {
			fmt.Fprintf(w, "  MISSED (Claude has it, we do not): %s\n", shortID(id))
		}
	}
}

// execCommand is the seam for the cross-check subprocess, so tests never spawn a
// node process.
var execCommand = exec.Command

// crossCheckAgents compares our background set against `claude agents --json`.
// That command is authoritative but costs a node startup (~0.4s) and needs `claude`
// on PATH, which is why it is opt-in here and never used by the daemon.
func crossCheckAgents(children map[string]fleet.Child) *crossCheckDiff {
	diff := &crossCheckDiff{Ours: len(children)}
	cmd := execCommand("claude", "agents", "--json")
	out, err := cmd.Output()
	if err != nil {
		diff.Error = fmt.Sprintf("running `claude agents --json`: %v", err)
		return diff
	}
	var listed []struct {
		SessionID string `json:"sessionId"`
		Kind      string `json:"kind"`
	}
	if err := json.Unmarshal(out, &listed); err != nil {
		diff.Error = fmt.Sprintf("decoding `claude agents --json`: %v", err)
		return diff
	}
	theirs := map[string]bool{}
	for _, l := range listed {
		if l.Kind == "background" || l.Kind == "bg" {
			theirs[l.SessionID] = true
		}
	}
	diff.Theirs = len(theirs)
	for id := range children {
		if !theirs[id] {
			diff.Leaked = append(diff.Leaked, id)
		}
	}
	for id := range theirs {
		if _, ok := children[id]; !ok {
			diff.Missed = append(diff.Missed, id)
		}
	}
	sort.Strings(diff.Leaked)
	sort.Strings(diff.Missed)
	return diff
}

func dash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func shortOrDash(id string) string {
	if id == "" {
		return "—"
	}
	return shortID(id)
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "NO"
}

// ago renders a duration in seconds compactly; -1 means "never".
func ago(secs float64) string {
	if secs < 0 {
		return "never"
	}
	return time.Duration(secs * float64(time.Second)).Truncate(time.Second).String()
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
