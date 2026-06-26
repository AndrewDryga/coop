# Tasks

The work queue. **A task is a folder, and its state is which directory it's in:**

- `todo/<id>/`         — not started (the loop picks the next from here)
- `in_progress/<id>/`  — claimed/active (the loop resumes one of these)
- `blocked/<id>/`      — parked on a human decision (NOT picked; has a `decision.md`)
- `done/<id>/`         — shipped/archived (never loaded by the loop)

A state change is a folder move — use the commands, not a manual `mv`:
`coop tasks claim <id>` · `block <id>` · `unblock <id>` · `done <id>` · `drop <id>`.
The directory IS the status; there is no status field to keep in sync.

## A task folder (`<id>` is `YYYY-MM-DD-<slug>`)

    in_progress/2026-06-26-egress-fail-closed/
      task.md        REQUIRED — frontmatter (id, title, labels, updated) + body + subtasks
      spec.md        optional — the design, when it's substantial
      log.md         optional — append-only working journal (what I did and why)
      state.md       optional — overwritten resume snapshot (where I am / next action)
      decision.md    optional — the pending decision (present ⇔ task is in blocked/)
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

- One task = one outcome = one commit. The move to `done/` ships in that commit.
- Claim with `coop tasks claim <id>` BEFORE you start.
- Blocked? `coop tasks block <id>`, then fill in `blocked/<id>/decision.md`.
- Finish with `coop tasks done <id>`; abandon with `coop tasks drop <id>`.
- Counts / subtask progress / the blocked list are DERIVED — `coop tasks`,
  `coop tasks decisions`. Never hand-maintain them here.
- The body is plain markdown (syncs cleanly to GitHub Issues; one conversion from Jira).

Run `coop tasks` to list the queue, `coop tasks add "<title>"` to scaffold a task.
