---
name: blocked-decisions-are-plain-and-long-term
description: a decision.md says in plain English why the task is blocked and what to decide, and recommends the option best for the product long term
scope: agent-workflow
sources: [.agent/tasks/README.md, internal/tasks/channel.go, internal/tasks/cmd.go, AGENTS.md]
check: none
updated: 2026-10-17
---

# Write each blocked decision in plain English and recommend the long-term pick

A `decision.md` is read by the human, often several in a row and without the code open. Write it
so they can answer it in one pass:

- `# Decision:` is a real question in plain words, not the task title with a question mark.
- **Why it is blocked:** what happened and why an agent cannot go on, in two or three sentences.
- **The decision:** the one thing the human must choose or do.
- **Options:** what each one means for the user or the product, cost included. Drop an option the
  evidence has already ruled out instead of offering it.
- **Recommendation:** one line naming the option that is best for the product in the long term,
  and why. When that option is large, say so and name the small step to take now. Never recommend
  an option only because it is quick.

Leave out internal names the human would not know: function names, line numbers, commit ids,
fixture numbers, option letters with no meaning. Keep a command, path or quoted text when the human
has to run, find or approve it. Check the facts are still current before writing (a push, a count,
a CI run): a question the repo has already answered is closed with that evidence, not asked.

**Why:** the owner asked for simple language in decisions twice on 2026-09-19, and on 2026-10-17
asked that every blocked task say why it is blocked, what decision is needed from them, and which
option is best for the product long term. A sweep that day found decisions dense with internal
names, two asking for a push that had already happened, and one recommending against the owner's
recorded wish for short release notes.

**How to apply:** keep the `**Blocks:**`, single-line `**Recommendation:**` and `**Resolution:**`
lines that `coop tasks decisions` reads. Details an agent needs to resume go in log.md and
state.md, not decision.md. Related: [[cli-text-is-plain-english]].

## Changelog
- 2026-10-17 — created. Swept all 11 blocked decision.md files: 2 were already answered by the
  repo (main pushed, CI green) and closed with that evidence; 9 rewritten to this shape, 3 of them
  with changed options or recommendation (fork-record fence, release summary, case-insensitive
  secrets). The writers in `coop tasks block` do not ask why a task is blocked; queued as
  2026-10-17-ask-why-a-task-is-blocked-in-coop-tasks-block-an.
