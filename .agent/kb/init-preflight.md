---
name: init-preflight
description: init resolves stack prerequisites and renders the Dockerfile before any scaffold or hook mutation
subsystem: scaffold
sources: [internal/scaffold/scaffold.go, internal/cli/commands.go, internal/cli/init_test.go]
updated: 2026-09-05
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

## Changelog
- 2026-09-05 — created after reproducing fresh scaffold writes and re-init Git-config rewrites
  before stack rejection; direct and CLI regressions snapshot complete isolated trees and retry.
