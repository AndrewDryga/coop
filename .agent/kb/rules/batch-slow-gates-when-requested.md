---
name: batch-slow-gates-when-requested
description: explicit quick-iteration requests use focused checks and defer unrelated slow suites to final qualification
scope: agent-workflow
sources: [AGENTS.md, .agent/skills/work/SKILL.md, .agent/skills/sweep/SKILL.md, Makefile]
check: none
updated: 2026-09-14
---

# Honor explicit requests for focused, quick verification

When the human explicitly asks to avoid long tests while iterating, run focused behavioral tests
and quick compile/static checks after each slice. At final qualification, run the smallest native
or slow checks that prove the changed boundaries; do not run unrelated provider matrices merely
because they exist. A generic request to hurry is not that authorization. Ordinary per-task gate
defaults and the product's CI checks remain unchanged unless the human explicitly narrows them.

Keep a durable final-verification ledger before closing the first affected task. Record
each task/commit, its focused evidence and its still-required slow or native checks.
Task closure under this explicit direction means implementation is finished, not that
deferred verification passed. The overall requested outcome remains open until final
qualification passes; failures still need repair and relevant rechecks.

**Why:** the user corrected repeated whole-repository gate runs during a sweep — "skip all slow
gates to the final check after all tasks are closed" — and later made the same boundary explicit
for defect iteration: "do not run very long tests in the coop itself ... we want to iterate quickly".

Follow [[static-bounded-supervision]] for logs and [[requested-outcome-controls-stopping]]
for the final stop condition. Batching never supplies missing access, review or destructive
operation approval. Do not change CI or runtime acceptance checks to speed agent supervision.

## Changelog
- 2026-09-14 — extended the existing explicit batching exception from queue sweeps to a focused
  implementation/qualification loop. Re-read AGENTS, work/sweep skills and Makefile: the normal
  gate remains authoritative, while the user's explicit direction permits focused iteration and
  the smallest relevant final native proof; unrelated live provider matrices stay in the ledger.
- 2026-09-13 — swept AGENTS, work/sweep skills, Makefile and both linked workflow rules:
  six surfaces retain ordinary gating/final verification and need no default change.
  Recorded the explicit scheduling exception and durable deferred-proof obligation;
  whether the human authorized batching is a review judgment, so check is none.
