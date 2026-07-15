---
name: sweep
description: Drain this repo's .agent/tasks/ queue autonomously and run-to-completion, taking EACH task to a ship-ready bar — claim a 00_todo/ task (`coop tasks claim`), build it, gate it green, self-review it against the .agent/rules KB and every hat, ITERATE until clean, COMMIT it on its own, then `coop tasks done` — without quitting early. Use to "work all the tasks" / drain a backlog / run an unattended sweep.
argument-hint: "[optional note or filter]"
allowed-tools: Read, Grep, Glob, Bash, Write, Edit
hooks:
  Stop:
    - hooks:
        - type: command
          command: 'bash "$CLAUDE_PROJECT_DIR/.agent/skills/sweep/queue-guard.sh"'
---

# /sweep — drain the queue to a ship-ready bar, one commit per task

Run the work loop to completion. Claude scopes the queue Stop hook above to this skill's
lifetime; runtimes with a persistent goal/tracker use that too. Scope: this repo's
`.agent/tasks/` (a task is a folder; its state is its directory — `coop tasks` lists
and moves them).

**Be agentic.** Each task is taken to a **ship-ready** bar: built, gated, then
*self-reviewed from every angle it touches and iterated until clean* — and only
**then** committed, on its own. The bar is not "it compiles"; it's "I'd defend
this in review from every hat **and it breaks no house rule.**" The house rules
are the spine: this repo's `AGENTS.md` and the worked examples in `.agent/rules/`.
Build *to* them, then *check the diff against them*.

## 1. Prepare
- Set the runtime's persistent goal/tracker when it exists: do not stop until every scoped task is
  committed and archived or properly blocked, and both `00_todo/` and `10_in_progress/` are empty.
- Read `AGENTS.md` in full (the gate **and** the contract), `.agent/tasks/README.md`,
  and every `.agent/rules/*.md` (the taste KB). If `coop` is on PATH, run `coop tasks`; otherwise
  inspect the queue state directories directly to announce the open `00_todo/` count.

## 2. The loop — claim the next task, repeat until 00_todo/ and 10_in_progress/ are empty
1. **Claim** — if `coop` is on PATH, run `coop tasks claim <id>`; otherwise move the task folder
   from `00_todo/` to the existing `10_in_progress/` directory yourself. Do this *first*, so a
   parallel agent won't grab it. A task already in `10_in_progress/` is a prior attempt —
   resume it: read its `state.md`, then `git status`/`git diff`.
2. **Build** — wear the hats; obey `AGENTS.md`, match `.agent/rules/` and the
   surrounding style exactly. `/spec` first if it spans more than one file (writes the
   task's `spec.md`). `/verify-api` before calling anything you're not certain exists.
3. **Gate green** — the repo's exact gate (`AGENTS.md` → "The gate"). No green, no
   review, no commit.
4. **Self-review the diff** from every angle it touches — correctness, security /
   abuse path, UX, tests (including the failure path), docs, readability — against
   the house rules. Fix what you find; iterate until you'd defend it.
5. **Commit** — one focused commit for this task; keep `log.md` (the *what + why*) current.
6. **Final snapshot** — after the commit, refresh `state.md` in `10_in_progress/`: preserve the
   useful summary and traps, set `Status` to `complete`, and set `Next action` to `none`.
7. **Done** — as the final filesystem action, if `coop` is on PATH run `coop tasks done <id>`;
   otherwise move the task folder to the existing `99_done/` directory yourself. A finished task
   is **moved**, never deleted — leave it in `99_done/`; never run
   `coop tasks rm` (pruning the archive is the human's call). Blocked instead? Fill in
   `decision.md` (question · options · recommendation), then use `coop tasks block <id>` when
   available or move the folder to the existing `50_blocked/` directory yourself.
8. Spot unrelated work? Use `coop backlog add` when available, or create a self-contained folder
   under `.agent/tasks/xx_backlog/`, then return to the queue —
   don't derail the current task.

## 3. Finish
- When `00_todo/` and `10_in_progress/` are empty, run a completeness pass: re-check every task
  you moved to `99_done/` against `git log` —
  gate green *and* a commit exists. Reopen anything that doesn't hold up with
  `coop tasks claim <id>` when available, or move it back to `10_in_progress/`, and go again.
- Report: tasks shipped, anything parked in the backlog or `50_blocked/`, gate status.
