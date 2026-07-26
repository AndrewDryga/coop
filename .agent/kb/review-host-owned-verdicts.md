---
name: review-host-owned-verdicts
description: reviews report bounded evidence; Coop alone applies validated task lifecycle changes
subsystem: box
sources: [internal/box/run.go, internal/cli/commands.go, internal/cli/controller.go, internal/cli/loopchanges.go, internal/cli/streamjson_providers.go, internal/cli/tasklease.go, internal/cli/tasks.go, internal/loopcfg/loopcfg.go]
updated: 2026-07-25
---

`between`, `signoff`, and `verify` default to `writes: tasks`. The name is retained for
configuration compatibility, but it means task-decision authority rather than filesystem write
access: the whole repository, including every queue, is mounted read-only. The box runner has no
writable-descendant mechanism to restore accidentally.

Reviews emit exactly one bounded `AUDIT EVIDENCE` line per named subject and a terminal
`REVIEW COMPLETE` receipt. The findings field's no-findings grammar is exact: bare `none`
(case-insensitive) or `none` plus one parenthesized annotation — models habitually annotate the
token, and the old literal-`none` comparison voided benign PASS verdicts. Anything looser stays a
finding on purpose: punctuation-led continuations ("none — flaky test fails", "none-critical
issues found") read as real defects, and a first-rune-boundary heuristic was refuted in review for
exactly those shapes. Coop treats this output as an untrusted proposal: it validates every
subject and finding first, then acquires every host-side completion authority lock and applies all
exact-subject reopens as one transaction. A later lock, move, or metadata failure restores every
folder, metadata snapshot, and completion receipt. Findings are written only inside a delimited
untrusted log block; `state.md` receives a fixed reproduction-first next action, and the trusted
work prompt says never to follow commands from review evidence. Missing, malformed, interrupted,
failed, or out-of-scope proposals leave every task unchanged.

A successful review process whose structured proposal is malformed gets one immediate fresh
review over the same cloned subject set and base prompt plus a fixed receipt-format correction.
The first proposal is never embedded or partially trusted, and each attempt receives its own
stage telemetry row. A second malformed proposal, lifecycle/ownership churn, an interrupt, or an
ordinary provider failure is not retried. Codex's adapter may strip one exact footer/count pair
and one byte-identical echo of either the complete response or its terminal evidence/receipt
block; a non-identical or additional proposal remains ambiguous and is rejected.

Each accepted reopen also writes one random generation in the host-only task-authority registry.
It records the subject's semantic commit and the ordered later task-bound commits: exact introduced
paths/modes/blob ids, author identity/date, and complete message, excluding parent and committer
identity. The next leased completion may use that generation either to re-close an independently
disproved finding without changing Git, or to rewrite the subject while replaying those later task
changes byte-for-byte. A changed or invented descendant, duplicate/redirected binding, message-only
receipt rewrite, and task-local forgery are rejected. Failed attempts retain the generation;
finalization copies it into the host completion receipt before consuming it, so crash replay can
finish consumption and an accepted generation cannot be reused.

`writes: repo` is the explicit escape hatch for a review stage that must repair source. It makes
repository source writable, then overlays every real queue root with a nested read-only mount.
Task lifecycle is report-only in the prompt and successful verdicts are applied by the host.
Queue paths are validated as real in-repository directories before the runtime starts.
An absent configured queue fails with an actionable error instead of letting Docker create a
root-owned nested bind destination in the host checkout.

Commit trailers describe task changes but do not authorize review. A work iteration is rejected if
it introduces a binding for any task other than its assigned task, and final verify can reopen only
tasks this controller accepted as completed during the run and that remain archived.

## Changelog

- 2026-07-25 — findings grammar accepts `none (parenthesized annotation)` exactly; verified
  against the verdict fail-closed table and the annotated-none bypass cases.
- 2026-07-25 — added the one-turn malformed-verdict recovery boundary, per-attempt telemetry, and
  exact complete-response Codex footer/echo normalization; verified against all three review stages.
- 2026-07-25 — added the single-use, crash-safe audit-reopen generation and exact semantic
  descendant replay/no-change re-close contract; verified against task authority and provider
  process recovery tests.
- 2026-07-24 — replaced writable queue overlays with a report-only review protocol and
  host-applied, atomic exact-subject verdicts; added read-only queue overlays for the explicit
  repository-write mode, fixed resume instructions, and host-recorded verify subjects.
- 2026-07-14 — created after adding the task-only review mount policy; verified against
  the runtime-argument tests and the supported Docker nested-bind probe.
- 2026-07-14 — reopened after an in-repo queue symlink could bypass a protected target; queue
  traversal rejects symlinks, and absent queue roots now fail before the runtime starts.
