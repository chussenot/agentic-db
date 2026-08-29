# Project Instructions for AI Agents

This file provides instructions and context for AI coding agents working on this project.

<!-- BEGIN BEADS INTEGRATION v:1 profile:minimal hash:6cd5cc61 -->
## Beads Issue Tracker

This project uses **bd (beads)** for issue tracking. Run `bd prime` to see full workflow context and commands.

### Quick Reference

```bash
bd ready              # Find available work
bd show <id>          # View issue details
bd update <id> --claim  # Claim work
bd close <id>         # Complete work
```

### Rules

- Use `bd` for ALL task tracking — do NOT use TodoWrite, TaskCreate, or markdown TODO lists
- Run `bd prime` for detailed command reference and session close protocol
- Use `bd remember` for persistent knowledge — do NOT use MEMORY.md files

**Architecture in one line:** issues live in a local Dolt DB; sync uses `refs/dolt/data` on your git remote; `.beads/issues.jsonl` is a passive export. See https://github.com/gastownhall/beads/blob/main/docs/SYNC_CONCEPTS.md for details and anti-patterns.

## Agent Context Profiles

The managed Beads block is task-tracking guidance, not permission to override repository, user, or orchestrator instructions.

- **Conservative (default)**: Use `bd` for task tracking. Do not run git commits, git pushes, or Dolt remote sync unless explicitly asked. At handoff, report changed files, validation, and suggested next commands.
- **Minimal**: Keep tool instruction files as pointers to `bd prime`; use the same conservative git policy unless active instructions say otherwise.
- **Team-maintainer**: Only when the repository explicitly opts in, agents may close beads, run quality gates, commit, and push as part of session close. A current "do not commit" or "do not push" instruction still wins.

## Session Completion

This protocol applies when ending a Beads implementation workflow. It is subordinate to explicit user, repository, and orchestrator instructions.

1. **File issues for remaining work** - Create beads for anything that needs follow-up
2. **Run quality gates** (if code changed) - Tests, linters, builds
3. **Update issue status** - Close finished work, update in-progress items
4. **Handle git/sync by active profile**:
   ```bash
   # Conservative/minimal/default: report status and proposed commands; wait for approval.
   git status

   # Team-maintainer opt-in only, unless current instructions forbid it:
   git pull --rebase
   git push
   git status
   ```
5. **Hand off** - Summarize changes, validation, issue status, and any blocked sync/commit/push step

**Critical rules:**
- Explicit user or orchestrator instructions override this Beads block.
- Do not commit or push without clear authority from the active profile or the current user request.
- If a required sync or push is blocked, stop and report the exact command and error.
<!-- END BEADS INTEGRATION -->


## Build & Test

Tasks run through mise (`mise.toml`); there is no Makefile, by decision.

```bash
mise run check       # the full serial gate — exactly what CI runs
mise run test        # go test ./...
mise run build       # static binary (CGO_ENABLED=0, proven by ldd)
mise run fmt-check   # gofmt drift (reports, never rewrites)
mise run vet         # go vet ./...
mise run lint        # staticcheck (the one adopted linter)
mise run check-docs  # front matter, dead links/anchors, CLI drift
```

## Architecture Overview

A multi-call Go binary (`claude-status`): Claude Code hooks write session
state to SQLite (`hook` — hot path, always exits 0); a single long-lived
daemon reconciles DB + niri into workspace glyphs and a tile cache. See
`docs/architecture.md` (diagrams, package layout) and `docs/adr/` (decisions,
alternatives priced). The repo is agentic-db; binary/module/DB stay
`claude-status` (recorded deferral).

## Conventions & Patterns

- Conventional Commits **with a scope**; commit via `git commit -F <msgfile>
  -- <explicit paths>`, never bare. Never mention any AI, model, or assistant
  name in a commit, tag, or PR. Do not commit or push unless asked.
- Versions are cut with `cog bump` only (`cog.toml`; gate applies from the
  v0.1.0 baseline tag forward). CHANGELOG.md is two-layer — see its preamble
  before touching it (cog insertion-anchor trap).
- The Go toolchain is pinned ONCE, in go.mod's `toolchain` directive;
  mise.toml mirrors it mechanically (`mise run pin-check` enforces the match).
- Docs: front matter on every markdown file (except this file and AGENTS.md);
  ADRs price their alternatives; diagrams are Mermaid in-markdown; cite the
  tile contract by repo (waybar-pwetty-box `tiles/claude/schema.json`), never
  by a machine path. `scripts/check-docs` gates all of it.
- Recurring agent roles live in `.claude/agents/` (daemon-reconciler,
  cli-surface-auditor, docs-writer); reference them in briefs, and edit them
  in the same commit when a run teaches a role something.
