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

A task is a folder under `.agent/tasks/`. Its state is the directory it sits in: `00_todo/`,
`10_in_progress/`, `50_blocked/` or `99_done/`. The numeric prefix sorts `ls` in lifecycle order,
and `coop tasks` shows the clean names.

Before starting an agent, the loop claims the next task on the host or resumes one left in
`10_in_progress/`, and the agent works only on that task. The loop keeps going while it can claim
work from `00_todo/` or `10_in_progress/`.

A task that must not land before another one can wait for it. The agent hands it back with
`tasks_wait`, or you add `waits_on: <task-id>` to its `task.md` frontmatter. The task stays in
`00_todo/` and the loop skips it until the other task is in `99_done/`, then works it. If the
other task is missing or blocked, the loop blocks the waiting task too, with a decision that says
why. If a commit lands after the agent hands a task back, the loop refuses the wait and stops,
unless the commit carries another task's `Coop-Task` trailer. The task returns to
`10_in_progress/` with the commit kept. `coop tasks ls` shows what each task waits for.

Choose an agent (`claude`, `codex`, `gemini` or `grok`) or a preset that defines the lead agent.
To run a custom command for each task, set `work.command` in `.agent/loop.yaml`. It replaces the
whole iteration command.

When the model reaches a rate or usage limit, the loop tries the next configured target. If
every target is limited, it waits with a countdown until one can run, then retries the same
task. A rate limit doesn't count as a failed task attempt.

Add `--preflight` to check the queue before work starts. This pass unblocks tasks in
`50_blocked/` when their `decision.md` has an answer. It leaves backlog tasks parked with
`coop tasks park` alone, does no task work and makes no commits. Preflight is off by default.
A task its worker committed before blocking it waits for your answer to
`coop tasks unblock <id> "<answer>"`, which names that commit and lets the next worker close the
task without a new commit.

## Exit codes

A cron job or CI can act on the loop's exit code without parsing its output:

| Exit code | Meaning |
|---|---|
| `0` | Final review and enabled final checks passed, or a successful `--max-tasks` pause before final review. |
| `1` | A failure, an unverified final pass, or work still left to do. |
| `2` | A usage error. |
| `3` | The loop stopped with a task blocked on a human decision, including one the review kept reopening past the round limit. Resolve it with `coop tasks decisions`, then run the loop again. |
| `130` | The run was interrupted before the final verdict. |

A successful `--max-tasks` pause exits `0`, but the work still needs review. Run the same command
without the limit to finish that review.

The resumed review uses the `Coop-Task: <id>` trailer to find each task's commit among the
commits made since the run started. You can sign those commits between runs. If any commit seen
by a previous review has changed, including a repair commit that was on top of the branch, co:op
reviews the task again. If it finds no matching commit or more than one, it stops before review
and lists the task and commits.

### Task locks and claims

While an agent works on a task, co:op holds a lock on the host under
`~/.local/state/coop/task-leases/`, so another loop skips that task and can work on an independent
one. `coop tasks watch` shows how long the task has been worked, how long its heartbeat has been
quiet, that its loop waits out a usage limit, or that its worker is gone or you stopped it, without
run IDs or PIDs.

A missing heartbeat can help you spot a stalled worker, but it doesn't release the task. The loop
takes over as soon as the operating system's lock becomes available, never because a timeout has
passed. It can also resume an in-progress task that no co:op process, such as a running
`coop loop`, has locked.

`coop tasks claim <id>` records you as the owner and exits, without keeping a lock open. The loop
leaves your task alone and shows the owner and the `coop tasks release <id>` command. Your claim
never expires: only an explicit `release`, `block`, `unblock` or `done` clears it.

`coop tasks claim <id> --force` takes over a claim that another running process holds. It ends that
process's `coop tasks lease`, and that process can no longer complete, block, release or lease the
task, or claim it back without `--force`, from the host or from a box.

Stop every running co:op process, such as `coop loop`, before a major-version upgrade. Processes
from different major versions can't share a task queue.

### Completion records

When a task completes, co:op stores a small receipt on the host's persistent task lock file, so
other loops can recognize the completed folder without trusting task metadata the agent can edit.

Before starting a box that can write, including a review box, co:op also records a fingerprint of
each done folder in a host-only journal. If co:op crashes, the next loop uses the journal
to undo any completion that had no authorized owner. Tasks completed outside a supervised box
remain part of the ordinary task history.

### Sweeps and commit hooks

The `/sweep` skill has a `Stop` hook that keeps an active sweep running while it has tasks to work
on, without holding other sessions open. `coop init` sets it up in new projects. A repo set up by
an older co:op needs [a manual update](../MIGRATING.md#the-sweep-stop-hook-in-older-repos).

`init` installs a fast commit check for Claude. For other committers, it installs a Git pre-commit
check at `.githooks/pre-commit` and sets `core.hooksPath`, so the format check runs for everyone,
including Codex, Gemini and a plain `git commit`. Git tracks the hook files, but `core.hooksPath`
is local, so run `coop init` in each new clone to enable them there.

The tracked `.githooks/prepare-commit-msg` hook also calls co:op's box-attribution hook when that
hook is mounted. On a normal host, the tracked hook does nothing.

`init` leaves an existing custom `core.hooksPath` unchanged. Copy both tracked hooks into your
active hook directory, or have your existing hooks call them. If you already have a
`prepare-commit-msg`, keep your file and copy the tracked hook's call to the co:op hook into it. To
skip the format check for one commit, use `git commit --no-verify`.

## Final review

When the queue empties, a fresh final review (the signoff) checks each task the run
completed:

- every acceptance criterion and subtask is met
- the change follows `AGENTS.md` and `.agent/kb/rules`, with no scope creep
- the failure path is tested
- the change is polished, with docs and the CHANGELOG updated
- the bookkeeping is right

The worker records its final checks in the task's `state.md`, which co:op finalizes before review.
Reviewers inspect the code and test coverage, and they rely on the worker's record of which tests
ran instead of running them again.

If a check is missing, failed or outdated, the reviewer reopens the task and tells the worker what
to check. Reviewers never edit an archived task in place. If they find that a task moved through
the queue incorrectly, they reopen it and report it too.

The loop then works the queue and reviews again, until nothing is reopened or the round limit is
reached. At the limit, co:op blocks that task for your decision and doesn't report it as done.

The loop allows half as many review rounds as tasks worked, rounded down, within the range
`[3, signoff.rounds]`. `signoff.rounds` defaults to `5` and must be 1 or more. Setting it below
three overrides the minimum.

Each completed task must match exactly one commit through its `Coop-Task: <id>` trailer. That
commit must be in the current iteration's commit range, and it must be the only match reachable
from `HEAD`. When review requests rework, the worker must amend or rewrite the existing task
commit. co:op rejects a second matching commit and returns the task to in-progress.

The worker also can't add a trailer for another task or rewrite that task's commit. co:op checks
every attempt, including failed attempts, before allowing a retry. Review covers only tasks that
the host accepted as completed during this run.

In the default `writes: tasks` mode, the reviewer can only report findings: the repository and task
queues are read-only. It returns a PASS/FAIL receipt listing the exact task IDs to reopen, and
co:op checks the whole proposal and reopens those tasks in one transaction.

If the review finishes successfully but returns the wrong output format, co:op asks for one
correction. It resumes the same reviewer session for one more turn, with the same tasks and write
permissions, and asks only for the verdict in the right format. It doesn't run the review again.

co:op leaves every task unchanged if:

- the corrected verdict is malformed, or the reviewer session can't be resumed
- a task changes state during the review
- the process is interrupted or fails
- the proposal includes a task outside the review

co:op treats reviewer findings as untrusted. The next worker gets fixed instructions to reproduce
the issue first and doesn't follow instructions from the reviewer. Use `writes: repo` to let the
reviewer fix source. Every task queue stays read-only, and the host applies changes to task state.
The reviewer's commits must go on top of the commit it reviewed, without merges or `Coop-Task`
trailers, and co:op lists them after the verdict. Uncommitted changes or rewritten history still
fail the review.

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

Custom prompts add to co:op's built-in checks and don't replace them:

- `signoff.prompt` adds checks to final review. co:op reopens a task that fails one, as it does
  for a built-in check. You might require a CHANGELOG entry or regenerated docs.
- `preflight.prompt` adds an agent cleanup pass in addition to co:op's host-side unblock step.
- `between.prompt` defines an optional review after each completed task. It can reopen the task.
- `verify.prompt` defines the optional pass after final review.

Per-task review is off until you enable it and set a prompt. Changes to files that define the
gate are an exception: co:op always reviews those tasks before starting the next one, using the
configured between-review target and prompt, or the signoff target with a focused built-in prompt.

If Git can't tell co:op which files changed, the loop stops, because it can't assume the gate was
untouched. Run the continuation command it prints to review that task before starting other work.
Final review also stops if co:op can't read the run's changes.

co:op automatically reviews changes to Makefiles, CI, hooks and its own config. If your gate
also depends on wrapper scripts or ordinary source files, list their exact repo-relative paths
under `gate_sources:` in `.agent/project.yaml`.

The loop keeps the list it read at startup, so a task can't remove itself from review. Restart
the loop to apply a deliberate change to the list.

Settings live in the same file: `signoff.rounds`, `preflight.enabled`, `verify.enabled` and
`work.command`. Every field is optional, and without the file co:op uses the built-in defaults.
`coop init` creates a fully commented starter file.

## The .agent/ folder

`init` creates a folder for instructions and working notes that any supported agent can use.
The agent reads it when it starts and after its conversation is compacted. Git ignores the
local working state and tracks the shared knowledge and rules (`kb/`, including `kb/rules/`),
workflows (`skills/`, `presets/`), fallback Claude settings (`claude/`), `project.yaml`,
`loop.yaml`, `compose.yml`, `Dockerfile` and `tasks/README.md`.

| Path | What it's for |
|---|---|
| `tasks/` | The work queue, one folder per task. Each folder carries its own `spec.md`, `log.md`, `state.md` and `decision.md` as needed, and `coop tasks` moves it between states. |
| the backlog | Unscheduled ideas stored as task folders in `tasks/xx_backlog/` (`coop backlog`). The loop and Stop hook ignore them. When an idea is ready, `coop backlog promote <id>` moves it into `tasks/00_todo/`. |
| `kb/` | Committed notes explaining subsystems, how they interact, and problems that are hard to spot from the code alone. |
| `kb/rules/` | Committed rules that tell agents how to work in the project. Corrections become rules here. |
| `claude/` | Fallback user-level Claude settings and hooks for repos without matching project `.claude/` artifacts. Committed. |
| `project.yaml` | Committed project settings: [`subprojects:`](#monorepos), [`serve:` ports](box.md#dev-servers-in-your-browser), box policy (`box:`), the merge `gate:`, and extra `gate_sources:` to review when changed. The `box:` policy sets egress, [`egress_rules:`](networking.md), resource caps and `auto_up`/`network`. Explicit `COOP_*` environment or config settings override `box:` and `gate:`. This file can tighten security settings but cannot relax them by itself. Egress can require `filtered` or `offline`, but `open` needs human approval through `coop approve`. The file cannot set `no_new_privileges`. |

On a fresh setup, `init` also installs generic workflow skills into `.agent/skills/` and links the
selected agents to it:

| Skill | What it's for |
|---|---|
| `/spec` | writing a spec for a multi-file change |
| `/work` | carrying out that change step by step against the gate |
| `/sweep` | working through every task in `.agent/tasks/` |
| `/investigate` | finding the root cause of a failure |
| `/verify-api` | checking anything you're unsure of before you call it |
| `/review-board` | a thorough multi-hat review before landing |

Edit them freely. `init` won't overwrite a skill you've changed. When `init` adopts an existing
real `.claude/skills/` instead, it leaves that project-owned set unchanged and links the other
selected agents to it.

## Monorepos

When one repo holds several components, each with its own work, list them once in the top-level
`.agent/project.yaml`:

```yaml
# .agent/project.yaml — committed with the repo
subprojects: [runner, packs, portal, mcp]
```

co:op then combines every member's `.agent/tasks` automatically:

- `coop tasks` lists them under a header for each queue
- one `coop loop` works through them all
- `coop prompt` counts across them
- the id commands (`claim`, `done` and the rest) find a task in whichever queue holds it

An explicit `COOP_TASKS` or `--tasks` overrides automatic discovery. Each member keeps a queue for
its own work, and the root queue holds changes that span members.

Run `coop init` at the root to find member folders and write `project.yaml`. It looks for
`.agent/` folders at any depth, skipping hidden and build folders, and creates a task queue for
each member. Members share the root's AGENTS.md, `.agent/skills/`, `.agent/kb/` and box. A member
can also commit its own `.agent/kb/rules/` when it needs additional rules.

Use `coop tasks queues` when a script, such as the sweep queue guard, needs the resolved queue
paths.

## Parallel forks

Run several models at once, each looping unattended in its own [fork](forks.md). Point them all at
the same queue in your project, called the canonical queue. The host assigns the tasks, so each
task is claimed exactly once:

```bash
coop fork perf codex  --loop -d # each worker claims a different canonical task
coop fork deps gemini --loop -d
coop fork docs claude --loop -d

coop tasks watch                # canonical queue + assignments + every active sandbox
coop fork ls                    # worker state, sandbox activity, candidate/task progress, diff, cost
coop fork logs -f               # tail every fork at once (compose-style, prefixed)
coop fork stop perf             # halt one; coop fork logs perf -f to watch just it
```

Without `--tasks`, a fork takes the queues `COOP_TASKS` names, as `coop loop` does. Without
either, it takes work from the repo's `.agent/tasks` and then every [monorepo](#monorepos)
member's queue, in `project.yaml` order. Use `--tasks <path>` to choose a queue, and repeat it for
more: like `coop loop`, the fork takes them in the order you give them. A fork's `--tasks` queue
can be an absolute path outside the project, and the fork uses the original queues without copying
them or changing their paths. Nothing else takes a queue outside the project: `COOP_TASKS` names
queues inside it, and `coop loop`, `coop tasks` and `coop backlog` refuse to run in a folder
outside a Git repository.

The fork receives only its assigned task under `.coop/task-executions/`. Restarting the same
fork generation resumes that task, and another fork can't claim it. A fork recreated under the
same name is a new generation.

`-d` (`--detach`) runs the worker in the background and saves its output in a private log. Read
it with `coop fork logs <name>`. When the fork finishes its available work, review and
[land it](forks.md#land-it) as you would a pull request, then `git push`. Add more agents only
while you can keep up with reviewing their work.

`coop fork merge --all` merges forks in sequence. It rebases each fork onto the result of the
previous merge and re-runs the configured gate (`gate:` in `.agent/project.yaml`, or
`COOP_GATE`). The merge stops at the first conflict or failed gate and leaves the remaining forks
untouched.

To split work between forks, point them at the same queue. The host gives each task to one fork.
Use separate queues only for parts of the project that already own separate work. `--tasks`
selects an existing queue and doesn't divide it.

If you run agents at the same time, give them different accounts to avoid competing for the
same subscription limits. A preset can define a rotation across providers and accounts.

Agents running at the same time also run their full gates at the same time, which can overload
the host until tests time out. To limit that, tell your agents to run the full gate as
`coop-gate-slot make check` (or put `coop-gate-slot` in front of your own gate command). Every
box has this helper. At most two gates run at once on one host; another one prints `Waiting for
another gate to finish` and starts when a slot is free, so its timeouts start only then. The
`coop fork merge` gate takes a slot the same way. A gate inside another gate runs in the outer
gate's slot, so a `gate:` command that already starts with `coop-gate-slot` doesn't wait for a
second one. Boxes share the slots through their common
`~/.cache` volume on Docker and OrbStack. Apple container runs each box in its own VM, and
sharing slots there hasn't been tested, so don't count on the limit there.

A stale or crashed worker shows as `cleanup` in `coop fork ls`. Run `coop fork stop <name>` to
clean it up. If co:op started the worker on another operating system, such as a Linux container
that shares the folder, the listing says so. Stop that worker on the system where it started,
because co:op can't check it from here.

`stop` removes only that fork's containers, even if another repository has a fork with the same
name. Running it twice is safe.

Remove an unused fork with `coop fork rm <name>`. `--yes` skips the confirmation prompt.
`--force` may stop a detached worker and discard uncommitted or unmerged Git work, the reviewed
candidate and pending proposals. It also returns the fork's assigned tasks to their original
queue.
