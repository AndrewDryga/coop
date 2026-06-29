# AGENTS.md — the contract every agent in this repo follows.
# CLAUDE.md should symlink here:  ln -s AGENTS.md CLAUDE.md

## BOOT — on a fresh start or after compaction, read in order:
1. this file
2. .agent/tasks/      (the queue + recent 99_done/ — what's left and what shipped; start at .agent/tasks/README.md)

## How we build (the creed)
- **Boring first.** Reach for the dull, proven shape; clever earns its place only when boring can't do the job — and you can say *why* in one sentence.
- **Wear the hats** before coding: PM (the right, smallest thing?), UX (obvious path; empty/error states handled?), Security (what's the abuse case?), Maintainer (clear in six months?).
- **Done means verified, not done-once** — formatted, gated green, tested including the failure path. Never "should work": show the gate, or say what you couldn't check.
- **Readable, no bloat.** Match the surrounding style; delete more than you add; no knobs nobody asked for; comments say *why*, not *what*.
- **Boy-scout rule.** Fix small, safe messes as you pass through; backlog the big ones — never smuggle an unrelated refactor into the commit.

## Use the agent stack
- **Set the objective.** For anything longer than a quick answer, set the runtime's persistent goal/tracker if it exists (`/goal` or equivalent), and keep it current. If your agent does not have that feature, use `.agent/tasks/` as the durable goal state. A goal is the stop condition, not a substitute for a plan.
- **Batch independent reads.** Use tool batching (`/batch`, parallel tool calls, or backgrounded shell reads) for independent searches, file reads, log collection, and docs lookups. Do not batch dependent steps or mutating commands that can race.
- **Delegate thinking, keep ownership.** Use native subagents/Task workers for broad research, codebase surveys, second opinions, review, and root-cause hypotheses. Treat them as read-only advisors unless your runtime explicitly gives them an isolated workspace. The lead agent makes the decision, edits files, runs the gate, and owns the result.
- **Keep writes serialized in this checkout.** Native workers are for thinking unless the runtime proves they have separate workspaces. Never let two workers edit the same checkout at once.
- **Use real capabilities only.** If a named feature does not exist in your runtime, do the closest safe thing with the tools you actually have; do not invent slash commands, tools, or worker APIs.

## The gate (adapt to this repo)
`<format-check> && <build --warnings-as-errors> && <tests>`

## The contract
- A task is a **folder**, and its state is which directory it sits in under `.agent/tasks/`: `00_todo/` · `10_in_progress/` · `50_blocked/` · `99_done/` (the numeric prefix just sorts `ls` in lifecycle order; `coop tasks` prints the clean names). Moving the folder IS the state change — use `coop tasks`, never a manual `mv`. There is no status field and no fifth state.
- **Every folder in `00_todo/` is live.** The loop picks the next from `00_todo/` and resumes one already in `10_in_progress/`; `50_blocked/` is parked, `99_done/` is the archive. Anything not ready to work belongs in `BACKLOG.md`, never as a task folder.
- Claim a task with `coop tasks claim <id>` (moves it to `10_in_progress/`) BEFORE you start it.
- `coop tasks done <id>` (moves it to `99_done/`) only when the gate is green and the change is committed (the task's own `log.md` carries the why). The folder move is local — the queue is gitignored working state, not part of the commit.
- Blocked? `coop tasks block <id>`, then fill in its `decision.md` (the question, options, your recommendation). Never guess on a one-way door.
- One task = one commit. Spot unrelated work? Put it in .agent/BACKLOG.md and stay on task.
- **Stay on the branch you're given.** Never create, switch, or delete a git branch unless explicitly asked — commit onto the current branch. (Coop checks you out on a branch already; a new one strands your work where the human isn't looking.)
- **Tasks are self-contained.** A task's `task.md` gets read by a fresh agent after a compaction or in a new session — so it can't lean on prior chat, a past review, or memory not in the repo. Each states: the problem + context, acceptance criteria, an approach (or a `spec.md`), and its subtasks. If it can't stand on its own with just the BOOT files, it isn't ready for the queue.
- Never stop while `00_todo/` or `10_in_progress/` has a task.

## The .agent/ working state
Durable working memory the BOOT protocol reads back. Only `rules/` is committed;
the rest is local (git-ignored) so it never creates commit noise or merge churn.
- `tasks/` — the work queue: one folder per task under `00_todo/`/`10_in_progress/`/`50_blocked/`/`99_done/`.
  See `tasks/README.md` for the layout and the per-task files (`task.md`, plus optional
  `spec.md`, `log.md`, `state.md`, `decision.md`, `screenshots/`, `artifacts/`). `coop tasks`
  lists and moves them.
- `state.md` — a small, overwritten resume snapshot of the task in flight (status, what's
  done, the next action, traps), kept inside the in-progress task's own folder. The loop's
  working agent refreshes it at each checkpoint (before a commit / pause) and once more as the
  final step when the task is done, so the next iteration — a *different* agent resuming the same
  `10_in_progress/` task, or you after a review — resumes from the note instead of re-deriving it
  from the diff. Overwrite, not append (that's the task's `log.md`); never blanked by hand — it
  travels with the task to `99_done/`.
- `BACKLOG.md` — anything noted but not scheduled: discovered work, chores, and product
  ideas. One item per `##` section (the file's header carries a template), captured then left — not auto-worked, not scanned by
  the Stop hook. A human promotes an item into `tasks/00_todo/` (a real task folder) when it's
  ready. The loop reads `tasks/` only; per-task reasoning lives in each task's own `log.md`.
- `rules/` — the taste knowledge base (the one committed part).

## Skills
Use the workflow skills instead of hand-rolling: `/spec` before a multi-file
change, `/work` to execute a plan step by step against the gate, `/sweep` to
drain `.agent/tasks/` unattended, `/investigate` to root-cause a failure before
fixing, `/verify-api` before calling anything you're not sure exists, and
`/review-board` for a thorough multi-hat review before landing. They live once in
`.agent/skills/`; each agent's dir (`.claude`, `.codex`, `.gemini`) symlinks to it.

## Taste
Every correction from me becomes a rule the same day: fix it, record it in
.agent/rules/, sweep the codebase for siblings, and graduate it into a lint/hook
when it's mechanically checkable.
