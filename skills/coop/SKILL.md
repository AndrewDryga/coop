---
name: coop
description: Use Coop (co:op) to run Claude Code, Codex, Gemini CLI or Grok in a sandbox, delegate changes to repository forks, and work task queues unattended. Use when the user asks to operate Coop, set up a Coop project, review a Coop fork, or diagnose a Coop launch or loop.
license: MIT
---

# Coop

[Coop](https://coop.dryga.com) runs coding agents in containers with secret-file
hiding and filtered networking. A direct run edits the project it mounts; use a
fork when the user wants work kept out of their checkout.

This skill operates Coop in the user's project. It does not impose the Coop
source repository's development rules, Make targets or release process.

## Establish where you are

- If `COOP_BOX=1`, you are already inside a box. Use the runtime's tools for
  the assigned work and its `coop-tasks` tools for task state when available.
  Host commands below are for the operator outside the box; do not install
  Coop inside the box or attempt nested orchestration. Give the operator the
  specific host action needed when a launch, approval or fork operation is required.
- On the host, check `command -v coop`, `coop --version`, the project folder,
  and its instructions. Inspect Git status before choosing direct work or a fork.
  Follow the user's chosen agent and workflow.
- Use the installed CLI's `coop help <command>` to check flags before an
  unfamiliar operation. These examples are starting points, not a replacement
  for the installed version's help.

## Set up a project

Coop needs macOS or Linux and Docker. If the binary is missing, use the
[installation guide](https://coop.dryga.com/install.html); installing this skill
does not install Coop. For a new setup, the operator signs in before initializing:

```sh
coop login claude
coop init
coop doctor
```

Substitute the requested provider. Login is an interactive user step; do not
collect credentials in chat. `coop init` creates project settings, task
infrastructure and agent instructions: inspect existing files before running it,
and do not reinitialize an established project to repair an unrelated failure.

## Run in the current checkout

```sh
coop codex
coop codex -p "Investigate the failing parser test and report the cause."
coop run -- npm test
```

Choose an interactive session for ongoing work, `-p` for one prompt, or `run --`
for an ordinary command. Direct runs can change the mounted project. An agent
started from this skill still needs a concrete task and acceptance criteria.

Coop consumes its own leading help and network options. To ask the underlying
agent for help, pass `--`, for example `coop codex -- --help`. To resume, use
the exact resume command Coop printed when that agent exited.

## Delegate to a fork

Check `coop fork ls` before choosing a name: opening an existing name resumes
that fork rather than creating a new one.

```sh
coop fork parser codex --isolated
coop fork review parser --stat
coop fork review parser
```

An ordinary fork separates working files but can share Git object storage.
`--isolated` creates independent Git storage and checks committed changes before
publication. It is creation-only; an existing ordinary fork cannot be converted
in place. Choose a new name when isolation is needed.

Review the actual changes and check results. When merging is within the user's
requested outcome, use `coop fork merge parser`. Merge rebases onto the current
branch and normally removes the fork afterward; review alone does not merge it.
Do not use `--force`, `--fresh`, fork deletion or `coop update` as routine repair
or cleanup: they can discard work or stop processes.

## Prepare and run a task queue

Inspect the existing queue with `coop tasks` first. Add only the work the user
asked to run. A ready task states the problem, context, verifiable acceptance
criteria, approach and concrete subtasks. For example:

```sh
coop tasks add "Add a health endpoint" \
  --context "The load balancer needs a cheap liveness probe." \
  --acceptance "GET /health returns 200 with JSON {\"ok\":true}; a handler test verifies it." \
  --approach "Add the route beside the existing routes and test its response." \
  --subtask "Implement the route and handler" \
  --subtask "Verify the response and run project checks"
```

Loops drain their selected queues, including existing tasks. Without `--tasks`,
the selection comes from `COOP_TASKS` or the project's queues. Inspect that scope
before launching; adding one task does not authorize working unrelated tasks.
Use `--tasks <queue-path>` to select an authorized queue, not a single task.

`coop loop codex` works those queues in the current checkout. For background
work in a separate copy, use a new fork and an explicit queue path:

```sh
coop fork health codex --isolated --loop -d --tasks .agent/tasks
coop fork ls
coop fork logs health
coop tasks decisions
```

Use `coop fork review health` before an authorized merge. A detached launch is
only a start, not proof of completion. Inspect task state, review and checks.
Blocked decisions need a real answer; a loop paused by a task limit may still
need final review. Do not declare success from its exit code alone.

## Diagnose a refusal

Read the full error and follow the remedy it names. Check `coop doctor` for
runtime/isolation problems, `coop fork ls` and `coop fork logs <name>` for fork
progress, and `coop tasks decisions` for tasks waiting on the user.

Networking is filtered by default. If the project needs another destination,
identify the exact domain and use the documented project request and host
approval flow. Do not switch to `--egress open`, expose credentials or run a
failed sandbox command directly on the host just to bypass a refusal. Fork
approvals are snapshots: a later approval in the parent does not update an
existing fork. Use the fork-specific command printed in its refusal.

Report what actually happened: started, still running, blocked, checks passed,
reviewed or merged. Name missing evidence rather than treating it as a pass.

Read only the relevant public guide for further detail:
[agents](https://coop.dryga.com/agents.html),
[forks](https://coop.dryga.com/forks.html),
[task loops](https://coop.dryga.com/loop.html),
[networking](https://coop.dryga.com/networking.html).
