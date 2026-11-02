---
name: design-for-real-requirements
description: justify complexity with real impact and make design changes required by current behavior
scope: agent-workflow
sources: [.agent/skills/spec/SKILL.md, .agent/skills/work/SKILL.md, .agent/skills/review-board/SKILL.md, internal/scaffold/templates/skills/spec/SKILL.md, internal/scaffold/templates/skills/work/SKILL.md, internal/scaffold/templates/skills/review-board/SKILL.md]
check: none
updated: 2026-11-02
---

# Choose the simplest coherent design for the real requirement

Added complexity must serve current approved behavior, a reachable failure with meaningful
impact, or a concrete maintenance need. An impossible state needs an identified invariant;
unknown reachability needs investigation. Leave harmless hypothetical cases and speculative
extensibility alone. Low probability does not dismiss credible security or data-loss failures.

When a requirement invalidates the existing model, compare extending it with the design that
would fit if the requirement had existed from the start. Include migration and compatibility
costs. Make the focused redesign when it gives the simpler correct result; tiny diffs and fewer
changed files are not substitutes for a coherent design or full acceptance.

Performance changes must improve the complete user workflow enough to justify their ongoing
cost. Before retaining a cache, daemon or warm pool, measure the actual critical path, idle
memory and CPU, rebuild peaks and scaling across projects. Bound retained resources and explain
invalidation, ownership and cleanup. A faster isolated step is not a product improvement when
it was already overlapped, shifts comparable cost to shutdown, or weakens fresh authority.
Reject a prototype when its measured benefit does not earn its complexity.

**Why:** the user clarified that pragmatic agents must avoid unnecessary edge-case machinery
while still doing the design work a new requirement needs.

**How to apply:** use this tradeoff in planning, implementation and review. Explain necessary
plan changes and carry them through existing authority; ask only about new product/security
decisions or changes to scope or explicit contracts the user has not authorized. This complements
[[audit-findings-need-practical-impact]] and [[requested-outcome-controls-stopping]].

## Changelog
- 2026-10-28 — swept 16 instruction files: eight repo skills, six scaffold skills and both
  AGENTS files; also read the two related rules. Replaced the one-caller abstraction ban and
  file-count emphasis in both spec copies, clarified design-change authority in both work copies,
  and added realistic-impact plus undersized-patch review in both review-board copies. Other
  instruction sources had no conflicting mandate. Design fitness and reachability remain
  review judgments, so the check is none.
- 2026-11-02 — required measured lifecycle benefit and bounded idle cost for performance
  designs. Swept planning, work and review-board skills in the repo and scaffold (six files);
  added the evaluation to both planning copies. Execution/review already weigh complexity and
  preserve correctness. Critical-path attribution and acceptable cost require judgment, so
  the check remains none.
