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

## Orchestration — spend the big model where it matters
When you lead a session here, you orchestrate: plan, decompose, synthesize, make the final
calls, and keep your own context lean by delegating. This repo's roles live in the
**frontier preset** (`.agent/presets/frontier/preset.yaml`) — under it, route by the nature
of the work:
- **thinker** (codex/gpt-5.6-terra at xhigh, read-only) — reasoning-heavy phases:
  architecture calls, complex or intermittent bugs, security, code review, a pre-commit
  check. Ask with a self-contained prompt: `coop-consult thinker --fresh "…"`; it returns
  a conclusion, you act on it.
- **critic** (grok/grok-4.5, read-only) — the second critical opinion from OUTSIDE the
  claude+codex pair doing the work: plan review, tradeoffs, one-way doors. Same shape:
  `coop-consult critic --fresh "…"`.
- **fast** (codex/gpt-5.6-luna at xhigh, write-capable) — mechanical, fully-specified work:
  boilerplate, bulk edits, test scaffolding, repo surveys. Run it via `coop-delegate fast`;
  it never commits — you review its diff, gate, and commit.
- **High-stakes decisions:** task the thinker AND the critic on the same problem in
  parallel, without showing either the other's answer, then synthesize the best of both.
Outside the preset (no `coop-consult`/`coop-delegate` on PATH), don't improvise substitutes:
use whatever subagent types your runtime actually offers for the same split — reasoning vs
mechanical — and skip peers. The single-writer rule above still holds: advisors and peers
think; you edit, gate, and commit.

## The gate (adapt to this repo)
`<format-check> && <build --warnings-as-errors> && <tests>`

## The contract
- A task is a **folder**, and its state is which directory it sits in under `.agent/tasks/`: `00_todo/` · `10_in_progress/` · `50_blocked/` · `99_done/` (the numeric prefix just sorts `ls` in lifecycle order; `coop tasks` prints the clean names). Moving the folder IS the state change: on the host use `coop tasks` (never a manual `mv`); inside the box — where `coop` isn't installed — move the folder yourself. There is no status field and no fifth state.
- **Every folder in `00_todo/` is live.** The loop picks the next from `00_todo/` and resumes one already in `10_in_progress/`; `50_blocked/` is parked, `99_done/` is the archive. Anything not ready to work goes in the backlog (`coop backlog add`), never in `00_todo/`.
- Claim a task with `coop tasks claim <id>` (moves it to `10_in_progress/`) BEFORE you start it.
- `coop tasks done <id>` (moves it to `99_done/`) only when the gate is green and the change is committed (the task's own `log.md` carries the why). The folder move is local — the queue is gitignored working state, not part of the commit.
- Blocked? `coop tasks block <id>`, then fill in its `decision.md` (the question, options, your recommendation). Never guess on a one-way door.
- One task = one commit. Spot unrelated work? Park it with `coop backlog add` and stay on task.
- **Stay on the branch you're given.** Never create, switch, or delete a git branch unless explicitly asked — commit onto the current branch. (Coop checks you out on a branch already; a new one strands your work where the human isn't looking.)
- **Hands-off destroyers — human-only, never run unattended.** `coop update` (replaces the running binary and reaps supervised boxes), `coop fleet down`, `coop fork rm`/`coop fork merge --force`, and `coop tasks rm` destroy state or processes with no undo. A loop or sweep must never invoke them — they're the human's call.
- **Tasks are self-contained.** A task's `task.md` gets read by a fresh agent after a compaction or in a new session — so it can't lean on prior chat, a past review, or memory not in the repo. Each states: the problem + context, acceptance criteria (including what does NOT count as done — the near-miss to reject, not just the target), an approach (or a `spec.md`), and its subtasks. If it can't stand on its own with just the BOOT files, it isn't ready for the queue.
- **In `coop loop`, work one task per iteration, then stop.** The loop re-invokes a fresh agent until `00_todo/` and `10_in_progress/` are empty — finishing the queue is the *loop's* job across iterations, not one agent draining it in a single run. (Interactive `/sweep` is the exception: it drains the queue in one session. Never stop while `00_todo/` or `10_in_progress/` has a task during a /sweep)

## The .agent/ working state
Durable working memory the BOOT protocol reads back. Knowledge (`rules/`, `skills/`,
`presets/`) and `project.yaml` are committed; the working state (tasks, backlog) is
local (git-ignored) so it never creates commit noise or merge churn.
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
- backlog — anything noted but not scheduled (discovered work, chores, product ideas), kept as
  task folders in the `tasks/xx_backlog/` drawer via `coop backlog add`. It lives OUTSIDE the
  lifecycle, so it's never auto-worked nor scanned by the Stop hook; `coop backlog promote <id>`
  moves an item into `tasks/00_todo/` (a folder move, not a rewrite) once it's ready. Shipped or
  cancelled? `coop backlog rm <id>` — a shipped idea's record is its commits. The loop reads the
  lifecycle states only; per-task reasoning lives in each task's own `log.md`.
- `rules/` — the taste knowledge base (committed).
- `project.yaml` — the committed per-project config: a monorepo's `subprojects:` (each
  member keeps its own `tasks/`, backlog drawer included; every coop task command aggregates them,
  and the root queue holds work that spans members) and the `serve:` ports coop
  publishes so a dev server in the box is reachable from the host browser. When working
  inside a subproject, also honor its own `.agent/rules/` and `AGENTS.md` if present.

## Skills
Use the workflow skills instead of hand-rolling: `/spec` before a multi-file
change, `/work` to execute a plan step by step against the gate, `/sweep` to
drain `.agent/tasks/` unattended, `/investigate` to root-cause a failure before
fixing, `/verify-api` before calling anything you're not sure exists,
`/review-board` for a thorough multi-hat review before landing, and `/release` to
cut a versioned, tagged release. They live once in
`.agent/skills/`; each agent's dir (`.claude`, `.codex`, `.gemini`) symlinks to it.

## Taste
Every correction from me becomes a rule the same day: fix it, record it in
.agent/rules/, sweep the codebase for siblings, and graduate it into a lint/hook
when it's mechanically checkable.
