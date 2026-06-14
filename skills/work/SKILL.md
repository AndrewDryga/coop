---
name: work
description: Execute an approved plan step-by-step, with the repo's gate between steps and no scope creep. Use when implementing a planned change, working a checklist, or the user says "go" / "implement it" / "do the plan". Stops and reports on the first red gate.
argument-hint: "[plan, or 'continue']"
allowed-tools: Read, Grep, Glob, Bash, Write, Edit
---

# Work the plan

Implement one step at a time. The point is a green, reviewable change — not speed.
If there's no plan yet and the change is non-trivial, run `/spec` first.

## The loop (per step)
1. **State the step** in one line so progress is visible.
2. **Build it** in the surrounding style. If you're unsure a function, option, or
   flag exists — yours or a dependency's — `/verify-api` before you write it; don't
   guess. Obey `AGENTS.md` and match `.agent/rules/`.
3. **Gate before moving on** — run the repo's exact gate (`AGENTS.md` → "The gate").
4. **Red gate → stop.** Don't pile the next step on a broken one. Fix it, or report
   the blocker with the error and your read on it. Never edit a test to make a real
   failure pass.

## Rules while working
- **No scope creep.** Build the approved slice. A good idea that wasn't in the plan
  → `.agent/BACKLOG.md` as "later", don't build it now. If the plan turns out wrong,
  stop and re-plan with the user — don't silently redesign.
- **Readable, no bloat.** Match the surrounding style. Delete dead code you pass.
  No speculative options or abstractions. Comments say *why*, not *what*.
- **Tests are part of the step**, not a follow-up — including the failure/denial path.
- **Greenfield.** Replacing code → delete the old and update every caller in the
  same change; no shims for behavior nothing depends on yet.

When the change is green and reviewable, hand back for review (or, in a `/sweep`
run, self-review the diff, commit, and tick the task).
