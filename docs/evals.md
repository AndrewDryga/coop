# Evaluations

`coop eval` runs the same tasks repeatedly, then an independent verifier grades the finished work.
Use it to check whether changing a model, preset, loop recipe or co:op build improves the result.
The [website guide](https://coop.dryga.com/docs.html#evals) walks through a first run.

## Try it

1. Preview the shipped `core` suite. The preview launches no container or provider and spends
   nothing:

   ```sh
   coop eval run core codex --timeout 35m --dry-run
   ```

   A dry run checks the plan, not whether your credentials, runtime or image are ready.

2. For a real run, sign in with `coop login codex`. You also need a working container runtime and
   a current co:op box image. If the image is missing, run `coop build --egress open` from a
   directory without a project Dockerfile. Building may download dependencies. Then run:

   ```sh
   coop eval run core codex --timeout 35m
   coop eval inspect
   ```

   A real run uses your provider account and may spend credits.

3. Change one thing and run the same suite again. Find the run IDs with `coop eval runs`, then
   compare them with `coop eval compare <before-id> <after-id>`. co:op takes the order from your
   arguments, not from timestamps. Inspect and compare show a short fingerprint of each
   configuration and its co:op build, so you can spot a preset or recipe that changed while keeping
   the same name.

`coop help eval run` lists every option, including these:

| Flag | What it does |
|---|---|
| `--timeout 35m` | Limits the whole command from the moment it starts. At the limit, preparation, trials and grading stop, though cleanup and saving the final record may finish afterward. It limits time, not spending. |
| `--repeat 3` | Runs each case three times. |
| `--jobs 2` | Allows two agent trials at once. |

Each case also has its own timeout in `suite.yaml`, and a larger `--timeout` doesn't raise it.
More configurations or repeats mean more paid work.

Install `cloc` if you want change-size figures. Without it, grading and verdicts still work, and
co:op labels the measurement as missing.

## Choose what to evaluate

- `core` has three small coding tasks for one agent, with independent checks that catch near
  misses. Choose a provider target such as `codex:gpt-6.1-sol/xhigh`.
- `queue` sends ten tasks through the real `coop loop`, including review and signoff. Use it to
  compare presets or loop recipes. Each trial works through its tasks in order and can take
  hours. For example, `coop eval run queue frontier --timeout 2h` uses your existing `frontier`
  preset. Add `--loop-config .agent/loop.yaml` to test your project's recipe.
- `coop eval init ./evals/my-suite` creates a custom suite from a working greeting example.
  Edit `suite.yaml`, `files/greeting/` and the separate `verifiers/greeting/verify.sh`. The
  verifier's README explains grading. `suite.yaml` includes a commented example for a loop.
- [`examples/evals/maintenance/`](../examples/evals/maintenance/) is an example of a custom
  comparison, with three independent maintenance loops for shell, Python and Node. It isn't an
  official benchmark score, and `coop eval ls` doesn't list it as a starter.

Each target or preset after the suite is a separate configuration, not a fallback. Presets need
a loop suite such as `queue`, and `core` and other single-agent suites accept targets only. This
command previews two configurations:

```sh
coop eval run core codex:gpt-6.1-sol codex:gpt-6.1-sol/xhigh --timeout 70m --dry-run
```

## Runtime profiles for custom agent cases

For an agent case that needs its own tools, use a trusted runtime profile, which gives the case a
dedicated image that you build yourself in place of the shared image. Add these four fields to the
case:

```yaml
runtime:
  profile: /absolute/path/to/clean-profile
  workdir: /app
  agent_timeout: 15m
  verifier_timeout: 15m
```

Set the case's `timeout` long enough to cover preparation, the agent's work and verification.
Loop suites don't support runtime profiles.

Keep the profile outside both the suite and co:op's evaluation state. It must contain no Git
metadata, symlinks or special files. Its project Dockerfile defaults to `.agent/Dockerfile` and
must inherit co:op's client image. The profile can't add project environment variables,
services, published ports or extra network access.

Review the files used to build the profile, then run `coop build --egress filtered` from the
profile directory before you start the evaluation. Evaluations never build a profile themselves.
The build uses ordinary host networking, not the filtered network of the candidate (the agent
being evaluated).

Profile cases always use the `coop-adapted-offline-profile-v1` protocol, which fixes these
settings:

| Setting | Value |
|---|---|
| Working directory | `/app` |
| CPU | 1 |
| Memory | 2 GiB |
| PIDs | 128 |
| Storage | declared as 10 GiB, but co:op doesn't enforce the quota or measure usage |
| Candidate network | only the core endpoints co:op maintains for its provider, through filtered networking |
| Grader | the same immutable image, with no network or credentials |

These settings adapt the workload to co:op's runtime and don't show that it runs under the same
conditions as an official benchmark. Exclude cases that depend on a disk quota or on running out
of disk space.

co:op saves a private copy of the profile and records its digest, image and platform before work
starts. This saved copy doesn't approve a build. The original profile must stay available and
unchanged for both the agent phase and the verifier phase, and if you rebuild a different image
after preparation, co:op refuses to start the candidate.

The saved profile counts toward the suite's 2 GiB and 200,000-entry limit. A dry run doesn't look
up the image, so the workload fingerprint it shows isn't final.

## Understand a result

`coop eval inspect` shows the latest run, or an older one if you add its run ID. It shows why
trials didn't pass and where to find the saved JSON records and work. See `coop help eval inspect`
for explanations of the results.

| Result | Meaning |
|---|---|
| `failed` | The independent verifier rejected the work. |
| `error` | Execution or grading didn't produce a verdict, so grading is incomplete. It doesn't mean the model failed the task. |
| A timeout, or a trial that never started | Grading is incomplete here too. |

A loop trial starts when its loop launches its first box. If the loop stops before that, for
example because the image is missing or the account isn't signed in, the trial is an error, not a
graded failure.

For a real run, exit code 0 means every trial has a grade, even if some tasks failed. If grading
is incomplete, the command exits 1 and saves the results so you can inspect what happened.
A dry run exits 0 after showing the plan. Code-size changes can help you review the work, but
they aren't a quality score.

To measure what changed between two runs:

- Choose two different runs.
- Run the same tasks, with the same number of repeats for each task.
- Test one configuration, such as a model or preset, in each run.

co:op also checks the saved workload IDs, which identify the tasks and how they run and are
graded. Both runs must have an ID, and the IDs must match. If any of these requirements aren't
met, co:op shows each run's results separately.

`coop eval compare` checks the saved plan for each run. A missing trial stays pending. If a
trial is recorded twice or wasn't part of the plan, co:op refuses to compare the results.

For each task, co:op reports the change only after every repeat in both runs has a pass or fail
grade. The overall change and its uncertainty range appear only when all tasks are fully graded
in both runs.

The report shows the pass rate after the change minus the pass rate before it, in percentage
points, counting tasks and repeats separately. It also gives a 95% uncertainty range, which applies
only to the tasks you tested and assumes each trial runs independently. The range uses a
conservative Hoeffding bound, `sqrt(log(40)/(cases*repeats))`, and stays between −100 and +100
percentage points.

If you plan to make a decision from the comparison, run each task at least three times. Before you
start, write down how much improvement would matter and how you'll decide. co:op doesn't save
those rules or choose a winner for you.

If a trial doesn't pass, co:op keeps the candidate's original workspace for inspection and removes
the writable copy used for grading. The grader receives only a cleaned copy of the workspace, and
the trial details list any symlinks that point outside the workspace and any special files left
out of that copy.

If a provider refuses a sign-in or reports a quota limit, check `coop credentials <agent>` before
starting another paid run. For a timeout, check both the case's timeout in `suite.yaml` and the
run's `--timeout`.

A run with no final summary may still be running or may have been interrupted. You can inspect
it, but you can't compare or resume it. To try again, start a new run yourself. Recorded
diagnostics may contain private provider output, so review them before you share them.

Each trial gets a private workspace without the source repository's Git history. The model
can't access the hidden verifiers, which run afterward in a separate container with no
credentials or network access.

co:op excludes your MCP servers and `COOP_RUN_ARGS` from trials, because they could change the
task setup without co:op recording the change. Use co:op-managed logins for provider credentials.

The results describe the workload you tested. They don't prove equal performance across
providers, and they aren't a public benchmark ranking.

co:op turns off the native client's web search and fetch tools for every evaluation candidate,
including consultations and delegated work that co:op manages. Terminal, file and editing tools
stay available. This relies on the client obeying the setting, and it won't stop a model from
making its own authenticated provider requests. Regular co:op runs keep their normal
web tools.

Before trials start, co:op saves private copies of the suite's named inputs and hidden
verifiers. The run uses those copies, so later edits to the source can't change its tasks or
the workload ID used for comparisons. The saved suite must fit within 2 GiB and 200,000 entries,
or co:op refuses the run before any provider work begins.
