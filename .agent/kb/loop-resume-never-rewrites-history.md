---
name: loop-resume-never-rewrites-history
description: a leaked box descendant un-completes committed work; resuming it later must never amend a non-HEAD commit, because that reparents the whole branch and cannot pass validation
subsystem: loop
sources: [internal/cli/controller.go, internal/cli/commands.go, internal/box/image.go, internal/box/run.go]
updated: 2026-08-01
---
A completed, committed task can land back in the queue with its work already in history. The chain,
observed twice in emisar on 2026-08-01:

1. The agent commits with its `Coop-Task` trailer and moves the folder to `99_done/`.
2. A descendant it started never exits — a leaked headless Chromium from a browser test is the real
   case. `coop-entry` waits `COOP_DESCENDANT_TIMEOUT` (default **1800s**, `internal/box/image.go`)
   and exits `DescendantsTimedOutExit` (191). It now announces that wait once with the process names
   holding the box open, and names them again on termination — before that it was silent for the
   whole window, which reads as a hung loop.
3. `classifyIteration` returns `background_timeout`, and `restoreBackgroundHandoffCompletion`
   **moves the finished task back to `in_progress`** (`internal/cli/commands.go:3152`) — the commit
   stays, the completion does not.
4. A later run resumes it and `resumeLine` offers the case-(a) recipe: amend the bound commit with a
   `Coop-Recovery` trailer.

Step 4 is safe ONLY while that commit is still HEAD. Deeper it is a trap, which is why
`resumePrefixFor` now gates the recipe on `boundTaskCommitIsHead`:

- Amending a commit N back reparents all N descendants. At N=286 that rewrote the entire branch.
- The rewrite can never pass anyway: every reparented descendant becomes "in range", and those carry
  OTHER tasks' `Coop-Task` trailers, so `unbindableTasks` hits its `!allowed[id] && inRange` guard and
  rejects the whole completion — which restores the task and re-jams the loop on the next iteration.

So when the binding is not HEAD, the resume line says STOP and routes to `coop tasks block`.

**Do not "fix" this by accepting a zero-commit completion.** `unbindableTasks` fails a no-HEAD-change
completion closed on purpose: a zero-commit close is valid only under FRESH host audit authority, so
forged task prose claiming a reopen cannot buy one. The e2e
`verification-only audit re-close needs fresh host authority` guards exactly that, and relaxing the
range check breaks it. `Coop-Recovery` is never parsed by any code — it exists only to make an
in-range commit exist.

The cheap mitigation for a repo whose tests leak browsers is a bounded drain:
`box: env: {COOP_DESCENDANT_TIMEOUT: "180"}` in `.agent/project.yaml` — coop itself never sets this
var. The real fix is not leaking the descendant. See [[box-entrypoint-descendant-handoff]] and
[[task-state-is-the-folder]].

## Changelog
- 2026-08-01 — created: traced the leaked-descendant → un-complete → deep-amend chain after it rewrote 283 and nearly 286 commits of emisar's main; gated the amend recipe on boundTaskCommitIsHead.
