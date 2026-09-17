---
name: audit-findings-need-practical-impact
description: audit realistic agent access and practical impact; avoid speculative machinery and unproven product-defect claims
scope: agent-workflow
sources: [AGENTS.md]
check: none
updated: 2026-09-15
---

# Require a realistic actor, reachable path, and meaningful impact for audit work

Show what the agent controls, the reachable operation, and what additional access or protected
data it gains. Do not assume prior host/root compromise to justify sandbox repairs. A deliberate
repeatable attack is not dismissed because it would be an unlikely accident. Keep real secret
and data-loss boundaries.

Withdraw disproportionate prescriptions for harmless narrow-window cache races and attacks on
data the actor can already delete. Reuse existing admission, read and mutation helpers before
proposing immutable workspaces, locks across human prompts, or a new approval framework. Review
additional access, not ordinary source-code edits.

Stay on one claimed task until it is finished or genuinely blocked. Do not claim a batch of tasks
to create the appearance of progress. For defect fixes, implement common real paths first and do
not add machinery for rare, low-impact cases whose failure is harmless.

A failed test establishes a failed test. Preserve safe evidence and find its cause before
prescribing provider rewrites, concurrency redesigns, or claiming repository corruption. Expired
test credentials are a validation prerequisite. User-approved CLI acceptance remains scope even
when cosmetic; do not replace it with the reviewer's taste.

**Why:** the user asked to remove overengineering and unlikely low-impact findings after the
audit overstated several claims and proposed approving script contents.

**How to apply:** retain a per-finding keep/narrow/withdraw/merge record. Remove withdrawn
implementation checkboxes without marking them fixed. State source/reproduction boundaries.
Broader proposals still need approval; pruning a task is not permission to weaken runtime checks.
Compatibility is practical impact too: before recommending an internal-only service topology,
check common runtime dependencies such as SaaS APIs, OIDC keys, object storage, email and webhooks.

When the human scopes a batch to fixing existing behavior, treat that as a maintenance boundary:
repair current integrations and measured performance without adding new runtimes, products or
configuration frameworks. Reuse existing queue tasks instead of duplicating them. Record explicit
exclusions in affected tasks and park excluded runnable work through the task CLI; preserve
unrelated prior authorizations. Task authoring is not implementation, and a speedup claim needs
matched before/after evidence without skipped features, safety checks or incomplete cleanup.

## Changelog

- 2026-09-15 — applied the maintenance-only correction while sweeping the current 15 todo tasks
  and two backlog items. Reused nine provider tasks and the existing Podman-removal task, parked
  the Apple-controls task, removed Apple live-qualification scope from Docker preflight, and
  added six source-mapped lifecycle performance tasks. No microVM task was present or added;
  unrelated eval, quota and credential work was retained. Scope and performance-evidence
  judgments remain review-only, so check stays none.

- 2026-09-14 — re-read AGENTS after the user corrected broad in-progress task claiming and
  speculative edge-case machinery. Added the one-active-task/common-real-path rule; the current
  task queue contract and boring-first creed already enforce it, with no sibling source change.

- 2026-09-13 — added the service-compatibility correction: a security fix must preserve common
  approved outbound dependencies rather than assuming every sidecar is a database or cache.

- 2026-09-12 — swept AGENTS.md's boring-first/root-cause/verification rules and the 43-ID audit.
  Withdrew S7/S14/S16/F11, merged duplicate test work and corrected overstated scan/session/loop
  claims in 2026-09-11-fix-defects-found-by-the-full-coop-feature-audit. Practical threat/impact
  judgment remains review-only; check is none.
