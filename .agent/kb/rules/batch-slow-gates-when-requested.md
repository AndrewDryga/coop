---
name: batch-slow-gates-when-requested
description: explicitly requested sweep batching keeps focused checks per task and defers slow suites to one final qualification
scope: agent-workflow
sources: [AGENTS.md, .agent/skills/work/SKILL.md, .agent/skills/sweep/SKILL.md, Makefile]
check: none
updated: 2026-09-13
---

# Honor explicit requests to batch slow sweep gates

When the human explicitly asks to defer slow gates until the implementation queue is
closed, run focused behavioral tests and quick checks per task, then run the slow suites
once against the combined result. A generic request to hurry is not that authorization.
Ordinary per-task gate defaults and the product's CI checks remain unchanged.

Keep a durable final-verification ledger before closing the first affected task. Record
each task/commit, its focused evidence and its still-required slow or native checks.
Task closure under this explicit direction means implementation is finished, not that
deferred verification passed. The overall requested outcome remains open until final
qualification passes; failures still need repair and relevant rechecks.

**Why:** the user corrected repeated whole-repository gate runs during a sweep:
"skip all slow gates to the final check after all tasks are closed".

Follow [[static-bounded-supervision]] for logs and [[requested-outcome-controls-stopping]]
for the final stop condition. Batching never supplies missing access, review or destructive
operation approval. Do not change CI or runtime acceptance checks to speed agent supervision.

## Changelog
- 2026-09-13 — swept AGENTS, work/sweep skills, Makefile and both linked workflow rules:
  six surfaces retain ordinary gating/final verification and need no default change.
  Recorded the explicit scheduling exception and durable deferred-proof obligation;
  whether the human authorized batching is a review judgment, so check is none.
