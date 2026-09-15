---
name: requested-outcome-controls-stopping
description: full implementation requests continue past verified slices through the requested final acceptance criteria
scope: agent-workflow
sources: [AGENTS.md, .agent/skills/work/SKILL.md, .agent/skills/spec/SKILL.md, .agent/skills/sweep/SKILL.md, internal/scaffold/templates/skills/work/SKILL.md, internal/scaffold/templates/skills/spec/SKILL.md]
check: none
updated: 2026-09-15
---

# Let the requested outcome control when work stops

When the human asks for a feature to be fully implemented and verified, keep working through its
approved acceptance criteria, including requested review, end-to-end tests, integration and docs.
A green internal slice is a progress checkpoint, not a final handoff. Do not silently convert
the request into a smaller delivery or describe unfinished acceptance criteria as optional next steps.

The task title, context and the human's requested outcome bound acceptance. Acceptance criteria may
sequence a first slice, but may not redefine that broader outcome as complete. If work is split into
children, keep the broad parent open until every child and its final verification are complete. A
completed child is not evidence that the umbrella is complete, and a later ledger with open required
checks is direct evidence that it is not.

A request to finish faster changes execution, not the approved scope or verification bar. Optimize
delegation, dependency ordering and feedback latency; do not substitute a smaller first release for
full implementation. Staged delivery is a sequencing tool, not permission to drop later stages.

**Why:** after an internal networking slice was verified but public integration and authenticated
qualification were unfinished, the human corrected: "You should not stop untill the full feature
is fully done".

**How to apply:** track the complete requested outcome in the task state. Gate each step and fix
red gates before proceeding. Continue into the next authorized step after green. Preserve scope
and authority boundaries: a genuine blocker requiring human input still warrants a concise question;
persistence never authorizes unrelated work, deployment, or destructive actions.

## Changelog
- 2026-09-15 — corrected a provider-credential task that kept a broad promise while narrowing
  acceptance to one Claude path, and an audit umbrella archived with two final ledger items open.
  Swept current completed tasks: newer checklists are structurally complete, while older unchecked
  boxes are legacy metadata and require scope/evidence review rather than automatic reopening.
- 2026-09-10 — tightened the wording of all four surfaces and re-synced both scaffold template
  copies, which a mid-task reset had left behind (`go test ./internal/scaffold` green again).
  Re-swept the six sources: 0 violations.
- 2026-09-09 — the operator clarified "shortest path to full implementation that is correct and
  tested". Swept all six source files; spec's unqualified instruction to move work to "later"
  could be misapplied after scope approval. Clarified both spec copies. Other four sources already
  preserve full completion. Task qualification/state now distinguish sequencing from completion.
- 2026-09-08 — swept the five source files; clarified AGENTS and both copies of work's premature handback language.
  Spec's pre-implementation approval and sweep's queue completion already have distinct legitimate
  stop conditions. The scaffold equality test caught the initially missed embedded skill; synced
  it and retained that regression gate. No further violations found. Whether a user outcome is
  met requires review.
