// Package herdr reads herdr's live agent roster.
//
// herdr is a terminal workspace manager (a multiplexer) whose vocabulary is
// workspace -> tab -> pane -> agent, where "agent" is not an object but an ASPECT
// of a pane: its current occupant. It is the substrate our agent groups now run
// on, and it is the only place that knows which pane holds which agent, because
// every pane of a herdr session shares ONE terminal window — so the window/title
// route the rest of this codebase uses cannot tell four agents apart.
//
// The roster comes from `herdr agent list`, one Unix-socket roundtrip through the
// CLI. There is NO --json flag: JSON is the default output of every herdr
// subcommand (only `herdr status` prints something else), and the envelope wraps
// the payload twice — `{"id":"cli:agent:list","result":{"agents":[…]}}`. Reading
// `.agents` at the top level, the obvious guess, finds nothing.
//
// IDENTITY. (workspace_id, name) is the only caller-meaningful stable key: pane and
// tab ids are stable while a pane lives and never reused, but they die with it,
// and `name` is NULLABLE — herdr clears it when a pane's occupant exits. name is
// also why this package exists at all: herdr's name for a pane is STABLE across
// the agent's whole life ("project-manager-keeper"), while Claude Code's own
// derived name for the very same session is per-run ("project-manager-9a", and a
// different suffix after the next restart). Measured on one live session
// 2026-08-25: herdr said project-manager-keeper, Claude said project-manager-9a.
//
// STATUS is exactly idle | working | blocked | done | unknown, and that is the
// whole vocabulary. See Status for how it is meant to land on fleet's existing
// demand ladder, for the two values that mislead, and for why the mapping is
// documented here but implemented on the fleet side (an import cycle otherwise).
//
// COST DISCIPLINE. List spawns a process. It belongs on the daemon's 1s tick and
// MUST NOT go anywhere near the 13ms DB poll — a fork+socket roundtrip 77 times a
// second would be visible in the machine's load, for data that changes at human
// speed. Fingerprint exists so a steady herd costs one call and no downstream
// rebuild: `revision` and `state_change_seq` are herdr's own monotonic
// change-detection counters, so an unchanged fingerprint means nothing observable
// moved. Deliberately NOT used here: `herdr agent explain` and `herdr agent read
// --source detection`, which return SCREEN CONTENTS — kilobytes of terminal
// scrape per agent. They are for an on-demand subcommand a human asks for, never
// for a tick.
//
// The output shape is UNDOCUMENTED (observed on herdr 0.8.2, socket protocol 20),
// so every function here is tolerant in clauded's style: a payload that will not
// parse, a herdr that is not installed, or a server that is not running all
// degrade to "no roster" rather than to an error a caller has to handle.
package herdr

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Status is herdr's reported agent status: idle | working | blocked | done |
// unknown, and that is the whole vocabulary. Values other than the five constants
// are preserved verbatim, so a future herdr status is observable rather than
// silently coerced (the same choice clauded.Status makes).
//
// HOW THIS LANDS ON THE DEMAND LADDER. Consumers rank attention with
// fleet.Demand, and the intended mapping is:
//
//	blocked -> DemandHuman    the attention signal; a person is being waited on
//	working -> DemandWorking
//	idle    -> DemandIdle
//	done    -> DemandIdle     sticky flavour of idle, see below
//	unknown -> its own bucket, NEVER "finished", see below
//
// The mapping is documented here but deliberately NOT IMPLEMENTED here, and the
// reason is structural: internal/fleet is becoming provider-shaped and will import
// this package to get the roster, so a Demand() method returning fleet.Demand
// would close an import cycle and simply refuse to build. This package therefore
// depends on nothing of ours — it speaks herdr's vocabulary and stops there, the
// same way clauded exposes busy/idle/waiting and lets fleet.demandOf do the
// ranking. Implement the switch on the fleet side.
//
// `done` maps to IDLE because it is the same underlying at-rest state, flagged
// "you have not looked at this yet". It is STICKY, and that makes it the COMMON
// reading rather than an edge case: only the UI's seen-bookkeeping clears the
// flag, and a CLI read explicitly does not mark anything seen, so a headless
// reader sees agents park at `done` and stay there — measured 2026-08-25, a live
// helpdesk agent sat at `done` for many minutes with nothing happening. Ranking it
// as DemandDone would let a caller retire an agent that is sitting right there
// ready for its next prompt, which is the bug this note exists to prevent.
//
// `unknown` gets NO rung on the ladder — it is its own bucket. It means "present
// but unclassifiable", which a transcript viewer and a model picker both produce;
// the agent is alive and may be mid-thought. Calling it finished is the trap (same
// retire-a-live-agent bug), and calling it blocked would fabricate an attention
// signal nobody asked for.
type Status string

const (
	// StatusIdle: the agent is alive and at rest.
	StatusIdle Status = "idle"
	// StatusWorking: the agent is taking a turn.
	StatusWorking Status = "working"
	// StatusBlocked: the agent is waiting on a human. This is THE attention
	// signal — herdr raises it from a detected permission or workflow prompt.
	StatusBlocked Status = "blocked"
	// StatusDone: background work finished and has not been LOOKED AT yet. A
	// sticky flavour of idle, not a lifecycle end — see the Status doc.
	StatusDone Status = "done"
	// StatusUnknown: an agent is present but herdr cannot classify what its
	// screen is showing. NOT "finished" — see the Status doc.
	StatusUnknown Status = "unknown"
)

// Known reports whether s is one of the five statuses herdr documents. An
// unrecognized value is worth surfacing rather than mapping on a guess.
func (s Status) Known() bool {
	switch s {
	case StatusIdle, StatusWorking, StatusBlocked, StatusDone, StatusUnknown:
		return true
	default:
		return false
	}
}

// AgentSession is herdr's bridge to the agent's own session id — the join key
// between a herdr pane and the first-party ~/.claude/sessions/<pid>.json record
// (and therefore to our own DB rows).
//
// It is OPTIONAL, and absent means absent: the key is missing from the JSON
// entirely, not present-and-null. A pane only gains it once herdr's Claude
// integration has run for that session, so sessions that predate installing the
// integration never grow one until they restart. Both shapes were live on the same
// machine at the same time on 2026-08-25 (one agent with a session, three
// without), so this is a steady state to render, not a migration to wait out.
type AgentSession struct {
	// Source is which integration reported it ("herdr:claude").
	Source string
	// Agent is the agent kind the integration speaks for ("claude").
	Agent string
	// Kind is how to read Value: "id" (a session id) or "path" (a transcript).
	Kind string
	// Value is the session id or path itself.
	Value string
}

// Agent is one pane's occupant, as `herdr agent list` reports it.
type Agent struct {
	// Kind is herdr's `agent` field: which agent program occupies the pane
	// ("claude"). Named Kind because Agent.Agent reads like a typo.
	Kind   string
	Status Status

	// WorkspaceID, TabID, PaneID are herdr's public ids ("w1", "w1:t8V",
	// "w1:p8V"). Stable while the pane lives, never reused, gone when it dies.
	WorkspaceID string
	TabID       string
	PaneID      string

	// Name is herdr's stable name for the agent ("project-manager-integrator").
	// OPTIONAL: herdr clears it when the pane's occupant exits, so an empty Name
	// is a normal live reading, not corruption. See Key.
	Name string

	// Cwd is the pane's shell cwd; ForegroundCwd is the cwd of whatever is
	// running in it. They differ when the agent cd'd into a worktree.
	Cwd           string
	ForegroundCwd string

	// Focused is true for the pane the human is looking at — one per herdr
	// session, so this is a "where is the human" hint, not a property of the
	// agent's work.
	Focused bool
	// InteractiveReady is whether the agent will accept a prompt right now.
	InteractiveReady bool

	// Revision and StateChangeSeq are herdr's own monotonic counters. They are
	// the CHEAP change detection: equal counters mean nothing moved, so a tick
	// can skip rebuilding anything downstream. See Fingerprint.
	Revision       int
	StateChangeSeq int64

	// TerminalID identifies the pane's terminal ("term_659da53280c417").
	TerminalID string

	// TerminalTitle is COSMETIC and must never be read for meaning. It carries
	// herdr's own spinner glyph, and when a terminal has splits it is the FOCUSED
	// pane's title fanned out to its siblings — the epic recorded four distinct
	// live agents all reporting one identical title. TitleStripped is the same
	// string with the glyph removed; it is decoded so a debug dump can show what
	// herdr shows, and for nothing else. To learn what an agent is DOING, read
	// Status and the agent's own first-party record.
	TerminalTitle string
	TitleStripped string

	// Session is the first-party session bridge; HasSession distinguishes an
	// absent `agent_session` key from one that decoded to zeroes.
	Session    AgentSession
	HasSession bool
}

// Key is the only caller-meaningful stable identity for an agent: its workspace
// plus its herdr name. pane and tab ids are stable but mortal, and the epic
// measured ~30 new Claude session ids an hour behind one window, so neither a pane
// id nor a session id survives long enough to name a row on a status bar.
type Key struct {
	WorkspaceID string
	Name        string
}

// Key returns the agent's stable key. ok is false when Name is empty (an exited
// occupant, or a pane herdr has not named), in which case there is nothing durable
// to key on and a caller should fall back to PaneID for the pane's lifetime rather
// than inventing a name.
func (a Agent) Key() (Key, bool) {
	if a.Name == "" {
		return Key{WorkspaceID: a.WorkspaceID}, false
	}
	return Key{WorkspaceID: a.WorkspaceID, Name: a.Name}, true
}

// ServerError is a herdr-side refusal, decoded from the envelope's `error` object.
// herdr exits non-zero AND writes this on stdout, which is why the exec seam keeps
// stdout on a failed exit.
//
// It is not really an error condition for us: `server_not_running` is the normal
// reading on a machine where nobody started herdr today, and the right response is
// to show no herdr rows. Callers should treat any ServerError as "no roster" and
// only doctor should print the code.
type ServerError struct {
	Code    string
	Message string
}

func (e *ServerError) Error() string {
	if e.Message == "" {
		return "herdr: " + e.Code
	}
	return "herdr: " + e.Code + ": " + e.Message
}

// NotRunning reports the specific benign case: herdr is installed but no server
// is listening on the socket.
func (e *ServerError) NotRunning() bool { return e.Code == "server_not_running" }

// ErrNotInstalled means no herdr executable was found on PATH. Distinct from a
// ServerError so a caller can tell "this machine does not use herdr" (say nothing,
// ever) from "herdr is here but asleep" (say nothing now).
var ErrNotInstalled = errors.New("herdr: executable not found on PATH")

// CallTimeout bounds one roster call. The socket can be slow or wedged while the
// server is busy, and the daemon's 1s tick cannot afford to block on it: a missed
// roster reading is invisible, a stalled tick freezes every bar on the machine.
const CallTimeout = 2 * time.Second

// runAgentList executes `herdr agent list` and returns its raw stdout. Package var
// so tests can stub it and never spawn a process — the same seam clauded uses for
// /proc, for the same reason (a test that shells out is a test that depends on the
// developer's machine having a herdr session up).
//
// $HERDR_BIN overrides the executable, which is what makes an integration test
// against a fake possible without touching this var.
//
// Stdout is returned even when herdr exits non-zero, because that is exactly how
// it reports a refusal: exit 1 with an `error` envelope on stdout. Discarding
// stdout on a non-zero exit would turn every diagnosable refusal into a bare
// "exit status 1".
var runAgentList = func(ctx context.Context) ([]byte, error) {
	bin := os.Getenv("HERDR_BIN")
	if bin == "" {
		bin = "herdr"
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return nil, ErrNotInstalled
	}
	cmd := exec.CommandContext(ctx, path, "agent", "list")
	out, err := cmd.Output() // stderr deliberately dropped: the JSON is on stdout
	if len(out) > 0 {
		return out, nil
	}
	return nil, err
}

// List returns the live roster, sorted deterministically.
//
// A ctx without a deadline gets CallTimeout applied here rather than at each call
// site: forgetting the deadline is the failure that hurts (see CallTimeout), and a
// caller that wants a longer one still wins by passing its own.
func List(ctx context.Context) ([]Agent, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, CallTimeout)
		defer cancel()
	}
	out, err := runAgentList(ctx)
	if err != nil {
		return nil, err
	}
	return ParseAgentList(out)
}

// rawEnvelope mirrors `herdr agent list`'s output. Note the DOUBLE wrapping:
// agents live at .result.agents, not at .agents. Unknown fields are ignored, so
// herdr adding to the payload (state_labels, tokens — documented in the epic,
// absent from the 0.8.2 output measured here) never breaks the read.
type rawEnvelope struct {
	ID     string `json:"id"`
	Result *struct {
		Type   string     `json:"type"`
		Agents []rawAgent `json:"agents"`
	} `json:"result"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// rawAgent mirrors one agent object. Pointer fields are the OPTIONAL ones, where
// "absent" and "present but empty" are different readings: name is cleared to null
// when a pane's occupant exits, and agent_session is missing entirely until
// herdr's Claude integration has spoken for that session.
type rawAgent struct {
	Agent                 string           `json:"agent"`
	AgentStatus           string           `json:"agent_status"`
	Cwd                   string           `json:"cwd"`
	ForegroundCwd         string           `json:"foreground_cwd"`
	Focused               bool             `json:"focused"`
	InteractiveReady      bool             `json:"interactive_ready"`
	Name                  *string          `json:"name"`
	PaneID                string           `json:"pane_id"`
	TabID                 string           `json:"tab_id"`
	WorkspaceID           string           `json:"workspace_id"`
	Revision              int              `json:"revision"`
	StateChangeSeq        int64            `json:"state_change_seq"`
	TerminalID            string           `json:"terminal_id"`
	TerminalTitle         string           `json:"terminal_title"`
	TerminalTitleStripped string           `json:"terminal_title_stripped"`
	AgentSession          *rawAgentSession `json:"agent_session"`
}

type rawAgentSession struct {
	Source string `json:"source"`
	Agent  string `json:"agent"`
	Kind   string `json:"kind"`
	Value  string `json:"value"`
}

// ParseAgentList decodes one `herdr agent list` payload. Pure, and exported so
// doctor can render a payload captured from a file or a bug report without
// spawning herdr.
//
// Tolerance is the whole design here, and it is asymmetric on purpose:
//   - garbage, truncated JSON, or an envelope with no `result` yields NO AGENTS
//     AND NO ERROR. The caller's honest reading of "herdr told me nothing usable"
//     is an empty roster, and making that an error only invites callers to log it
//     every second.
//   - a herdr-side refusal yields a *ServerError, because the code names something
//     actionable ("run herdr") that a human should be able to see in doctor.
//
// Agents with no pane_id are dropped: there is nothing to key them on, and an
// object that thin means the payload is not what we think it is.
func ParseAgentList(data []byte) ([]Agent, error) {
	var env rawEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, nil
	}
	if env.Error != nil {
		return nil, &ServerError{Code: env.Error.Code, Message: env.Error.Message}
	}
	if env.Result == nil {
		return nil, nil
	}
	out := make([]Agent, 0, len(env.Result.Agents))
	for _, r := range env.Result.Agents {
		if r.PaneID == "" {
			continue
		}
		a := Agent{
			Kind:             r.Agent,
			Status:           Status(r.AgentStatus),
			WorkspaceID:      r.WorkspaceID,
			TabID:            r.TabID,
			PaneID:           r.PaneID,
			Cwd:              r.Cwd,
			ForegroundCwd:    r.ForegroundCwd,
			Focused:          r.Focused,
			InteractiveReady: r.InteractiveReady,
			Revision:         r.Revision,
			StateChangeSeq:   r.StateChangeSeq,
			TerminalID:       r.TerminalID,
			TerminalTitle:    r.TerminalTitle,
			TitleStripped:    r.TerminalTitleStripped,
		}
		if r.Name != nil {
			a.Name = *r.Name
		}
		if r.AgentSession != nil {
			a.Session = AgentSession{
				Source: r.AgentSession.Source,
				Agent:  r.AgentSession.Agent,
				Kind:   r.AgentSession.Kind,
				Value:  r.AgentSession.Value,
			}
			a.HasSession = true
		}
		out = append(out, a)
	}
	sortAgents(out)
	return out, nil
}

// sortAgents imposes a total order: workspace, then pane id. herdr's own order is
// not promised anywhere, and the tiles learned the hard way that a
// non-deterministic pick blinks — two renders of one unchanged roster must produce
// the same bytes.
func sortAgents(as []Agent) {
	sort.Slice(as, func(i, j int) bool {
		if as[i].WorkspaceID != as[j].WorkspaceID {
			return as[i].WorkspaceID < as[j].WorkspaceID
		}
		return as[i].PaneID < as[j].PaneID
	})
}

// Fingerprint folds a roster into one string that changes when anything observable
// changes, so the 1s tick can compare it against last tick's and skip the whole
// downstream rebuild.
//
// revision and state_change_seq carry the work: they are herdr's OWN monotonic
// counters, bumped by the server when a pane's state moves, so they are the
// cheapest possible change signal. The other fields are folded in because they are
// free and they close the gap where herdr changes something without bumping a
// counter we happen to be watching — a rename, a focus move, the agent_session
// arriving. A missed change here is a bar that lies until the next real event, so
// the fingerprint errs toward changing too often.
//
// Not a hash: a short readable string diffs nicely in a log, and the roster is
// four to a few dozen entries, never thousands.
func Fingerprint(agents []Agent) string {
	var b strings.Builder
	for _, a := range agents {
		b.WriteString(a.PaneID)
		b.WriteByte(':')
		b.WriteString(strconv.Itoa(a.Revision))
		b.WriteByte(':')
		b.WriteString(strconv.FormatInt(a.StateChangeSeq, 10))
		b.WriteByte(':')
		b.WriteString(string(a.Status))
		b.WriteByte(':')
		b.WriteString(a.Name)
		b.WriteByte(':')
		if a.HasSession {
			b.WriteString(a.Session.Value)
		}
		if a.Focused {
			b.WriteString(":f")
		}
		b.WriteByte('\n')
	}
	return b.String()
}
