---
name: batch-delegated-editorial-decisions
description: after editorial authority is delegated, finish coherent batches and ask only about material choices
scope: agent-workflow
sources: [AGENTS.md, .agent/skills/spec/SKILL.md, .agent/kb/rules/requested-outcome-controls-stopping.md]
check: none
updated: 2026-09-11
---

# Finish delegated editorial work in coherent batches

When the human has established examples and delegated the remaining design decisions, apply that
style to the remaining scope without requesting approval for each sentence, spacing choice, or
routine error variant. Inspect actual behavior, decide the copy, and save complete examples in
the task immediately. Distinguish prior human approvals from decisions made under delegation.

Pause only for a material product, security, destructive-action, compatibility, or scope choice
that cannot be resolved within the existing contract. Delegated design does not authorize
implementation, deployment, or executing a destructive command.

**Why:** during the CLI review the human said, "stop doing this in such small chunks" and asked
the agent to stop only on bigger decisions where their input was genuinely needed.

**How to apply:** review command families with normal, empty, failure, and prompt states together.
Give progress updates, not repeated approval gates. Save the full output and source boundaries so
another agent can implement it without reconstructing the conversation. Preserve the user's
explicitly requested review cadence until they delegate it.

## Changelog

- 2026-09-11 — swept the three source documents and this task's review workflow. No source rule
  required per-sentence approval; the review's old one-item-at-a-time procedure was the violation
  and was replaced in its task.md and cli-review.md. Spec's separate implementation approval stays
  intact. Whether a decision is material requires review, so check remains none.
