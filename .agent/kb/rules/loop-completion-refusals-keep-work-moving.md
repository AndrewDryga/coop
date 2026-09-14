---
name: loop-completion-refusals-keep-work-moving
description: ordinary completion mistakes get immediate feedback and bounded safe repair without weakening task acceptance
scope: loop
sources: [internal/loop/loop.go, internal/loop/completion.go, internal/loop/prompts.go, internal/taskmcp/tools.go, internal/tasks/completion_recovery.go, internal/tasks/completion_checklist.go, internal/tasks/projection.go, internal/tasks/candidate.go, internal/tasks/pending_review.go, internal/tasks/pending_review_test.go, internal/cli/scripted_loop_completion_process_e2e_test.go]
check: "go test ./internal/tasks -run 'TestUncommittedCompletionCanRetry|TestParkUncommittedCompletion|TestTrustedCompletionRequiresCurrentChecklist|TestIncompleteForkCompletionRefusesAcceptanceButCanResume|TestForkChecklistCheckedAgainAtPublicationAndLanding|TestPendingReviewRebindAllowsAuthorizedRewriteOfLaterCohortTask|TestPendingReviewRecoversAuthorizedRewriteOfLaterCohortTask'"
updated: 2026-09-15
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
ownership or raw-history checks, or accept failed/unrun required gates. An already-satisfied task
or an inconclusive investigation records an explicit no-change outcome with reason and evidence;
it never fabricates an empty commit or calls unfinished implementation complete. Human choices use
the existing blocked state. Audit rework retains its separate authority.
Remember a completion attempt even when its binding precheck passes: a later checklist refusal
must not disappear after provider exit. A safe stop for unfinished committed work includes the
current checklist count and required-check action, without claiming the task completed.

Final-review repair is cohort-aware. Rewriting one reopened subject must retain every sibling's
review debt. When the repaired subject is a descendant in an earlier sibling's recorded history,
rebind that one entry to the host-authorized replacement and require every other recorded semantic
entry to remain unchanged. Do not reject the valid repair merely because that authorized entry
changed, and do not reset or silently clear the sibling's pending review.

On restart, explicit review-model selections in the current loop configuration replace the
stored models. Preserve the saved acceptance prompts, permissions, verification requirement,
and round limit; absent model selections still use the stored targets. A model change must not
discard pending review or silently resume an unwanted provider.

**Why:** on 2026-09-12 the human's overnight Emisar run stopped when an agent completed a permitted
log-only decision without a task-bound commit. “Run this command once and have agent work all
night” is a core product promise, not an optional recovery flag.

**How to apply:** use the assigned task's existing host lease and bounded recovery path, preserve
its evidence, and name the refusal, retry, or blocking action in readable terminal output. Test
same-session MCP repair, subprocess retry/parking/next-task progress, signoff and the blocked exit
code. The check above pins the conservative recovery boundary; the tagged scripted process suite
also runs in `make check` and proves continuation and both terminal modes.

## Changelog
- 2026-09-15 — replaced fake decision commits with explicit evidence-backed already-satisfied,
  could-not-reproduce and won't-fix completion outcomes. The host requires unchanged iteration
  history and checkout state; normal implementation still requires one task-bound commit.
- 2026-09-14 — a user-requested Opus-only drain restored Codex from an older pending review
  despite explicit Opus stage configuration. Swept signoff and verify resume selection; the
  focused `TestPendingReviewHonorsConfiguredModelsWithoutChangingAcceptance` regression covers
  replacement, absent selections, and preservation of the saved acceptance contract.
- 2026-09-14 — a real Emisar Frontier review repaired the later task in a two-task pending cohort,
  then Coop rejected the earlier task's still-valid review debt because its recorded descendant was
  required to remain unchanged. Swept the audit-rewrite rebind path and pinned the exact authorized
  descendant replacement while retaining all other semantic checks.
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
