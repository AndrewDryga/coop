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
- Set the runtime's persistent goal/tracker when it exists: do not stop until every task that was
  in scope is committed and archived, and both `00_todo/` and `10_in_progress/` are empty.
- Read `AGENTS.md` in full (the gate **and** the contract), `.agent/tasks/README.md`,
  and every `.agent/rules/*.md` (the taste KB). Run `coop tasks` to announce the
  queue and the open `00_todo/` count.

## 2. The loop — claim the next task, repeat until 00_todo/ and 10_in_progress/ are empty
1. **Claim** — `coop tasks claim <id>` (moves `00_todo/` → `10_in_progress/`) *first*, so a
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
5. **Commit** — one focused commit for this task; keep the task's `state.md` (resume
   snapshot) and `log.md` (journal — the *what + why*) current.
6. **Done** — `coop tasks done <id>` (moves it to `99_done/`; the move ships in the
   commit). A finished task is **moved**, never deleted — leave it in `99_done/`; never run
   `coop tasks rm` (pruning the archive is the human's call). Blocked instead? `coop tasks
   block <id>` and fill in its `decision.md` (the question · options · recommendation), then move on.
7. Spot unrelated work? Park it with `coop backlog add` and return to the queue —
   don't derail the current task.

## 3. Finish
- When `00_todo/` and `10_in_progress/` are empty, run a completeness pass: re-check every task
  you moved to `99_done/` against `git log` —
  gate green *and* a commit exists. Reopen anything that doesn't hold up with
  `coop tasks claim <id>` (back to `10_in_progress/`), and go again.
- Report: tasks shipped, anything parked in the backlog or `50_blocked/`, gate status.
