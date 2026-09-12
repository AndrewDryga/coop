---
name: loop-completion-refusals-keep-work-moving
description: ordinary completion mistakes get immediate feedback and bounded safe repair without weakening task acceptance
scope: loop
sources: [internal/loop/loop.go, internal/loop/completion.go, internal/loop/prompts.go, internal/taskmcp/tools.go, internal/tasks/completion_recovery.go, internal/tasks/completion_checklist.go, internal/tasks/projection.go, internal/tasks/candidate.go, internal/cli/scripted_loop_completion_process_e2e_test.go]
check: "go test ./internal/tasks -run 'TestUncommittedCompletionCanRetry|TestParkUncommittedCompletion|TestTrustedCompletionRequiresCurrentChecklist|TestIncompleteForkCompletionRefusesAcceptanceButCanResume|TestForkChecklistCheckedAgainAtPublicationAndLanding'"
updated: 2026-09-12
---

# Recover ordinary completion mistakes without losing work or acceptance

An unattended loop is responsible for continuing across recoverable agent mistakes by default.
Return a completion refusal while the agent can still fix it, before moving the task to done.
Keep the post-exit host audit authoritative: feedback is not acceptance.

Completion requires a nonempty, fully checked current checklist. Check it before official
completion mutations, including captured fork acceptance and candidate landing; do not place the
guard on raw recovery moves or prevent an unfinished projection from resuming. Failed,
unavailable, and never-attempted required checks remain open. Recording pending verification is
not equivalent to passing it, including in proposed task acceptance. Checked boxes are a
structural prerequisite, not independent evidence that verification ran.

If a successful attempt ends after an ordinary no-commit completion refusal, allow one fresh
repair only when HEAD is unchanged, the checkout/index/untracked set is clean, and raw history
has no existing binding. Park a repeated clean refusal with an unanswered decision and continue
the remaining queue. A blocked task is unfinished, including in telemetry and the final verdict.

Never advance the audit base over unbound code, hide dirty work from the next task, loosen task
ownership or raw-history checks, or accept failed/unrun required gates. A legitimate no-code
decision uses one meaningful decision commit only when the task's acceptance permits that outcome;
it is not a shortcut for incomplete implementation. Audit rework retains its separate authority.
Remember a completion attempt even when its binding precheck passes: a later checklist refusal
must not disappear after provider exit. A safe stop for unfinished committed work includes the
current checklist count and required-check action, without claiming the task completed.

**Why:** on 2026-09-12 the human's overnight Emisar run stopped when an agent completed a permitted
log-only decision without a task-bound commit. “Run this command once and have agent work all
night” is a core product promise, not an optional recovery flag.

**How to apply:** use the assigned task's existing host lease and bounded recovery path, preserve
its evidence, and name the refusal, retry, or blocking action in readable terminal output. Test
same-session MCP repair, subprocess retry/parking/next-task progress, signoff and the blocked exit
code. The check above pins the conservative recovery boundary; the tagged scripted process suite
also runs in `make check` and proves continuation and both terminal modes.

## Changelog
- 2026-09-12 — live Emisar tasks completed with explicit open runtime checks. Swept assigned MCP,
  trusted host/CLI, queued finalization, and fork acceptance/publication/landing; added the shared
  fresh-checklist prerequisite. Recovery still reopens unfinished projections. Also observed
  generated tasks offering pending verification as success; tightened guidance without claiming
  to attest test execution or interpreting arbitrary prose as authority. Controller regression
  retains post-binding refusals, and the terminal safe stop reports their count and repair.
- 2026-09-12 — swept the six source surfaces: completion feedback arrived only after provider exit,
  ordinary no-code acceptance lacked a commit recipe, and every binding refusal stopped the run.
  Fixed here; raw-DAG and dirty-tree refusal tests remain fail-closed. Process tests prove one
  repair, honest parking, next-task completion, final review, and static versus live terminal UI.
