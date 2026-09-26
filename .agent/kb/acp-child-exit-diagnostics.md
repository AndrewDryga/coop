---
name: acp-child-exit-diagnostics
description: stdout EOF can precede stderr collection when an ACP child exits
subsystem: acp
sources: [internal/sessionsvc/acp.go, internal/sessionsvc/acp_test.go]
updated: 2026-09-26
---

An ACP child's stdout and stderr are read independently. At unexpected stdout EOF,
`cmd.Wait` may still be copying stderr into the bounded collector. Rendering the
turn failure immediately can lose an allowlisted setup diagnostic such as the
missing-image `coop build` guidance. `sessionACPChildClosedFailure` waits briefly
for the exit-completion signal before applying `safeSessionACPExitDetail`; a child
that closes stdout but remains alive still receives a generic, bounded failure.
Never expose arbitrary child stderr, which may contain credentials.

## Changelog
- 2026-09-26 — recorded the reproduced 1-in-100 Ubuntu gate failure and the
  stdout-before-stderr scripted regression; verified against `acp.go` and `acp_test.go`.
