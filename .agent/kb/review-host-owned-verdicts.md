---
name: review-host-owned-verdicts
description: reviews report bounded evidence; Coop alone applies validated task lifecycle changes
subsystem: box
sources: [internal/tasks/review_dispute.go, internal/box/run.go, internal/loop/review.go, internal/loop/receipt.go, internal/loop/pending_review.go, internal/tasks/audit.go, internal/tasks/pending_review.go, internal/loop/changes.go, internal/loop/streamjson_providers.go, internal/tasks/cmd.go, internal/tasks/lease.go, internal/tasks/queue.go, internal/loopcfg/loopcfg.go]
updated: 2026-11-06
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

An audit reopen carries its full bounded finding in host authority. Accepted re-completion copies
it into pending review with whether the worker's validated attempt left HEAD unchanged. A repeated
finding after that zero-commit close moves directly to blocked with both the finding and the
worker's state explanation in `decision.md`. Matching normalizes case, whitespace and terminal
periods; it never matches truncated display prefixes. Oversized findings reopen normally.
Known earlier repairs and ambiguous unbound commits after the reopen suppress automatic dispute
blocking; tagged sibling commits do not. The dispute names the exact completion receipt, so
startup can retire its review debt after an interrupted host move. Explicit human unblock resets
the finding. Blocked subjects are neither accepted nor scheduled for another review round.
A missing-evidence finding requires the evidence even for a zero-commit re-close.

A successful review process whose structured proposal is malformed gets one immediate format-only
correction in the same native provider session. Coop returns the exact validator errors and a
subject-specific response skeleton without remounting the repository, restarting services, or
rerunning its gate. Harmless blank separators are normalized locally. The first proposal is never
partially trusted; a second malformed proposal, lifecycle/ownership/source churn, an interrupt, or
an ordinary provider failure is not retried. Codex's adapter may strip one exact footer/count pair
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
allowing a later host suffix. The unblock transaction first persists a non-authorizing v4 form,
moves the folder, then activates v3. Both a pre-move blocked task and a post-move todo task require
explicit host recovery; a lease never self-activates pending authority. Unsupported versions,
unknown fields, changed records, direct folder moves, overflow, and semantic mismatches fail
closed without changing or discarding the host record. Provider-written resolution prose cannot
invoke the host transition. Finalization copies the generation into the host completion receipt
before consuming it, so crash replay can finish consumption and an accepted generation cannot be
reused.

Other work keeps landing while a reopened task waits, and main's history holds what the exact
replay refuses: one task bound by several commits, one commit closing two tasks. Any such commit
after the baseline once kept the task blocked for good (2026-11-06: 60 commits, one task bound four
times). A person's unblock now moves the record past them (`movedAuditReopen`) when HEAD reaches
the end of the reviewed history in a straight line, no commit after it names the task or a task of
its History in any `Coop-Task` trailer, and Git binds neither of them to any other commit. That last
check reads Git's own trailers (`auditReopenGitBindings`) on every check of every record, moved or
not: Git skips trailing `#` comment lines and everything after a scissors line, which the raw parse
takes as the message's last paragraph, so a commit can bind a task for Git alone, such as a repair
after an ordinary record's baseline that the raw parse reads as unbound.
Subject and History stay as reviewed, their end is kept as `ReviewedHead`, and `BaselineHead` moves
to HEAD. Those accepted commits are never replayed, so a moved record allows only a zero-commit
re-close or one appended repair, never a rewrite: the rewrite check compares only the history the
record still replays and would accept one that drops them all (`TestMovedAuditAuthorityAllowsNoRewrite`).
The accepted commits are pinned only by `BaselineHead`, a commit of other work that the loop's
signing pass or an amend can rewrite. So a moved record moves again from `ReviewedHead` at the next
unblock, and an accepted repair stays a later commit rather than becoming the baseline. That also
means a person's unblock accepts a history whose accepted commits were dropped or changed, as long
as nothing names the task; the record then stays moved (it never regains rewrite authority). The
host's signing carry (`carriedMovedAuditReopen`) follows a moved record only across a rewrite that
kept every accepted commit's change, author and message, and moves the baseline to its own
rewritten copy, never further. Commits after the baseline get the strict check as before. It is
the only check a failed audit-rework attempt gets against forged bindings (the loop skips
`AttemptBindingsRefused` for audit rework), so a lease never moves a record by itself; a stale
lease parks the task with advice to try the unblock first. A merge after the reviewed history still
refuses. The unblock saves the person's answer before any check, so a refusal never loses it, and
the refusal names what changed.

Open gap (2026-11-06): a moved record is consumed by its completion, and a later review that fails
the task again captures a new record with `CaptureAuditReopen`, which still needs every later
commit to replay exactly. That capture refuses the same history, so the review cannot reopen the
task, and the same holds for any task with such commits after its own.

`writes: repo` is the explicit escape hatch for a review stage that must repair source. It makes
repository source writable, then overlays every real queue root with a nested read-only mount.
Task lifecycle is report-only in the prompt and successful verdicts are applied by the host.
Queue paths are validated as real in-repository directories before the runtime starts.
An absent configured queue fails with an actionable error instead of letting Docker create a
root-owned nested bind destination in the host checkout.

Commit trailers describe task changes but do not authorize review. A work iteration is rejected if
it introduces a binding for any task other than its assigned task. Final signoff and verify can act
only on archived generations this controller accepted or exact host-private pending-review records
that revalidate their completion receipt and task/queue identity and still find the task's one
commit by trailer after the host-recorded run base (or after its own anchor, for a host completion
committed before the run), and show that anchored commit to the reviewer. A verdict covers the
content its round recorded, every commit in the subject's review range up to HEAD, so verification
never clears a commit that changed after signoff, an untrailered repair included; that task goes
back to signoff. They never derive authority by scanning historical archives.

## Changelog

- 2026-11-06 — every check of an ordinary record reads Git's trailers too, not only a moved
  record's (task 2026-11-06-make-every-audit-reopen-check-read-git-s-trailer).
- 2026-11-06 — a person's unblock moves a reopened task's record past commits that leave the task
  alone, again from the reviewed history after its baseline was rewritten, and the signing carry
  follows a moved record; the unblock saves the answer before any check and a stale lease advises
  unblocking first; recorded the review-reopen capture gap (task
  2026-11-06-let-the-owner-unblock-a-reopened-task-after-unre).

- 2026-11-05 — added repeated-finding human handoff; traced host move rollback, pending completion
  rollback, blocked debt recovery, worker attempt provenance and all three ordinary review stages.

- 2026-10-17 — the recorded content now covers the whole review range up to HEAD (third review).
- 2026-10-17 — the recorded content covers the commits after the subject's too (an untrailered
  audit repair), and an anchored commit joins the reviewer's change block and diff.
- 2026-10-17 — a round records the reviewed commit content; a changed commit goes back to signoff,
  and a host completion from before the run is anchored at its one commit in the branch.
- 2026-10-16 — pending-review records no longer pin a raw binding; they find the task's commit by
  trailer after the host-recorded run base (see [[signoff-scope-is-run-anchored]]).
- 2026-09-14 — replaced the obsolete fresh-review recovery description with the same-session,
  format-only correction boundary and its frozen-state/no-repeated-setup constraints.
- 2026-09-13 — final review authority now persists exact accepted generations across process stops;
  resumed and explicitly imported subjects retain the same host-verdict boundary without granting
  archive-wide authority.
- 2026-08-26 — removed support for the unshipped v1/v2 descendant formats and the now-unused
  manual adoption bridge; active v3 and pending v4 are now the only accepted formats, with
  unsupported versions and unknown fields rejected fail-closed.
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
- 2026-08-10 — sources repointed for the loop-engine extraction: the review pipeline moved out of
  `internal/cli/commands.go` into `internal/loop/review.go` (running a stage) + `receipt.go` (parsing
  the receipt and applying the verdict), `loopchanges.go` → `internal/loop/changes.go`,
  `streamjson_providers.go` → `internal/loop/`; `internal/cli/util.go` dropped from sources — the new
  package carries its own local copies of the trivial helpers. Host-owned verdict contract unchanged.
