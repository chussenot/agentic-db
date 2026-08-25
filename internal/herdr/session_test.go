package herdr

import (
	"os"
	"path/filepath"
	"testing"
)

// liveSession is trimmed from the real ~/.config/herdr/session.json of 2026-08-25
// (version 3): three workspaces on ONE repo, which is the shape that makes label
// resolution matter — every worktree_space.label is "project-manager", and only
// custom_name tells the three apart. w1 has custom_name null; w9 and wA have names.
// The UI keys the real file also carries (sidebar_width, layout, zoomed,
// public_pane_numbers, next_public_tab_number, …) are kept on w1 so the decode is
// exercised against them.
const liveSession = `{
  "version": 3,
  "sidebar_width": 30,
  "workspaces": [
    {
      "id": "w1",
      "custom_name": null,
      "identity_cwd": "/home/eliebleton/taf/project-manager",
      "worktree_space": {
        "key": "/home/eliebleton/taf/project-manager/.git",
        "label": "project-manager",
        "repo_root": "/home/eliebleton/taf/project-manager",
        "checkout_path": "/home/eliebleton/taf/project-manager",
        "is_linked_worktree": false
      },
      "public_pane_numbers": {"7": 283, "1": 1, "123": 397, "2": 255},
      "next_public_pane_number": 398,
      "tabs": [
        {
          "custom_name": null,
          "layout": {"Pane": 1},
          "panes": {"1": {"cwd": "/home/eliebleton/taf/project-manager"}},
          "zoomed": false,
          "focused": 1,
          "root_pane": 1
        },
        {
          "custom_name": "project-manager-helpdesk",
          "layout": {"Pane": 7},
          "panes": {
            "7": {
              "cwd": "/home/eliebleton/taf/project-manager",
              "agent_name": "project-manager-helpdesk",
              "managed_agent_kind": "claude"
            }
          },
          "focused": 7,
          "root_pane": 7
        },
        {
          "custom_name": "project-manager-integrator",
          "layout": {"Pane": 123},
          "panes": {
            "123": {
              "cwd": "/home/eliebleton/taf/project-manager",
              "agent_name": "project-manager-integrator",
              "managed_agent_kind": "claude",
              "agent_session": {
                "source": "herdr:claude",
                "agent": "claude",
                "kind": "id",
                "value": "c90b0c55-9e66-4a95-9c9a-5bf052a77338"
              }
            }
          },
          "focused": 123,
          "root_pane": 123
        }
      ],
      "active_tab": 2
    },
    {
      "id": "w9",
      "custom_name": "project-manager-6",
      "identity_cwd": "/home/eliebleton/taf/project-manager/.claude/worktrees/project-manager-6",
      "worktree_space": {
        "key": "/home/eliebleton/taf/project-manager/.git",
        "label": "project-manager",
        "repo_root": "/home/eliebleton/taf/project-manager",
        "checkout_path": "/home/eliebleton/taf/project-manager/.claude/worktrees/project-manager-6",
        "is_linked_worktree": true
      },
      "tabs": [
        {
          "panes": {
            "1": {
              "cwd": "/home/eliebleton/taf/project-manager/.claude/worktrees/project-manager-6",
              "agent_name": "project-manager-6",
              "managed_agent_kind": "claude"
            }
          }
        }
      ]
    },
    {
      "id": "wA",
      "custom_name": "project-manager-7",
      "identity_cwd": "/home/eliebleton/taf/project-manager/.claude/worktrees/project-manager-7",
      "worktree_space": {
        "key": "/home/eliebleton/taf/project-manager/.git",
        "label": "project-manager",
        "repo_root": "/home/eliebleton/taf/project-manager",
        "checkout_path": "/home/eliebleton/taf/project-manager/.claude/worktrees/project-manager-7",
        "is_linked_worktree": true
      },
      "tabs": []
    }
  ]
}`

func TestParseSessionLabels(t *testing.T) {
	ws := ParseSession([]byte(liveSession))
	if len(ws) != 3 {
		t.Fatalf("got %d workspaces, want 3", len(ws))
	}
	for _, tc := range []struct {
		id        string
		wantLabel string
		why       string
	}{
		{"w1", "project-manager", "custom_name is null, so the space label is the fallback"},
		{"w9", "project-manager-6", "custom_name must WIN over the repo-level space label"},
		{"wA", "project-manager-7", "custom_name must WIN over the repo-level space label"},
	} {
		if got := ws[tc.id].DisplayLabel(); got != tc.wantLabel {
			t.Errorf("%s DisplayLabel = %q, want %q (%s)", tc.id, got, tc.wantLabel, tc.why)
		}
		if got := Label(ws, tc.id); got != tc.wantLabel {
			t.Errorf("%s Label = %q, want %q", tc.id, got, tc.wantLabel)
		}
	}
	// The whole point of the precedence: the three space labels are IDENTICAL, so
	// reading them first would render one herd as three indistinguishable rows.
	if ws["w1"].Space.Label != ws["w9"].Space.Label || ws["w9"].Space.Label != ws["wA"].Space.Label {
		t.Error("fixture no longer has the colliding repo labels this test is about")
	}
	if !ws["w9"].HasSpace || !ws["w9"].Space.IsLinkedWorktree {
		t.Errorf("w9 space decoded wrong: %+v", ws["w9"].Space)
	}
	if ws["w9"].Space.RepoRoot != "/home/eliebleton/taf/project-manager" {
		t.Errorf("w9 repo root = %q", ws["w9"].Space.RepoRoot)
	}
	if ws["w9"].IdentityCwd != "/home/eliebleton/taf/project-manager/.claude/worktrees/project-manager-6" {
		t.Errorf("w9 identity cwd = %q", ws["w9"].IdentityCwd)
	}
}

func TestDisplayLabelFallsBackToID(t *testing.T) {
	// No custom name and no space (a workspace not bound to a checkout): the id is
	// all there is, and it beats rendering an empty string.
	for _, tc := range []struct {
		name string
		in   Workspace
		want string
	}{
		{"nothing but an id", Workspace{ID: "w3"}, "w3"},
		{"space with an empty label", Workspace{ID: "w3", HasSpace: true}, "w3"},
		{"custom name wins", Workspace{ID: "w3", CustomName: "chamber", HasSpace: true, Space: WorktreeSpace{Label: "repo"}}, "chamber"},
		{"space label when unnamed", Workspace{ID: "w3", HasSpace: true, Space: WorktreeSpace{Label: "repo"}}, "repo"},
	} {
		if got := tc.in.DisplayLabel(); got != tc.want {
			t.Errorf("%s: DisplayLabel = %q, want %q", tc.name, got, tc.want)
		}
	}
	// An id nobody has heard of resolves to itself rather than to "".
	if got := Label(map[string]Workspace{}, "wZ"); got != "wZ" {
		t.Errorf("Label of an unknown workspace = %q, want wZ", got)
	}
}

func TestParseSessionPaneIdentity(t *testing.T) {
	ws := ParseSession([]byte(liveSession))
	w1 := ws["w1"]
	if len(w1.Panes) != 3 {
		t.Fatalf("w1 panes = %d, want 3 (flattened across tabs)", len(w1.Panes))
	}
	// A plain shell pane: no agent, and that is not corruption.
	if p := w1.Panes["1"]; p.AgentName != "" || p.AgentKind != "" || p.HasSession {
		t.Errorf("pane 1 should hold no agent: %+v", p)
	}
	// The durable copy of an agent's identity, and its session bridge.
	p, ok := w1.PaneByAgentName("project-manager-integrator")
	if !ok {
		t.Fatal("PaneByAgentName found no integrator pane")
	}
	if p.Number != "123" || p.AgentKind != "claude" {
		t.Errorf("integrator pane = %+v", p)
	}
	if !p.HasSession || p.Session.Value != "c90b0c55-9e66-4a95-9c9a-5bf052a77338" || p.Session.Kind != "id" {
		t.Errorf("integrator session = %+v (has=%v)", p.Session, p.HasSession)
	}
	// The helpdesk pane predates the integration install: named, no session.
	hp, ok := w1.PaneByAgentName("project-manager-helpdesk")
	if !ok {
		t.Fatal("PaneByAgentName found no helpdesk pane")
	}
	if hp.HasSession {
		t.Errorf("helpdesk pane should carry no agent_session: %+v", hp.Session)
	}
	// An empty name must match nothing: an unnamed pane is not a wildcard.
	if _, ok := w1.PaneByAgentName(""); ok {
		t.Error("an empty agent name must not match a pane")
	}
	if _, ok := w1.PaneByAgentName("nobody"); ok {
		t.Error("an unknown agent name must not match a pane")
	}
}

func TestReadSessionMissingFileIsEmptyNotError(t *testing.T) {
	got, err := ReadSession(filepath.Join(t.TempDir(), "session.json"))
	if err != nil {
		t.Fatalf("a machine without herdr must not error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want empty, got %+v", got)
	}
}

func TestReadSessionTolerantOfGarbage(t *testing.T) {
	// herdr rewrites this file in place, so a read can catch it half-written.
	// Labels degrade to bare workspace ids; nothing fails.
	for _, tc := range []struct{ name, body string }{
		{"garbage", "not json"},
		{"truncated mid-write", `{"version":3,"workspaces":[{"id":"w1","cus`},
		{"empty file", ""},
		{"no workspaces", `{"version":3}`},
		{"workspace with no id", `{"version":3,"workspaces":[{"custom_name":"x"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "session.json")
			if err := os.WriteFile(path, []byte(tc.body), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			got, err := ReadSession(path)
			if err != nil {
				t.Fatalf("want no error, got %v", err)
			}
			if len(got) != 0 {
				t.Errorf("want no workspaces, got %+v", got)
			}
		})
	}
}

func TestReadSessionReadsARealFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.json")
	if err := os.WriteFile(path, []byte(liveSession), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := ReadSession(path)
	if err != nil {
		t.Fatalf("ReadSession: %v", err)
	}
	if got["w9"].DisplayLabel() != "project-manager-6" {
		t.Errorf("w9 label = %q", got["w9"].DisplayLabel())
	}
}

func TestDefaultSessionPath(t *testing.T) {
	// HERDR_SOCKET_PATH first: herdr injects it into every pane, and a NAMED
	// session lives in its own directory next to its socket, so the socket is the
	// only thing that gets that case right.
	t.Setenv("HERDR_SOCKET_PATH", "/run/user/1000/herdr/colony/herdr.sock")
	if got, want := DefaultSessionPath(), "/run/user/1000/herdr/colony/session.json"; got != want {
		t.Errorf("with a socket: %q, want %q", got, want)
	}

	t.Setenv("HERDR_SOCKET_PATH", "")
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg")
	if got, want := DefaultSessionPath(), "/tmp/xdg/herdr/session.json"; got != want {
		t.Errorf("with XDG_CONFIG_HOME: %q, want %q", got, want)
	}

	t.Setenv("XDG_CONFIG_HOME", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory to check the default against")
	}
	if got, want := DefaultSessionPath(), filepath.Join(home, ".config", "herdr", "session.json"); got != want {
		t.Errorf("default: %q, want %q", got, want)
	}
}
