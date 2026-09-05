---
name: design-for-shared-checkout-not-forks
description: "the primary workflow is outside agents and `coop loop` sharing one checkout; forks are an optional isolation tool, never the precondition a design or recommendation assumes"
scope: architecture
sources: [AGENTS.md, README.md, internal/cli/help.go, .agent/kb/task-authority-model.md]
check: "none"
updated: 2026-09-05
---

# Design for outside agents and `coop loop` sharing one checkout; forks are optional

The human's day-to-day is a `coop loop` in the main checkout plus agents driven from outside coop
(Zed's Codex and Claude sessions, the plain provider CLIs) working the same queue and the same tree.
A feature, a coordination mechanism, or a recommendation must work in that setting. `coop fork` is
an isolation tool a user may reach for; it is never the default answer, the assumed context of a
design, or the fix for a coordination gap.

**Why:** on 2026-09-05 the audit answered "how do parallel Codex sessions coordinate with the loop?"
with "run them as forks" twice, and the human corrected it: "I don't use forks usually — we should
not optimize design around them; I use outside agents and `coop loop` most of the time." Task-level
coordination (claim, lease, labels) has to be right for the shared checkout, and tree-level hazards
have to be mitigated there too, not delegated to a workflow the human does not run.

**How to apply:**
- When a coordination gap appears (adoption collisions, misleading labels, exclusion between
  agents), fix it in the claim and lease authorities every writer already shares — see
  [[task-authority-model]] — never by requiring a fork.
- Docs and help may present forks as an option; they must not present them as the prerequisite for
  parallel work, and advice to the user starts from the shared-checkout case.
- A design note that says "use a fork" for the common path is a smell. Say instead what the loop
  and an outside agent each need to do in one tree: claim before work, hold the lease while working,
  stage exact files, commit small and often.

## Changelog
- 2026-09-05 — created from the human's correction during the pre-release audit. Swept AGENTS.md,
  README, site, and help: forks appear as an option (the parallel-forks sections, `coop fork … --loop`)
  and nothing presumes them; the violation was the audit's own recommendation text, and the two
  coordination tasks queued that day are framed for the shared checkout.
