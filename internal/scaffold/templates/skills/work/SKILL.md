---
name: work
description: Execute an approved plan with focused checks while iterating and broad proof once on the completed candidate. Use when implementing a planned change, working a checklist, or the user says "go" / "implement it" / "do the plan". Investigate red checks without hiding failures.
argument-hint: "[plan, or 'continue']"
allowed-tools: Read, Grep, Glob, Bash, Write, Edit
---

# Work the plan

Implement one step at a time. The point is a green, reviewable change — not speed.
Use a short written plan for routine specified work; run `/spec` for non-obvious or cross-owner work.

## The loop (per step)
1. **State the step** in one line so progress is visible.
2. **Build it** in the surrounding style. If you're unsure a function, option, or
   flag exists — yours or a dependency's — `/verify-api` before you write it; don't
   guess. Obey `AGENTS.md` and match `.agent/kb/rules/`.
3. **Verify the step** — run the repo's actual owner checks and happy/denial-path tests.
   Rules/docs-only work needs no Docker, race or provider matrix. Unknown/shared/build/workflow
   inputs need broad proof. Run broad free checks once on the completed executable candidate;
   record deferred proof in the campaign ledger. Do not rerun unchanged broad suites after every
   edit. Honor the repo's validation rules and the user's explicit scope/batching direction.
4. **Red check → investigate.** Don't build dependent steps on a known broken contract. Fix
   the cause and rerun the affected checks. Independent implementation and source review can
   continue; neither requires waiting for slow qualification. Never edit a test to make a real
   failure pass or report a red/missing gate as green.

## Rules while working
- **No scope creep.** Build the approved slice; capture a separate idea instead of building it now.
  Use the proposal route supplied by the current run when there is one. Otherwise, ready work goes
  to `00_todo/` (`coop tasks add` when `coop` is on `PATH`, or a self-contained task folder when it
  is not); only genuinely large or unscoped work goes to `xx_backlog/` (`coop backlog add` when
  available, or a self-contained folder there). If the plan turns out wrong, explain the evidence
  and revise it. Continue design changes needed for the approved behavior within existing authority;
  ask only about new product/security decisions or changes to scope or explicit contracts that
  the user has not authorized.
- **Complexity must earn its place.** Add defensive machinery for a reachable failure with
  meaningful impact; check the real invariants before treating a case as possible or impossible.
  Keep harmless hypothetical cases and speculative extensibility out of the change. If the new
  requirement breaks the old model, make the necessary design change and update its affected
  callers and tests; a smaller patch is no excuse for an incomplete or tangled solution.
- **Readable, no bloat.** Match the surrounding style. Delete dead code you pass.
  No speculative options or abstractions. Comments say *why*, not *what*.
- **Tests are part of the step**, not a follow-up. Assert observable behavior at the appropriate
  boundary, including failure/denial paths; avoid assertions coupled only to private structure.
  For a bug fix, first use or add a meaningful regression test and see it fail for the expected
  reason. If an automated reproducer is impractical, record the manual reproduction and the
  verification limit. After the fix, rerun the same check. For refactoring, pin the existing
  behavior before changing structure and verify it still holds afterward.
- **Greenfield.** Replacing code → delete the old and update every caller in the
  same change; no shims for behavior nothing depends on yet.

When the requested change is green and reviewable, hand back for review (or, in a `/sweep`
run, self-review the diff, commit, and tick the task). If the human asked for the full outcome
— review, integration, tests, docs — a green substep is a checkpoint, not the handback: keep
going. A real blocker or a decision needing new authority still earns a pause; a finished
internal slice does not.
