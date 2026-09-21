---
name: loop-resume-never-rewrites-history
description: a leaked box descendant un-completes committed work; resuming it later must never amend a non-HEAD commit, because that reparents the whole branch and cannot pass validation
subsystem: loop
sources: [internal/tasks/audit.go, internal/tasks/completion_recovery.go, internal/loop/completion.go, internal/loop/prompts.go, internal/loop/ratelimit.go, internal/loop/loop.go, internal/box/image.go, internal/box/run.go]
updated: 2026-09-22
---
A completed, committed task can land back in the queue with its work already in history. The chain,
observed twice in emisar on 2026-08-01:

1. The agent commits with its `Coop-Task` trailer and moves the folder to `99_done/`.
2. A descendant it started never exits — a leaked headless Chromium from a browser test is the real
   case. `coop-entry` waits `COOP_DESCENDANT_TIMEOUT` (default **900s**, `internal/box/image.go`)
   and exits `DescendantsTimedOutExit` (191). It now announces that wait once with the process names
   holding the box open, and names them again on termination — before that it was silent for the
   whole window, which reads as a hung loop.
3. `classifyIteration` returns `background_timeout`, and `RestoreBackgroundHandoffCompletion`
   **moves the finished task back to `in_progress`** (`internal/tasks/audit.go:2552`) — the commit
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

**Do not bypass the existing binding with a no-change outcome.** For this already-bound
interrupted task, a zero-new-commit close requires FRESH host audit authority. Explicit no-change
outcomes reject a task with an existing binding, so forged task prose claiming a reopen cannot
buy one. The e2e
`verification-only audit re-close needs fresh host authority` guards exactly that, and relaxing the
range check breaks it. `Coop-Recovery` is never parsed by any code — it exists only to make an
in-range commit exist.

The cheap mitigation for a repo whose tests leak browsers is a bounded drain:
`box: env: {COOP_DESCENDANT_TIMEOUT: "180"}` in `.agent/project.yaml` — coop itself never sets this
var. The real fix is not leaking the descendant. See [[box-entrypoint-descendant-handoff]] and
[[task-state-is-the-folder]].

## The other interrupted shape: no commit, work only in the tree

The chain above is the *committed* interruption. The opposite one is quieter and was costlier per
occurrence: a hard kill (OOM, a crashed container runtime, `docker kill`) stops the agent before it
commits AND before it checkpoints. Nothing lands in history, and `state.md` keeps whatever it last
said — **"not started" for a task that had run ~13 hours**, while four modified files and a new rule
card sat uncommitted in the shared checkout.

So `state.md` is a best-effort snapshot, never evidence: it is written by the very process that got
killed. The tree is the evidence. `resumePrefixFor` now handles this case — no bound commit + a
resumed (`10_in_progress`) task + a dirty tree yields `uncommittedResumeLine`, which names the
changed paths and makes `git diff` the first step, explicitly telling the agent not to trust
state.md over the tree and to commit only its own paths (never `git add -A`, since a shared checkout
can hold another task's work at the same time).

It is gated on the resumed state on purpose: a FRESH claim in a dirty tree is somebody else's work,
and pointing a new task at it invites cross-task edits.

The shared work prompt adds the same ownership boundary to ordinary, fork and audit attempts:
inspect both working and cached diffs, preserve foreign/uncertain work, and never stage the ignored
task folders. Whole-path `commit --only` is safe only for wholly owned paths; overlapping hunks
need reviewed isolation. Mutation-test ownership checks and original-state capture come before
any restoration hook, so a dirty-source refusal cannot trigger a HEAD restore. These are agent
instructions, not a runtime guarantee that every generated shell command follows them.

## No-change outcomes and refused completion

A task may already be satisfied, or an investigation may conclude `could_not_reproduce` or
`wont_fix`. The assigned agent calls `tasks_complete` with that explicit outcome (`already_satisfied`
for existing implementation), concrete `reason` and `evidence` after required verification. It
does not fabricate an empty task commit. The host requires unchanged iteration history and
checkout/index state, with no existing binding for this task. Unchanged pre-existing unrelated
work is allowed — UNCHANGED IN CONTENT, which `git status` cannot establish. Its lines carry a path
and a state, so an agent that inherits a dirty or staged file can rewrite those bytes, leave the line
reading exactly as before, and claim it changed nothing; both the pre-move and post-exit checks
compared those labels. `tasks.CheckoutFingerprint` digests the status lines plus `git diff` and
`git diff --cached` (with `--binary`, or a changed archive reads as the content-free "Binary files
differ"; `--no-textconv`/`--no-ext-diff`, so a repository driver can neither mask bytes nor execute;
`--ignore-submodules=dirty`, so it never descends into an agent-writable child) plus every untracked
file's content, read directly so an untracked SYMLINK is digested by where it points and never
followed off the checkout. Human decisions remain blocked; no-change outcomes never substitute for
unfinished work or a failed gate. Host-authorized audit rework retains its separate authority.

Assigned `tasks_complete` validates the binding before moving the folder so the same agent can
repair it. A successful exit after an ordinary no-commit refusal gets one fresh attempt only when
HEAD did not move, the entire source checkout is clean, and raw history has no binding to recover.
A second clean refusal is parked with its evidence and an unanswered decision; other tasks continue.
Changed history or dirty source stops instead: resetting the iteration base there could omit the
original diff from protected-gate review and signoff. The final report distinguishes blocked from
completed work. See [[loop-completion-refusals-keep-work-moving]].

## Changelog
- 2026-09-22 — the no-change guard compares CONTENT, not status labels: a same-status rewrite of
  inherited dirty/staged/untracked work used to complete without review.
- 2026-09-22 — aligned stale refusal, recovery decision and state guidance with the existing
  explicit no-change API. Same-session MCP repair now proves refusal followed by evidence-backed
  no-change completion without advancing HEAD. Preserved unrelated staged work and denied changed
  history, new dirty work and existing task bindings. Implementation-repair fixtures use real edits.
- 2026-09-13 — repaired shared resume/staging guidance after ignored-task-folder Git failures;
  removed blanket discard and casual worktree advice. Prompt and hermetic Git recipe tests cover
  foreign index/content preservation and preflight refusal before restoration hooks.
- 2026-09-12 — rechecked normal completion versus audit authority; documented meaningful no-code
  decisions, pre-move tool feedback, and clean no-commit recovery. Committed interruption remains
  distinct and cannot use the no-change retry path to hide its original diff.
- 2026-09-03 — re-verified the interrupted-completion path after deleting the retired audit-history
  algorithm and refreshed the shifted `RestoreBackgroundHandoffCompletion` citation; behavior is
  unchanged.
- 2026-08-01 — created: traced the leaked-descendant → un-complete → deep-amend chain after it rewrote 283 and nearly 286 commits of emisar's main; gated the amend recipe on boundTaskCommitIsHead.
- 2026-08-03 — added the uncommitted-interruption case: a hard-killed task left ~13h of work in the tree with a state.md still reading "not started"; resumePrefixFor now points a resumed agent at the stranded diff instead of letting it start over.
- 2026-08-10 — sources repointed: `controller.go` moved to `internal/tasks/audit.go` whole (the
  2026-08 tasks/lease/completion-audit extraction, ~20.5k lines); `resumePrefixFor` and
  `restoreBackgroundHandoffCompletion` are now `tasks.ResumePrefixFor`/`tasks.RestoreBackgroundHandoffCompletion`. Facts unchanged.
- 2026-08-10 — sources repointed: the loop engine moved out of `internal/cli` into `internal/loop` (`commands.go`'s loop half → `loop.go`, `classifyIteration` → `ratelimit.go`);
  re-verified the chain — `resumeLine`/`boundTaskCommitIsHead` stayed in `internal/tasks/audit.go`, unchanged.
- 2026-09-05 — corrected the descendant timeout default to 900s (verified against `handoff_wait` in image.go).
