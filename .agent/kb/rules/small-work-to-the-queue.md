---
name: small-work-to-the-queue
description: "ready work goes to `00_todo/`; only the genuinely large or unscoped goes to the backlog"
scope: agent-workflow
sources: [AGENTS.md, .agent/skills/review-board/SKILL.md, .agent/skills/work/SKILL.md, .agent/skills/sweep/SKILL.md, internal/scaffold/templates/agent/tasks/README.md, internal/scaffold/templates/skills/review-board/SKILL.md, internal/scaffold/templates/skills/work/SKILL.md, internal/scaffold/templates/skills/sweep/SKILL.md]
check: "none"
updated: 2026-09-03
---

# Small discovered work goes to the queue, not the backlog

When you spot a SEPARATE task while working — not part of the one you're on — where it lands depends
on its size, not its topicality. A simple, ready fix goes to the QUEUE (`00_todo/`) so the loop works
it soon. Only work that's genuinely big or not-yet-ready — needs a spec, a decision, or real
scoping — goes to the BACKLOG. The backlog is NOT a dumping ground for small stuff.

**Why:** the backlog bloated to 21 items because agents parked every discovered task there, including
trivial ones ("delete this dead function", "this help line has a stray `·`"). Nothing in the loop ever
touches the backlog, so those quick fixes languished for weeks while the drawer grew too long to read —
and the human had to hand-triage it back into the queue. A task you can state an acceptance for in one
line IS ready; the queue is where ready work belongs, and the loop drains it. Reserving the backlog for
the big/unready keeps it a short, meaningful list of things that actually need a human's planning.

**How to apply:**
- Trivial + safe + on-topic → just fix it inline (boy-scout); don't create a task at all.
- A separate fix you can write a one-line acceptance for → the QUEUE. Use the proposal route supplied
  by the current run first. Otherwise use `coop tasks add` when `coop` is on `PATH`, or create the
  self-contained `00_todo/<id>/` folder when it is not.
- Only if it needs a spec, a decision, or real scoping (you CAN'T state its acceptance in a line yet)
  → the BACKLOG. Use the supplied proposal route first; otherwise use `coop backlog add` when
  `coop` is on `PATH`, or create the self-contained `xx_backlog/<id>/` folder. It waits there until
  someone fleshes it into a spec + promotes it.
- Never fold the discovered fix into the current task's commit — one task = one commit.

Related: [[fix-the-bug-not-the-feature]].

## Changelog
- 2026-09-03 — swept the three canonical workflow skills and their byte-matched scaffold copies.
  Corrected review severity and "separate idea" routing so every ready fix goes to the queue and
  only genuinely large or unscoped work goes to the backlog; made proposal routing authoritative
  before host commands or folder fallback; added all six files to `sources`.
- 2026-07-13 — created
- 2026-08-06 — card metadata added (format v1); body unchanged
- 2026-08-09 — sources: the live `.agent/tasks/README.md` is gitignored working state, so
  `make rules-check` passed on a laptop and would have failed the moment CI ran it; point at the
  committed template it's scaffolded from instead. Swept all 32 cards: it was the only untracked
  source. Body unchanged.
- 2026-08-09 — validate-on-write backfill: read both current sources. AGENTS.md's "Boy-scout rule"
  bullet and its `.agent/ working state` section both state the queue-vs-backlog split by size,
  matching the card; internal/scaffold/templates/agent/tasks/README.md carries the same principle
  in its own (independently worded, expected to drift as a generic starting point) form. 0
  violations.
