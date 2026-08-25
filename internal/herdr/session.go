package herdr

// Workspace labels and durable pane identity, read from herdr's own session file.
//
// The roster from `herdr agent list` says which workspace an agent is in ("w1")
// but never what that workspace IS. The name a human recognizes lives in
// ~/.config/herdr/session.json, which herdr rewrites as the session changes — a
// few kilobytes, so cheap that reading it every tick costs less than the process
// spawn the roster already pays for.
//
// The file is also the DURABLE copy of pane identity: tabs[].panes{} records
// agent_name, managed_agent_kind, cwd and (once the integration has run)
// agent_session per pane. Those survive a herdr server restart, whereas the live
// roster is gone until every agent is back.

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// WorktreeSpace is the git checkout a workspace is bound to. Label is herdr's own
// name for the repo, which is the REPO name and therefore shared by every
// workspace on that repo — see Workspace.DisplayLabel for why that matters.
type WorktreeSpace struct {
	// Key is herdr's identity for the space (the .git path).
	Key string
	// Label is herdr's repo-level name ("project-manager").
	Label string
	// RepoRoot is the main checkout; CheckoutPath is this workspace's checkout,
	// which differs when the workspace sits on a linked worktree.
	RepoRoot     string
	CheckoutPath string
	// IsLinkedWorktree is true when CheckoutPath is a linked worktree rather than
	// the main checkout.
	IsLinkedWorktree bool
}

// PaneIdentity is one pane's entry under tabs[].panes{}. This is the identity that
// outlives the live roster: a herdr server restart empties `agent list` but leaves
// this file intact.
type PaneIdentity struct {
	// Number is the pane's key in the JSON object ("7", "123"). It is herdr's
	// INTERNAL pane number and does NOT appear in the public pane id — the same
	// pane was live as "w1:p8V" while keyed "7" here, and the public id also has
	// its own mapping under public_pane_numbers ("7" -> 283). Three numbering
	// namespaces, none of them derivable from another by inspection, so do not
	// try to join a roster entry to a pane by id. Join on (workspace, agent name),
	// which is the key herdr itself treats as stable.
	Number string
	// Cwd is the pane's recorded working directory.
	Cwd string
	// AgentName mirrors the roster's `name`; AgentKind mirrors its `agent`. Both
	// are absent for a pane holding a plain shell.
	AgentName string
	AgentKind string
	// Session is the same first-party bridge the roster carries, persisted; see
	// AgentSession. HasSession distinguishes absent from empty.
	Session    AgentSession
	HasSession bool
}

// Workspace is one entry of session.json's workspaces[].
type Workspace struct {
	// ID is the public workspace id the roster reports ("w1", "w9", "wA").
	ID string
	// CustomName is the name the human gave this workspace; empty when unset
	// (the JSON has null, not a missing key).
	CustomName string
	// IdentityCwd is the directory the workspace is anchored to. For a colony
	// chamber this is the worktree, which is what distinguishes two workspaces
	// that share one Space.
	IdentityCwd string
	// Space is the git checkout binding; HasSpace is false for a workspace not
	// bound to one.
	Space    WorktreeSpace
	HasSpace bool
	// Panes is every pane of every tab, keyed by PaneIdentity.Number. Flattened
	// across tabs on purpose: nothing downstream cares which tab a pane is in,
	// and the tab layout is UI state we have no business modelling.
	Panes map[string]PaneIdentity
}

// DisplayLabel is the name to show for this workspace: CustomName if set, else the
// space's repo Label, else the raw id.
//
// The precedence is load-bearing, not a preference. Space.Label is the REPO name,
// so every workspace on one repo carries the same one: measured 2026-08-25, three
// live workspaces all labelled "project-manager" while their custom names were
// "project-manager-6" and "project-manager-7" (and null for the main one). Reading
// Label first would render a herd as three identical rows — exactly the
// indistinguishable-rows failure this epic exists to remove.
func (w Workspace) DisplayLabel() string {
	if w.CustomName != "" {
		return w.CustomName
	}
	if w.HasSpace && w.Space.Label != "" {
		return w.Space.Label
	}
	return w.ID
}

// PaneByAgentName returns the durable identity of the pane holding the named agent.
// This is the join between a roster entry and the persisted record, and it goes
// through the NAME because the id namespaces do not line up — see
// PaneIdentity.Number.
func (w Workspace) PaneByAgentName(name string) (PaneIdentity, bool) {
	if name == "" {
		return PaneIdentity{}, false // an unnamed pane matches nothing, not everything
	}
	for _, p := range w.Panes {
		if p.AgentName == name {
			return p, true
		}
	}
	return PaneIdentity{}, false
}

// DefaultSessionPath returns the path to herdr's session file.
//
// The default session's directory is the socket's directory: `herdr session list`
// prints them side by side (directory ~/.config/herdr, socket
// ~/.config/herdr/herdr.sock), and a NAMED session gets its own directory. So
// HERDR_SOCKET_PATH — which herdr injects into every pane's environment, and which
// our hook therefore inherits for free — is the most specific thing available and
// is honoured first: it makes this correct for a named session, which a hardcoded
// ~/.config/herdr would silently get wrong.
//
// Otherwise it is $XDG_CONFIG_HOME/herdr (verified honoured by herdr 0.8.2: with
// XDG_CONFIG_HOME set, `herdr --help` reported its config under it), else
// ~/.config/herdr.
func DefaultSessionPath() string {
	if sock := os.Getenv("HERDR_SOCKET_PATH"); sock != "" {
		return filepath.Join(filepath.Dir(sock), "session.json")
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "herdr", "session.json")
}

// ReadSession reads and decodes session.json, keyed by workspace id.
//
// Tolerant like everything else here: a missing file (herdr never ran on this
// machine) and an unparseable one (a format change, or a read that caught herdr
// mid-write — it rewrites this file in place) both yield an empty map and no
// error, so labels degrade to bare workspace ids rather than taking a tick down.
func ReadSession(path string) (map[string]Workspace, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]Workspace{}, nil
		}
		return nil, err
	}
	return ParseSession(data), nil
}

// rawSessionFile mirrors the parts of session.json we read. Everything else in
// there — sidebar widths, layouts, tab focus, next_public_pane_number — is UI
// state; notably next_public_pane_number is NOT a run counter and must not be read
// as one.
type rawSessionFile struct {
	Version    int            `json:"version"`
	Workspaces []rawWorkspace `json:"workspaces"`
}

type rawWorkspace struct {
	ID          string  `json:"id"`
	CustomName  *string `json:"custom_name"`
	IdentityCwd string  `json:"identity_cwd"`
	Space       *struct {
		Key              string `json:"key"`
		Label            string `json:"label"`
		RepoRoot         string `json:"repo_root"`
		CheckoutPath     string `json:"checkout_path"`
		IsLinkedWorktree bool   `json:"is_linked_worktree"`
	} `json:"worktree_space"`
	Tabs []struct {
		Panes map[string]struct {
			Cwd              string           `json:"cwd"`
			AgentName        *string          `json:"agent_name"`
			ManagedAgentKind *string          `json:"managed_agent_kind"`
			AgentSession     *rawAgentSession `json:"agent_session"`
		} `json:"panes"`
	} `json:"tabs"`
}

// ParseSession decodes a session.json payload. Pure, and it never fails: garbage
// yields an empty map, for the reasons in ReadSession.
func ParseSession(data []byte) map[string]Workspace {
	var f rawSessionFile
	if err := json.Unmarshal(data, &f); err != nil {
		return map[string]Workspace{}
	}
	out := make(map[string]Workspace, len(f.Workspaces))
	for _, rw := range f.Workspaces {
		if rw.ID == "" {
			continue // nothing to key on
		}
		w := Workspace{
			ID:          rw.ID,
			IdentityCwd: rw.IdentityCwd,
			Panes:       map[string]PaneIdentity{},
		}
		if rw.CustomName != nil {
			w.CustomName = *rw.CustomName
		}
		if rw.Space != nil {
			w.Space = WorktreeSpace{
				Key:              rw.Space.Key,
				Label:            rw.Space.Label,
				RepoRoot:         rw.Space.RepoRoot,
				CheckoutPath:     rw.Space.CheckoutPath,
				IsLinkedWorktree: rw.Space.IsLinkedWorktree,
			}
			w.HasSpace = true
		}
		for _, tab := range rw.Tabs {
			for num, rp := range tab.Panes {
				p := PaneIdentity{Number: num, Cwd: rp.Cwd}
				if rp.AgentName != nil {
					p.AgentName = *rp.AgentName
				}
				if rp.ManagedAgentKind != nil {
					p.AgentKind = *rp.ManagedAgentKind
				}
				if rp.AgentSession != nil {
					p.Session = AgentSession{
						Source: rp.AgentSession.Source,
						Agent:  rp.AgentSession.Agent,
						Kind:   rp.AgentSession.Kind,
						Value:  rp.AgentSession.Value,
					}
					p.HasSession = true
				}
				w.Panes[num] = p
			}
		}
		out[rw.ID] = w
	}
	return out
}

// Label resolves an agent's workspace label against a workspace map, falling back
// to the raw workspace id when the file has nothing for it (a workspace created
// since the last write, or no session.json at all). Small, but it keeps the
// "id or label?" decision in one place instead of at every call site.
func Label(ws map[string]Workspace, workspaceID string) string {
	if w, ok := ws[workspaceID]; ok {
		return w.DisplayLabel()
	}
	return workspaceID
}
