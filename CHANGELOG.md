---
title: Changelog
status: active
date: 2026-08-29
---

# Changelog

This changelog has two layers. **Notes**, hand-written below, record WHY each
release exists. The **generated record** — produced by `cog bump`, never by
hand — accumulates under the anchor at the bottom of this preamble: cog
inserts each new release section immediately after the first
dash-space-dash-space-dash line in the file. That is why this preamble
describes the anchor instead of quoting it — an early literal occurrence
would hijack the insertion point (a trap already paid for once, in
quivive 0.1.0).

Versions are cut with `cog bump` only, from the `v0.1.0` baseline tag forward
(see `cog.toml`); history before that tag predates the convention and is not
walked.

## Notes

### v0.1.0

The baseline. Everything before this tag is pre-convention history: the
working indicator (hook → SQLite → daemon → glyphs + tile cache), recap
reporting, and `claude-resume`. This tag exists so the conventional-commit
gate and the generated record below have a floor to stand on — it asserts
nothing about API stability.

- - -
