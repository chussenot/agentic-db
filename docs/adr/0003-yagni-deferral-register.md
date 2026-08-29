---
title: "ADR-0003: YAGNI deferral register"
status: active
date: 2026-08-29
decision-makers: [chussenot]
supersedes: none
---

# ADR-0003: YAGNI deferral register

Things deliberately **not** built, each with the trigger that reverses the
deferral. A row leaving this table is a decision: either it graduates to its
own ADR (build it) or it gets a line here saying why it never will.

| Deferral | Why deferred | Reversal trigger |
|---|---|---|
| CI platform matrix | This tool targets one compositor (niri) on Linux; a matrix is CI minutes spent on a promise nobody made. | A second target platform or compositor is actually promised to someone. |
| Tile-JSON goldens + byte-copy sample sync with waybar-pwetty-box | Neither repo currently checks in sample payloads for the `claude` tile, so there is nothing to sync byte-for-byte. | Either side gains checked-in sample payloads; then adopt the byte-copy sample-sync rule the quivive tile uses, with a sync test that skips LOUDLY (naming the missing sibling checkout) when `waybar-pwetty-box` is absent. |
| Renaming binary/module/DB to `agentic-db` | The repo-vs-binary name split is a recorded, standing decision (see the README): every installed hook, unit, and DB path says `claude-status`, and a rename breaks all of them for zero behavior. | The next change that *already* breaks the installed contract (hook paths, DB schema reset, unit rename) — piggyback the rename on a migration users must run anyway. |
| A config file | Two knobs exist (`--db`, `--output`) and both have sane machine defaults; a config file is parsing, precedence, and docs for nothing. | A third machine-specific default appears (the moment "edit the source constant" is the answer twice). |
| Automatic `events` pruning | The audit log is retained indefinitely by design (it exists to debug state drift after the fact); `PruneEvents` exists as a manual primitive. | Measured harm: DB size or `LoadLive` latency demonstrably degraded by `events` growth on a real desktop. |
| A second static linter | staticcheck + `go vet` are the adopted gate (reason recorded on the `lint` task in `mise.toml`); stacking linters buys overlap and config churn first, findings second. | A real bug class ships that staticcheck+vet demonstrably miss and another specific linter demonstrably catches. |
| Conventional-commit gating of pre-`v0.1.0` history | The convention starts at the baseline tag; rewriting or re-judging history that predates the rule is churn with no consumer. | Never — the baseline tag is the floor by design (see `cog.toml`). |
