# The loop

`coop loop` works through your task queue with a fresh agent for each task. When the queue is
empty, a reviewer checks the completed work and reopens anything unfinished. Tasks that need
your decision wait for you. Start with the [loop guide](https://coop.dryga.com/docs.html#loop),
or use this page for the details.

```bash
coop init                 # scaffold AGENTS.md, the .agent/ working folder, and the hooks
coop tasks add "..."      # add a task (a folder under .agent/tasks/00_todo/)
coop loop claude          # disposable agents work the queue until it's done, then sign off
coop loop codex           # …or name claude, codex, gemini, grok, or a preset
```

A task is a folder under `.agent/tasks/`. Its state is the directory it sits in: `00_todo/`, `10_in_progress/`, `50_blocked/` or `99_done/`. The numeric prefix sorts `ls` in lifecycle order, and `coop tasks` shows the clean names.

Before starting an agent, the loop claims the next task on the host or resumes one left in
`10_in_progress/`. The new agent works only on that task, without carrying over the previous
agent's conversation. The loop keeps going while it can claim work from `00_todo/` or
`10_in_progress/`.

Choose an agent (`claude`, `codex`, `gemini` or `grok`) or a preset that defines the lead agent.
To run a custom command for each task, set `work.command` in `.agent/loop.yaml`. It replaces the
whole iteration command.

When the model reaches a rate or usage limit, the loop tries the next configured target. If
every target is limited, it waits with a countdown until one can run, then retries the same
task. A rate limit does not count as a failed task attempt.

Add `--preflight` to check the queue before work starts. This pass unblocks tasks in
`50_blocked/` when their `decision.md` has an answer. It leaves backlog tasks parked with
`coop tasks park` alone, does no task work and makes no commits. Preflight is off by default.

## Exit codes

A cron job or CI can branch on the loop's exit code without parsing its output:

| Exit code | Meaning |
|---|---|
| `0` | Final review and enabled final checks passed, or a successful `--max-tasks` pause before final review. |
| `1` | A failure, an unverified final pass, or actionable work. |
| `2` | A usage error. |
| `3` | The loop stopped with a task blocked on a human decision, including one the review kept reopening past the round cap. Resolve it with `coop tasks decisions`, then run the loop again. |
| `130` | The run was interrupted before the final verdict. |

`--max-tasks` pauses the loop before final review. A successful pause exits `0`, but the work
still needs review. Run the same command without the limit to finish that review.

The resumed review uses the `Coop-Task: <id>` trailer to find each task's commit among the
commits made since the run started. You can sign those commits between runs. If any commit seen
by a previous review has changed, co:op reviews the task again. This includes a repair commit
that was on top of the branch during that review.

If co:op finds no matching commit or more than one, it stops before review and lists the task
and commits. There is one exception to the run's commit range: another session may have
committed a task before this run started, then completed it during the run. The reviewer sees
that task's single matching commit in the branch.

While an agent works on a task, co:op holds a lock on the host under
`~/.local/state/coop/task-leases/`. Another loop skips that task and can work on an independent
todo task. `coop tasks watch` reads the status stored beside the lock and shows `busy`, `stalled`
or `unleased`, without showing run IDs or PIDs.

A missing heartbeat can help you spot a stalled worker, but it does not release the task. The
loop takes over as soon as the operating system's lock becomes available, never because a
timeout has passed. This also lets it resume an in-progress task that no controller has locked.

When you run `coop tasks claim <id>`, the command records you as the owner and exits. It does
not keep a lock open. The loop leaves your task alone and shows the owner and
`coop tasks release <id>` command. Your claim stays in place until an explicit `release`,
`block`, `unblock` or `done` clears it; it never expires.

Stop running co:op controllers before a major-version upgrade. Running controllers from different
major versions against the same task queue is not supported.

When a task completes, co:op stores a small receipt on the host's persistent task lock file.
Other loops use that receipt to recognize the completed folder after its owner releases the
lock. They do not have to trust task metadata that the agent can edit.

Before starting a box that can write, including a review box, co:op also records a fingerprint
of each done folder in a host-only journal. It removes the journal only after checking the
box's queue changes. If the controller crashes, the next loop uses the journal to undo any
completion that had no authorized owner. Tasks completed outside a supervised box remain part
of the ordinary task history.

The `/sweep` skill has a `Stop` hook that keeps an active sweep running while it has tasks to
work on. It does not hold other sessions open. New projects already use this setup.
`coop init` preserves existing files, so update an older repo by hand:

1. Remove only the stock `Stop` group from `.claude/settings.json`. Keep `PreToolUse` and custom hooks.
2. Delete `.claude/hooks/stop-guard.sh`, but only if it's the old stock guard.
3. Remove a stale `.agent/active`.
4. Merge the current `.agent/skills/sweep/` files, keeping your local customizations.

`init` installs a fast commit check for Claude. For other committers, `init` installs a Git
pre-commit check at `.githooks/pre-commit` and sets `core.hooksPath`. The format check then runs
for everyone, including Codex, Gemini and a plain `git commit`. Git tracks the hook files, but
the `core.hooksPath` setting is local. Run `coop init` in each new clone to enable them there.

The tracked `.githooks/prepare-commit-msg` hook also calls co:op's box-attribution hook when it
is mounted. On a normal host, the shim does nothing.

An existing custom `core.hooksPath` stays unchanged. Copy both tracked hooks into your active hook
directory, or have your existing hooks call them. If you already have a `prepare-commit-msg`,
add the shim's co:op hook call to it instead of replacing the file. To skip the format check for
one commit, use `git commit --no-verify`.

## Final review

When the queue empties, a fresh final review (the signoff) checks each task the run
completed:

- every acceptance criterion and subtask is met
- the change follows `AGENTS.md` and `.agent/kb/rules`, with no scope creep
- the failure path is tested
- the change is polished, with docs and the CHANGELOG updated
- the bookkeeping is right

Before review, co:op finalizes each completed task's `state.md` in one atomic update. The worker
records its final checks there for the reviewer. Reviewers inspect the code and test coverage,
but rely on the worker's record of which tests ran; they do not run the tests again.

A missing, failed or outdated check causes the reviewer to reopen the task and tell the worker
what to check. Reviewers never edit an archived task in place. If they find that a task moved
through the queue incorrectly, they reopen and report it too.

If the reviewer reopens a task, the loop works through the queue and reviews again. It continues
until a review finds nothing to reopen or reaches the round limit. At the limit, co:op blocks
the repeatedly reopened task for your decision and does not report it as done.

By default, the loop allows half as many review rounds as tasks worked, rounded down, with
a minimum of three and a maximum of `5`. The range is `[3, signoff.rounds]`; setting a maximum
below three overrides the minimum.

Each completed task must match exactly one commit through its `Coop-Task: <id>` trailer. That
commit must be in the current iteration's commit range, and there must be exactly one matching
commit reachable from `HEAD`. When review requests rework, the worker must amend or rewrite the
existing task commit. A second matching commit is rejected, and the task returns to in-progress.

The worker also cannot add a trailer for another task or rewrite that task's commit. co:op
checks every attempt, including failed attempts, before allowing a retry. Review covers only
tasks that the host accepted as completed during this run.

For each task it reviews, the reviewer returns a structured evidence line. It also returns a
PASS/FAIL receipt with a sorted list of the exact task IDs to reopen. In the default
`writes: tasks` mode, the reviewer can only report findings: the repository and task queues are
read-only.

co:op checks the whole proposal, acquires ownership of every task under review on the host, and
reopens the proposed tasks in one transaction. The reviewer cannot change the queue directly.

If the review process finishes successfully but returns the wrong output format, co:op runs the
whole review once more. It uses the same tasks and write permissions, with a fixed instruction
to correct the receipt format. Each attempt gets its own telemetry record.

co:op leaves every task unchanged if the second verdict is malformed, a task changes state
during the review, the process is interrupted or fails, or the proposal includes a task outside
the review.

co:op stores reviewer findings in a delimited log block and treats them as untrusted. The next
worker gets fixed instructions to reproduce the issue first, instead of following instructions
from the reviewer. Use `writes: repo` to let the reviewer fix source. Every task queue is still
remounted read-only, and the host applies lifecycle changes.

Tune the loop in one committed `.agent/loop.yaml`, with a section per step:

| Section | Step |
|---|---|
| `preflight` | the cleanup pass before the loop starts working |
| `work` | the iterations that work the tasks |
| `between` | the per-task reviewer |
| `signoff` | the final review |
| `verify` | an optional affected-feature pass after the final review |

Set `agent:` for `work`, `between`, `signoff` or `verify` to choose its models. Each setting takes
a fallback list of targets (`provider[:model][/effort][@account]`) or preset names. For example,
`signoff.agent` can use a stronger model to review work done by a cheaper `work.agent`. Every
step except `work` also accepts a `prompt:`.

Custom prompts add to co:op's built-in checks; they do not replace them:

- `signoff.prompt` adds checks to final review. A task that fails one is reopened, just as it
  would be for a built-in check. You might require a CHANGELOG entry or regenerated docs.
- `preflight.prompt` adds an agent cleanup pass in addition to co:op's host-side unblock step.
- `between.prompt` defines an optional review after each completed task. It can reopen the task.
- `verify.prompt` defines the optional pass after final review.

Per-task review is off until you enable it and set a prompt. Changes to files that define the
gate are an exception: co:op always reviews those tasks before starting the next one. It uses
the configured between-review target and prompt, or the signoff target with a focused built-in
prompt. Both prompts name the task that just finished.

If Git cannot tell co:op which files changed, the loop stops. It cannot assume the gate was
untouched. Run the continuation command it prints to review that task before starting other
work. Final review also stops if co:op cannot read the run's changes.

co:op automatically reviews changes to Makefiles, CI, hooks and its own config. If your gate
also depends on wrapper scripts or ordinary source files, list their exact repo-relative paths
under `gate_sources:` in `.agent/project.yaml`. co:op cannot discover all those dependencies for
you.

The loop keeps the list it read at startup, so a task cannot remove itself from review. Restart
the loop to apply a deliberate change to the list. Projects without a list still get the
built-in protection.

Settings live in the same file: `signoff.rounds`, `preflight.enabled`, `verify.enabled` and `work.command`. Every field is optional, and a missing file means the built-in defaults. `coop init` scaffolds a fully commented starter.

## The .agent/ folder

`init` creates a folder for instructions and working notes that any supported agent can use.
The agent reads it when it starts and after its conversation is compacted. Git ignores the
local working state. It tracks the shared knowledge and rules (`kb/`, including `kb/rules/`),
workflows (`skills/`, `presets/`), fallback Claude settings (`claude/`), `project.yaml`,
`loop.yaml`, `compose.yml`, `Dockerfile` and `tasks/README.md`.

| Path | What it's for |
|---|---|
| `tasks/` | The work queue: one folder per task under `00_todo/`, `10_in_progress/`, `50_blocked/` and `99_done/`. A task's state is its directory, and `coop tasks` moves it. Each folder carries its own `spec.md`, `log.md`, `state.md` and `decision.md` as needed. The loop reads `00_todo/` and `10_in_progress/`. |
| the backlog | Unscheduled ideas stored as task folders in `tasks/xx_backlog/` (`coop backlog`). The loop and Stop hook ignore them. When an idea is ready, `coop backlog promote <id>` moves it into `tasks/00_todo/`. |
| `kb/` | Committed notes explaining subsystems, how they interact, and problems that are hard to spot from the code alone. |
| `kb/rules/` | Committed rules that tell agents how to work in the project. Corrections become rules here. |
| `claude/` | Fallback user-level Claude settings and hooks for repos without matching project `.claude/` artifacts. Committed. |
| `project.yaml` | Committed project settings: [`subprojects:`](#monorepos), [`serve:` ports](box.md#dev-servers-in-your-browser), box policy (`box:`), the merge `gate:`, and extra `gate_sources:` to review when changed. The `box:` policy sets egress, [`egress_rules:`](networking.md), resource caps and `auto_up`/`network`. Explicit `COOP_*` environment or config settings override `box:` and `gate:`. This project file can tighten security settings, but cannot relax them by itself: egress can require `filtered` or `offline`, while `open` needs human approval through `coop approve`. It cannot set `no_new_privileges`. |

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

You do not need to maintain `COOP_TASKS` by hand. An explicit `COOP_TASKS` or `--tasks` still
overrides automatic discovery. Each member keeps a queue for its own work, and the root queue
holds changes that span members.

Run `coop init` at the root to find member folders and write `project.yaml`. It looks for
`.agent/` folders at any depth, skipping hidden and build folders, and creates a task queue for
each member. Members share the root's AGENTS.md, `.agent/skills/`, `.agent/kb/` and box. A member
can also commit its own `.agent/kb/rules/` when it needs additional rules.

Use `coop tasks queues` when a script, such as the sweep queue guard, needs the resolved queue
paths.

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

Without `--tasks`, a fork takes work from the repo's `.agent/tasks` and every
[monorepo](#monorepos) member's queue. Use `--tasks <path>` to choose one queue. You can also give
an absolute path to a queue outside the project. The fork uses that original queue; it does not
copy it or change its path.

The fork receives only its assigned task under `.coop/task-executions/`. Restarting the same
fork generation resumes that task, and another fork cannot claim it.

`-d` (`--detach`) runs the worker in the background. co:op saves its output in a private log;
read it with `coop fork logs <name>`. When the fork finishes its available work, review and
[land it](forks.md#land-it) as you would a pull request, then `git push`. Add more agents only
while you can keep up with reviewing their work.

`coop fork merge --all` merges forks in sequence. It rebases each fork onto the result of the
previous merge and re-runs the configured gate (`gate:` in `.agent/project.yaml`, or
`COOP_GATE`). Each fork must pass against that updated base. The queue stops at the first
conflict or failed gate and leaves the remaining forks untouched.

`coop tasks split` and copying a whole queue into a fork are no longer supported. Those copies
let separate workers move the same task independently. A combined dashboard could show the
results but could not coordinate ownership. Use separate queues only for parts of the project
that already own separate work. `--tasks` selects an existing queue; it does not divide one.

If you run agents at the same time, give them different accounts to avoid competing for the
same subscription limits. A preset can define a rotation across providers and accounts.

A stale or crashed worker shows as `cleanup` in `coop fork ls`. Run `coop fork stop <name>` to
clean it up. If Coop started the worker on another operating system, such as a Linux container
that shares the folder, the listing says so. Stop that worker on the system where it started;
Coop cannot check it from here.

`stop` removes only that fork's containers, even if another repository has a fork with the same
name. Running it twice is safe.

Remove an unused fork with `coop fork rm <name>`. `--yes` skips the confirmation prompt.
`--force` may stop a detached worker and discard uncommitted or unmerged Git work, the reviewed
candidate and pending proposals. It also returns the fork's assigned tasks to their original
queue.
