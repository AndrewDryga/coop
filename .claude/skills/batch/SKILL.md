---
name: batch
description: Drain this repo's .agent/TASKS.md queue autonomously and run-to-completion, taking EACH item to a ship-ready bar — claim `[ ]`, build it, gate it green, self-review it against the .agent/rules KB and every hat, ITERATE until clean, COMMIT it on its own, tick `[x]` — without quitting early. Arms the Stop-hook sentinel. Use to "work all the tasks" / drain a backlog / run an unattended batch.
argument-hint: "[optional note or filter]"
allowed-tools: Read, Grep, Glob, Bash, Write, Edit
---

# /batch — drain the queue to a ship-ready bar, one commit per item

Run the work loop to completion with the Stop hook armed, so it can't quit while
work remains. Scope: this repo's `.agent/TASKS.md`.

**Be agentic.** Each item is taken to a **ship-ready** bar: built, gated, then
*self-reviewed from every angle it touches and iterated until clean* — and only
**then** committed, on its own. The bar is not "it compiles"; it's "I'd defend
this in review from every hat **and it breaks no house rule.**" The house rules
are the spine: this repo's `AGENTS.md` and the worked examples in `.agent/rules/`.
Build *to* them, then *check the diff against them*.

## 1. Arm
- `touch .agent/active` — arms the Stop hook: until you remove it, trying to stop
  while any `- [ ]` remains is blocked. (It's git-ignored.)
- Read `AGENTS.md` in full (the gate **and** the contract) and every
  `.agent/rules/*.md` (the taste KB). Announce the queue and the open-`[ ]` count.
- Optionally set a `/goal` to harden "don't stop early" on top of the sentinel.

## 2. The loop — for the first `- [ ]`, repeat until none remain
1. **Claim** — flip `- [ ]` → `- [w]` *first*, so a parallel agent won't grab it.
   Skip any `- [w]` (someone's live claim).
2. **Build** — wear the hats; obey `AGENTS.md`, match `.agent/rules/` and the
   surrounding style exactly. `/plan` first if it spans more than one file.
   `/verify-api` before calling anything you're not certain exists.
3. **Gate green** — the repo's exact gate (`AGENTS.md` → "The gate"). No green, no
   review, no commit.
4. **Self-review the diff** from every angle it touches — correctness, security /
   abuse path, UX, tests (including the failure path), docs, readability — against
   the house rules. Fix what you find; iterate until you'd defend it.
5. **Commit** — one focused commit for this item. Append a one-line *what + why* to
   `.agent/LOG.md`.
6. **Tick** — flip `- [w]` → `- [x]`. Blocked instead? `- [B]` + a
   `.agent/PENDING_DECISIONS.md` entry (decision · options · recommendation), move on.
7. Spot unrelated work? Drop it in `.agent/BACKLOG.md` and return to the queue —
   don't derail the current item.

## 3. Finish
- When no `- [ ]` remains, `rm -f .agent/active` to disarm, then run a completeness
  pass: re-check every `[x]` you made against `git log` — gate green *and* a commit
  exists. Reopen (`[x]` → `[ ]`) anything that doesn't hold up, and go again.
- Report: items shipped, anything parked in BACKLOG / PENDING_DECISIONS, gate status.
