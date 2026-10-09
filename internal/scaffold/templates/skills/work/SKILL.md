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
   guess. Obey `AGENTS.md` and match `.agent/kb/rules/`.
3. **Verify the step** — run focused checks that exercise its changed boundaries. The repo's
   exact gate is the final default, subject to explicit user scope/batching direction in
   `.agent/kb/rules/batch-slow-gates-when-requested.md`; do not rerun it after every small edit.
4. **Red check → investigate.** Don't build dependent steps on a known broken contract. Fix
   the cause and rerun the affected checks. Independent implementation and source review can
   continue; neither requires waiting for slow qualification. Never edit a test to make a real
   failure pass or report a red/missing gate as green.

## Rules while working
- **No scope creep.** Build the approved slice; capture a separate idea instead of building it now.
  Use the proposal route supplied by the current run when there is one. Otherwise, ready work goes
  to `00_todo/` (`coop tasks add` when `coop` is on `PATH`, or a self-contained task folder when it
  is not); only genuinely large or unscoped work goes to `xx_backlog/` (`coop backlog add` when
  available, or a self-contained folder there). If the plan turns out wrong, stop and re-plan with
  the user — don't silently redesign.
- **Readable, no bloat.** Match the surrounding style. Delete dead code you pass.
  No speculative options or abstractions. Comments say *why*, not *what*.
- **Tests are part of the step**, not a follow-up — including the failure/denial path.
- **Greenfield.** Replacing code → delete the old and update every caller in the
  same change; no shims for behavior nothing depends on yet.

When the requested change is green and reviewable, hand back for review (or, in a `/sweep`
run, self-review the diff, commit, and tick the task). If the human asked for the full outcome
— review, integration, tests, docs — a green substep is a checkpoint, not the handback: keep
going. A real blocker or a decision needing new authority still earns a pause; a finished
internal slice does not.
