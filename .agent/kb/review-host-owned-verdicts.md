---
name: review-host-owned-verdicts
description: reviews report bounded evidence; Coop alone applies validated task lifecycle changes
subsystem: box
sources: [internal/box/run.go, internal/cli/commands.go, internal/tasks/audit.go, internal/cli/loopchanges.go, internal/cli/streamjson_providers.go, internal/tasks/cmd.go, internal/tasks/lease.go, internal/tasks/queue.go, internal/cli/util.go, internal/loopcfg/loopcfg.go]
updated: 2026-08-10
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
block. Because stdout/stderr collection can place that footer outside the response tail, the host
verdict boundary also collapses one earlier normalized evidence+receipt envelope only when it is
byte-identical to the terminal envelope. A non-identical, partial, or additional proposal remains
ambiguous and is rejected.

Each accepted reopen also writes one random generation in the host-only task-authority registry.
Current records anchor the full baseline HEAD and every ordered later commit, whether task-bound or
unbound: exact introduced paths/modes/blob ids, author identity/date, and complete message,
excluding parent and committer identity. Task IDs are ownership metadata, not a history filter.
Lease-start validation permits a later host suffix only after the exact recorded prefix; completion
snapshots that whole base sequence and requires the rewritten subject first followed by an exact
semantic replay. Dropped, changed, reordered, or invented unbound commits therefore fail just like
task-bound descendants. A rewritten subject must also retain the reviewed subject's exact raw sole
parent (or remain a root). Hardened Git reads ignore replacement objects. Authority capture and use
walk the complete reachable commit DAG from raw objects, so graft and shallow metadata cannot hide
older duplicate bindings. Subject and replacement validation then walk raw sole-parent chains from
their exact terminals. Semantic change hashes compare explicit raw parent/child trees, never a
traversal-selected parent. Parent headers must be canonical and contiguous, every named parent must
resolve to a commit, and raw commit reads are type-constrained, size-bounded, and locally
content-hashed before use. Raw authority trailers use a fixed final-paragraph grammar independent
of repository trailer configuration. Linear and complete-DAG walks have aggregate byte/work
budgets. Tree objects likewise have per-object and aggregate object/byte/entry caps, accept only
canonical modes, and are content-hashed into a host-private bare snapshot; semantic diffs read only
that immutable snapshot, never the mutable agent repository. Resets, replacement refs, baseline or
replacement grafts, shallow boundaries, malformed objects, oversized commits or trees, and
non-commit parents therefore fail closed without discarding ancestry or blocking the batch reader.
The generation may still re-close an independently disproved finding
without changing Git. A duplicate/redirected binding, message-only receipt rewrite, or task-local
forgery is rejected. Failed attempts retain the generation;
when a validated rewrite parks the task blocked for external acceptance, the still-held lease
rebases that same generation to the rewritten subject while retaining the descendant baseline.
Unblocking therefore resumes the same single-use authority rather than requiring another rewrite.
The leased work prompt and any rejection remedy also select from that host authority: an
audit-reopened task must independently verify the finding, then either re-close without a commit
or make a real tree-changing rewrite, and must never use the ordinary crash-recovery receipt path.
Before building that prompt, the loop requires the leased record to match current HEAD exactly.
A mismatch launches no provider and parks the task blocked with manual guidance to restore the
audited pre-attempt history; cycling the task state alone cannot repair stale Git history.
Complete active/pending records are versions 3/4. Their blocked recovery uses the exact persisted
baseline HEAD as its only candidate, then validates the rewritten subject plus full replay before
allowing a later host suffix. Versions 1/2 decode for diagnostics but authorize no lease,
completion, activation, or automatic upgrade: they omitted an old HEAD anchor and all unbound
history. After restoring the audited pre-attempt HEAD, a human must run
`coop tasks unblock <id> --adopt-audit-head <full-sha> "<answer>"`; Coop requires that exact SHA and
the legacy subject/bound projection, captures complete history, and retains the generation. The
transaction first persists a non-authorizing v4 form, moves the folder, then activates v3. Both a
pre-move blocked task and a post-move todo task require explicit host recovery; a lease never
self-activates pending authority. Provider-written resolution prose cannot invoke adoption, and
changed records, direct folder moves, overflow, or semantic mismatches remain inert.
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

- 2026-08-10 — sources repointed: `controller.go`/`taskcmd.go`/`tasklease.go`/`tasks.go` moved to
  `internal/tasks/audit.go`/`internal/tasks/cmd.go`/`internal/tasks/lease.go`/`internal/tasks/queue.go`
  (the 2026-08 tasks/lease/completion-audit extraction); `internal/cli/util.go` unchanged (the moved
  package's own `internal/tasks/util.go` carries a separate local copy of the trivial helpers it
  needed). Facts unchanged.
- 2026-07-26 — pinned audit rewrites to the reviewed subject's raw sole parent (including root),
  raw-walked reachable bindings plus the persisted baseline and complete replacement sequence,
  parsed fixed-grammar bindings and metadata from locally hashed commit bodies, snapshotted locally
  hashed and bounded tree DAGs before diffing, ignored replacement/graft/shallow traversal
  metadata, and rejected dropped ancestry, decoys, forged or malformed parents, dangling objects,
  non-commit parents, and oversized input in direct and blocked recovery.
- 2026-07-26 — replaced the task-bound descendant projection with baseline-anchored complete
  ordered history, including unbound commits; v1/v2 are inert until exact-HEAD host adoption, and
  v3/v4 recovery uses only the persisted baseline.
- 2026-07-26 — made leased work prompts and rejected-completion remedies distinguish
  host-authorized audit rework from ordinary commit-bound crash recovery; exact-HEAD preflight
  launches no provider and parks stale generations for explicit history restoration.
- 2026-07-26 — explicit host unblock transactionally upgrades a pre-existing stale blocked
  generation from a bounded exact reflog baseline through the current rewrite terminal, then
  validates current HEAD; a downgrade-safe, non-authorizing pending form makes both crash
  boundaries explicitly recoverable and fail-closed, while preflight/direct moves, leases, and
  authority-inspection errors remain inert.
- 2026-07-26 — accepted one exact normalized evidence+receipt echo at the host verdict boundary,
  including a Codex footer routed on the other stream; verified against conflict, partial,
  additional, subject-scope, and external process cases.
- 2026-07-25 — preserved audit-reopen authority across a validated rewrite that settles blocked;
  verified through real unblock and verification-only completion with an older subject plus
  descendant.
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
