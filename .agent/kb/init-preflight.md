---
name: init-preflight
description: init resolves stack prerequisites and renders the Dockerfile before any scaffold or hook mutation
subsystem: scaffold
sources: [internal/scaffold/scaffold.go, internal/scaffold/compose_add.go, internal/scaffold/projectfile.go, internal/cli/commands.go, internal/cli/init_test.go]
updated: 2026-09-13
---

# Stack validation precedes scaffold writes

`scaffold.Init` resolves its stack, verifies the explicit asdf prerequisite and renders any
Dockerfile before creating directories, writing instructions/configuration, or configuring Git
hooks. Invalid stacks fail even when `.tool-versions` exists; explicit asdf still requires that
file on re-init, including when a customized Dockerfile already exists.

The existing create-only write path owns the Dockerfile and its auto-detection announcement.
Tool parsing and unknown-tool acceptance are unchanged. The CLI calls Compose and global MCP
scaffolding only after Init succeeds. First-run prompts remain CLI-owned and precede Init; this
preflight does not introduce a second CLI validation implementation.

Additive `init --services` edits use the same rooted file capture, create-only write and
staged replacement as project scaffolding. Leaf symlinks, outside parents, lexical escapes
and nonregular files are refused; relative parent aliases inside the repository work.
Staged edits retain permissions independently of umask and preserve the original on write
failure. A changed identity/content at the pre-publication recheck or a raced-in new file
produces a Compose-specific retry error. This is atomic replacement with a recheck, not a
compare-and-swap guarantee against every concurrent edit between recheck and rename.

## Changelog
- 2026-09-13 — verified additive Compose edits against rooted helpers and synthetic
  outside-link/partial-write/intervening-edit/FIFO/umask regressions; no service startup.
- 2026-09-05 — created after reproducing fresh scaffold writes and re-init Git-config rewrites
  before stack rejection; direct and CLI regressions snapshot complete isolated trees and retry.
