# Tasks

The work queue. **A task is a folder, and its state is which directory it's in:**

- `00_todo/<id>/`         — not started (the loop picks the next from here)
- `10_in_progress/<id>/`  — claimed/active (the loop resumes one of these)
- `50_blocked/<id>/`      — parked on a human decision (NOT picked; has a `decision.md`)
- `xx_done/<id>/`         — shipped/archived (never loaded by the loop)

The numeric prefix is just a sort key: it makes a plain `ls .agent/tasks` list the states in
lifecycle order instead of alphabetically (`xx_` keeps done last). `coop tasks` prints the
clean names — todo · in_progress · blocked · done.

A state change is a folder move — use the commands, not a manual `mv`:
`coop tasks claim <id>` · `block <id>` · `unblock <id>` · `done <id>`.
The directory IS the status; there is no status field to keep in sync.

## A task folder (`<id>` is `YYYY-MM-DD-<slug>`)

    10_in_progress/2026-06-26-egress-fail-closed/
      task.md        REQUIRED — frontmatter (id, title, labels, updated) + body + subtasks
      spec.md        optional — the design, when it's substantial
      log.md         optional — append-only working journal (what I did and why)
      state.md       optional — overwritten resume snapshot (where I am / next action)
      decision.md    optional — the pending decision (present ⇔ task is in 50_blocked/)
      screenshots/   optional — screenshots, the common visual deliverable (git-ignored)
      artifacts/     optional — other outputs: repros, data, generated files (git-ignored)

## task.md (self-contained — a fresh agent works it from this file alone)

    ---
    id: 2026-06-26-egress-fail-closed
    title: <one-line outcome>
    labels: []
    updated: <timestamp>
    ---

    # <one-line outcome>

    **Context:** the problem, why it matters, where it lives in the code.
    **Acceptance criteria:** the gate green + the behavior/test that proves it.
    **Approach:** the boring plan (move to spec.md when it grows past ~a screen).

    ## Subtasks
    - [ ] an end-to-end, testable step (work one at a time; check off after the gate)

## Rules

- One task = one outcome = one commit. The move to `xx_done/` ships in that commit.
- Claim with `coop tasks claim <id>` BEFORE you start.
- Blocked? `coop tasks block <id>`, then fill in `50_blocked/<id>/decision.md`.
- Finish with `coop tasks done <id>` — a finished task is **moved** to `xx_done/`, never
  deleted. Agents only move tasks between states; they don't remove them.
- Pruning the archive is a manual, human step: `coop tasks remove --all-done` (or
  `coop tasks remove <id>` for one). The loop and `/sweep` never do this.
- Counts / subtask progress / the blocked list are DERIVED — `coop tasks`,
  `coop tasks decisions`. Never hand-maintain them here.
- The body is plain markdown (syncs cleanly to GitHub Issues; one conversion from Jira).

Run `coop tasks` to list the queue, `coop tasks add "<title>"` to scaffold a task.
