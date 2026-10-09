# The loop

`coop loop` works through your task queue on its own, with a fresh agent for every task. When the queue is empty, a final review reopens unfinished work, and a task that needs your decision waits for you. The [loop guide](https://coop.dryga.com/docs.html#loop) shows the flow; this page has the details.

```bash
coop init                 # scaffold AGENTS.md, the .agent/ working folder, and the hooks
coop tasks add "..."      # add a task (a folder under .agent/tasks/00_todo/)
coop loop claude          # disposable agents work the queue until it's done, then sign off
coop loop codex           # …or name claude, codex, gemini, grok, or a preset
```

A task is a folder under `.agent/tasks/`. Its state is the directory it sits in: `00_todo/`, `10_in_progress/`, `50_blocked/` or `99_done/`. The numeric prefix sorts `ls` in lifecycle order, and `coop tasks` shows the clean names.

Before each iteration, the loop selects and claims the next task on the host, or resumes one left in `10_in_progress/`. Then it starts a fresh agent assigned to that exact task, so old context never piles up. The loop won't quit while `00_todo/` or `10_in_progress/` has work it can acquire.

Name the agent (`claude`, `codex`, `gemini` or `grok`), or a preset whose lead supplies it. For something custom, `work.command` in `.agent/loop.yaml` still overrides the whole iteration command.

If the model hits a rate or usage limit mid-run, the loop doesn't count it as a failure. It reads the reset time from the agent's own output, waits it out with a countdown, and resumes the same item once the limit clears. An overnight run rides through the daily cap and doesn't burn retries against it.

Add `--preflight` to run one cleanup pass before the loop starts working. It unblocks any `50_blocked/` task whose `decision.md` now has an answer, so a fresh run starts from a tidy queue. A task you parked with `coop tasks park` waits in the backlog, so preflight leaves it alone. It works no task and makes no commits. It mirrors the final review at the other end of the run. Preflight is off by default.

## Exit codes

A cron job or CI can branch on the loop's exit code without parsing its output:

| Exit code | Meaning |
|---|---|
| `0` | Final review and enabled final checks passed, or a successful `--max-tasks` pause before final review. |
| `1` | A failure, an unverified final pass, or actionable work. |
| `2` | A usage error. |
| `3` | The loop stopped with a task blocked on a human decision, including one the review kept reopening past the round cap. Resolve it with `coop tasks decisions`, then run the loop again. |
| `130` | The run was interrupted before the final verdict. |

`--max-tasks` pauses the loop; it does not certify the queue as done. A successful pause exits `0`
and leaves final review pending. Run the same command without the limit to resume that review
before treating the work as verified complete.

Each controller leases its exact task with a host-only lock under `~/.local/state/coop/task-leases/` while the agent runs. A second loop skips a held task and can take independent todo work. `coop tasks watch` shows a short `busy`, `stalled` or `unleased` state from metadata beside that lock, without exposing run IDs or PIDs. A stale heartbeat is only a diagnostic. The loop adopts a task only once its kernel lock is available, and then right away. It never adopts one by timeout. An unleased in-progress folder is adopted through that same lock.

A human's `coop tasks claim <id>` works differently. The command exits at once and holds no lock, so it records durable ownership instead. The loop refuses that task, naming the owner and `coop tasks release <id>`, until an explicit `release`, `block`, `unblock` or `done` clears it. A claim never expires on its own.

Stop running co:op controllers before a major-version upgrade. Controllers from different major versions don't share a supported authority contract.

A completed task leaves a small host-only receipt on the persistent authority lock inode. Concurrent loops use it to recognize a released owner's finalized folder, without trusting task metadata the provider can write. Before every writable agent or review box starts, the controller also journals the current done-folder fingerprints in that host-only registry. It removes the journal only after it validates the box's queue changes. After a controller crash, the next loop replays the journal and restores any unowned completion instead of silently grandfathering it. Completions made outside a supervised box stay ordinary task history.

The `/sweep` skill carries a scoped `Stop` hook, so only an active sweep is held while actionable tasks remain. Fresh scaffolds already have this layout. `coop init` is non-destructive, so an existing repo has to migrate by hand:

1. Remove only the stock `Stop` group from `.claude/settings.json`. Keep `PreToolUse` and custom hooks.
2. Delete `.claude/hooks/stop-guard.sh`, but only if it's the old stock guard.
3. Remove a stale `.agent/active`.
4. Merge the current `.agent/skills/sweep/` files, keeping your local customizations.

`init` also installs a fast Claude commit-gate hook. That hook is Claude-only, so `init` also installs a tracked git pre-commit gate (`.githooks/pre-commit`) and points `core.hooksPath` at it. The format check then runs for every committer, including Codex, Gemini and a plain `git commit`. The hook files are tracked, so a fresh clone gets them, but `core.hooksPath` is local Git config: run `coop init` in a new clone to point it at them again. The tracked `.githooks/prepare-commit-msg` shim also chains co:op's mounted box-attribution hook when it's present, and does nothing on a normal host.

A custom `core.hooksPath` is left untouched. Copy or chain both tracked hooks from the active hook directory. If that directory already has a `prepare-commit-msg`, add the shim's co:op hook call to that file instead of replacing it. Skip the format gate once with `git commit --no-verify`.

## Final review

When the queue empties, a fresh, demanding final review (the signoff) re-checks each task the run shipped, held to a senior reviewer's bar:

- every acceptance criterion and subtask is met
- the change follows `AGENTS.md` and `.agent/kb/rules`, with no scope creep
- the failure path is tested
- the change is polished, with docs and the CHANGELOG updated
- the bookkeeping is right

co:op atomically finalizes each completed `state.md` before the review. The worker records its final checks in that state as a compact handoff. Reviewers inspect the implementation and the test coverage and trust the reported execution. They never repeat tests themselves. A missing, failed or stale check reopens the task with an exact request for the worker. Reviewers never change an archived task in place. An unexpected lifecycle defect is reopened and reported like any other completion-integrity failure.

If the review reopened work, the loop drains the queue and reviews again. It repeats until a review reopens nothing (verified done) or the loop hits the round cap. At the cap, the task the review keeps reopening is blocked for a human, and the loop doesn't report it as done. The cap scales with the batch: half the tasks worked this run, clamped to `[3, signoff.rounds]` (default `5`). A small batch still gets a few tries, and a big overnight batch can't ping-pong one stuck task forever.

Each completed task must have exactly one `Coop-Task: <id>` binding in the current iteration's commit range, and exactly one reachable from `HEAD`. So rework after a review amends or rewrites the existing task commit. A second bound commit is rejected, and the task goes back to in-progress. A worker can't bind another task in its iteration either: verify subjects come only from tasks the host accepted as completed during this run.

Every review closes with one structured evidence line per subject, and a PASS/FAIL receipt naming the exact sorted task IDs it proposes to reopen. In the default `writes: tasks` mode, the review only reports: the whole repository, task queues included, is read-only. co:op validates the complete proposal, acquires every subject's host-side task authority, and applies all the exact-subject reopens as one transaction.

If a review process succeeds but returns malformed structured output, co:op re-runs the complete review once, right away. The re-run covers the same subjects under the same configured writes policy, with a fixed receipt-format correction. Each attempt gets its own telemetry record. A malformed second verdict, lifecycle churn, an interruption, a process failure or an out-of-scope proposal changes no task.

Reviewer findings are stored in a delimited, untrusted log block. The next worker gets a fixed, reproduction-first action instead of instructions the reviewer wrote. `writes: repo` remains the deliberate escape hatch for fixing source. Even then, every task queue is remounted read-only, and the host still applies lifecycle changes.

Tune the loop in one committed `.agent/loop.yaml`, with a section per step:

| Section | Step |
|---|---|
| `preflight` | the cleanup pass before the loop starts working |
| `work` | the iterations that work the tasks |
| `between` | the per-task reviewer |
| `signoff` | the final review |
| `verify` | an optional affected-feature pass after the final review |

`work`, `between`, `signoff` and `verify` each take an `agent:` model ladder, and every step except `work` takes a `prompt:`. A ladder lists targets (`provider[:model][/effort][@account]`) or preset names, so `signoff.agent` can review on a stronger model than the cheaper `work.agent` loop.

Prompts never replace a co:op built-in. `signoff.prompt` appends extra checks to the built-in final review. `preflight.prompt` adds a separate agent cleanup pass on top of the unblock step co:op runs on the host. The final review still reopens a shipped task that fails one of your checks, for example that the CHANGELOG gained an entry or that the docs were regenerated. `between.prompt` sets an opt-in per-task audit that runs after each completed task and may reopen it. `verify.prompt` sets its opt-in final pass the same way.

Ordinary between review is off unless you switch it on and set its prompt. A completed task that changed a gate-defining file always gets an immediate protected audit before the loop moves on. That audit uses the configured between target and prompt, or falls back to the signoff target and a focused built-in prompt. co:op names the just-finished task in either prompt.

co:op protects Makefiles, CI, hooks and its own config automatically. If the real checker is a wrapper script or an ordinary source file, list each exact repo-relative path under `gate_sources:` in `.agent/project.yaml`. co:op freezes that list when the loop starts, so a task can't remove its own protection. A deliberate change to the list applies after a restart. Existing projects keep the built-in behavior. The list is explicit on purpose, because co:op doesn't claim it can infer the checker's complete source graph.

Settings live in the same file: `signoff.rounds`, `preflight.enabled`, `verify.enabled` and `work.command`. Every field is optional, and a missing file means the built-in defaults. `coop init` scaffolds a fully commented starter.

## The .agent/ folder

`init` creates a tool-neutral working folder that the agent reads back on every boot and after each compaction. Everything in it is local working state that git ignores, except the parts that are committed: the knowledge tree (`kb/`, including `kb/rules/`), the workflow assets (`skills/`, `presets/`), the Claude fallback adapter (`claude/`), `project.yaml`, `loop.yaml`, `compose.yml`, `Dockerfile` and `tasks/README.md`.

| Path | What it's for |
|---|---|
| `tasks/` | The work queue: one folder per task under `00_todo/`, `10_in_progress/`, `50_blocked/` and `99_done/`. A task's state is its directory, and `coop tasks` moves it. Each folder carries its own `spec.md`, `log.md`, `state.md` and `decision.md` as needed. The loop reads `00_todo/` and `10_in_progress/`. |
| the backlog | Unscheduled ideas, as task folders in the `tasks/xx_backlog/` drawer (`coop backlog`). It sits outside the lifecycle, so it's never auto-worked and never nagged by the Stop hook. `coop backlog promote <id>` moves an item into `tasks/00_todo/` when it's ready. |
| `kb/` | The committed descriptive knowledge base: subsystem maps, cross-cutting traps, and gotchas the code doesn't carry. |
| `kb/rules/` | The normative part of the knowledge tree. Corrections graduate into "do X, not Y" rules here. |
| `claude/` | Fallback user-level Claude settings and hooks for repos without matching project `.claude/` artifacts. Committed. |
| `project.yaml` | The committed per-project config: a monorepo's [`subprojects:`](#monorepos), the [`serve:` ports](box.md#dev-servers-in-your-browser), the box policy (`box:`), the merge `gate:`, and the exact project-specific `gate_sources:` that get protected loop review. The `box:` policy covers egress and [`egress_rules:`](networking.md), resource caps, and `auto_up`/`network`. `box:` and `gate:` rank below an explicit `COOP_*` env or conf setting. Because the file is committed and read on the host, it can only tighten your posture: egress pins `filtered` or `offline`, `open` is a request a human approves with `coop approve`, and `no_new_privileges` can't be set here. |

On a fresh scaffold, `init` also installs generic workflow skills into `.agent/skills/` and links the selected agents to it:

| Skill | What it's for |
|---|---|
| `/spec` | writing a spec for a multi-file change |
| `/work` | carrying out that change step by step against the gate |
| `/sweep` | draining `.agent/tasks/` |
| `/investigate` | finding the root cause of a failure |
| `/verify-api` | checking anything you're unsure of before you call it |
| `/review-board` | a thorough multi-hat review before landing |

Edit them freely; `init` won't overwrite a skill you've changed. When `init` adopts an existing real `.claude/skills/` instead, it leaves that project-owned set unchanged and links the other selected agents to it.

A repo that still has a single `.agent/TASKS.md` needs converting to the folder format. Paste the prompt in [MIGRATING.md](../MIGRATING.md) into any coding agent in the repo.

## Monorepos

When one repo holds several components, each with its own work, list them once in the top-level `.agent/project.yaml`:

```yaml
# .agent/project.yaml — committed with the repo
subprojects: [runner, packs, portal, mcp]
```

co:op then aggregates every member's `.agent/tasks` automatically:

- `coop tasks` rolls them up under per-queue headers
- one `coop loop` drains them all
- `coop prompt` counts across them
- the id commands (`claim`, `done` and the rest) find a task in whichever queue holds it

You no longer hand-maintain `COOP_TASKS`, though an explicit `COOP_TASKS` or `--tasks` still overrides. Members keep their own queues for their own work. The root keeps one too, for changes that span members.

`coop init` at the root detects the members (folders at any depth that have a `.agent/`, skipping hidden and build folders) and writes the `project.yaml`. It scaffolds each member with just its own task queue. Members share the root's AGENTS.md, `.agent/skills/`, `.agent/kb/` and box, though a large member may commit its own `.agent/kb/rules/` if it wants them. When a script (such as the sweep queue guard) needs the resolved queue paths, `coop tasks queues` prints them.

## Parallel forks

Run several models at once, each looping unattended in its own [fork](forks.md). Point them at the same canonical project queue. The host assigns the tasks, so every claim happens exactly once:

```bash
coop fork perf codex  --loop -d # each worker claims a different canonical task
coop fork deps gemini --loop -d
coop fork docs claude --loop -d

coop tasks watch                # canonical queue + assignments + every active sandbox
coop fork ls                    # worker state, sandbox activity, candidate/task progress, diff, cost
coop fork logs -f               # tail every fork at once (compose-style, prefixed)
coop fork stop perf             # halt one; coop fork logs perf -f to watch just it
```

With no `--tasks`, a fork schedules from every queue co:op knows about: the repo's own `.agent/tasks` plus each [monorepo](#monorepos) member's. `--tasks <path>` narrows the scheduler to one canonical queue, including an explicit absolute queue outside the project. It never copies or remaps that queue. At any moment, the fork receives only its assigned task, under `.coop/task-executions/`. A restart in the same generation resumes it, and another fork can't claim it.

`-d` (`--detach`) runs the worker in the background and captures its output in `../<repo>-forks/.coop/<name>.log`. When a fork drains its available work, review and [land it](forks.md#land-it) like a pull request, then `git push`. Add agents until review is your bottleneck.

`coop fork merge --all` lands all forks at once. It's a revalidating rebase queue: it rebases each fork onto the result of the last one and re-runs the configured gate (`gate:` in `.agent/project.yaml`, or `COOP_GATE`). A fork that was green can't ride in against a base an earlier landing already changed. The queue stops at the first conflict or red gate and leaves the rest untouched.

`coop tasks split` and whole-queue fork seeding are retired. Those copies could each move the same task on their own, and no merged dashboard could make them one authority. Keep semantic grouping as separate canonical queues only when the project truly owns separate queues. `--tasks` is a filter. It doesn't slice a queue.

Give direct targets that run at the same time different accounts if you want them to avoid subscription contention. A preset can carry a full provider and account rotation.

A stale or crashed worker shows as `cleanup` in `coop fork ls` until `coop fork stop <name>` reaps it. `stop` reaps only that fork's containers, even if another repository uses the same fork name, and stopping twice is safe. Remove a fork you no longer need with `coop fork rm <name>`. `--yes` confirms without a prompt. `--force` may stop its detached worker, discard dirty or unmerged Git work along with the reviewed candidate and pending proposals, and return its canonical assignments to the queue.
