---
name: acp-child-exit-diagnostics
description: stdout EOF can precede stderr collection, and a fast-failing child can exit before its first frame is written; both wait briefly for the exit before rendering safe setup guidance
subsystem: acp
sources: [internal/sessionsvc/acp.go, internal/sessionsvc/acp_test.go]
updated: 2026-10-16
---

An ACP child's stdout and stderr are read independently. At unexpected stdout EOF,
`cmd.Wait` may still be copying stderr into the bounded collector. Rendering the
turn failure immediately can lose an allowlisted setup diagnostic such as the
missing-image `coop build` guidance. `sessionACPChildClosedFailure` waits briefly
for the exit-completion signal before applying `safeSessionACPExitDetail`; a child
that closes stdout but remains alive still receives a generic, bounded failure.
Never expose arbitrary child stderr, which may contain credentials.

The same child can exit before the session writes its first frame. The write then
fails with a broken pipe, or with "file already closed" once `cmd.Wait` has run:
after the child exits, Wait closes the parent's ends of the stdin and stdout pipes.
`writeSessionACP` therefore reports every failed write through
`sessionACPChildClosedFailure`, so the diagnostic does not depend on whether the
read or the write noticed the exit first. During a turn nothing else closes the
child's stdin (a warm child in use is out of the pool, see `takeWarmExecution`; only
`Run`'s deferred `stopSessionChild` stops it, after the turn), so a failed write
always means the child's side is gone.

## Changelog
- 2026-10-16 — a loaded gate reported "ACP frame could not be written" for a
  missing image; failed writes now share the closed-read diagnostic.
  `TestSessionACPChildExitBeforeFirstFrameKeepsLaunchDiagnostic` waits for the
  exit before the first write (no timing); it failed the old code 3/3.
- 2026-09-26 — recorded the reproduced 1-in-100 Ubuntu gate failure and the
  stdout-before-stderr scripted regression; verified against `acp.go` and `acp_test.go`.
