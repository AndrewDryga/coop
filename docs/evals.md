# Evaluations

`coop eval` runs the same tasks repeatedly, then grades the finished work independently. Use it
to check whether changing a model, preset, loop recipe or co:op build improves the result. Keep
the suite and planned set of trials the same before and after the change so the runs are
comparable. The [website guide](https://coop.dryga.com/docs.html#evals) walks through a first run.

## Try it

1. Preview the shipped `core` suite. The preview launches no container or provider and spends
   nothing:

   ```sh
   coop eval run core codex --timeout 35m --dry-run
   ```

   A dry run validates the plan. It doesn't check that your credentials, runtime or image are
   ready.

2. For a real run, sign in with `coop login codex`. You also need a working container runtime and
   a current co:op box image. If the image is missing, run `coop build --egress open` from a
   directory without a project Dockerfile. Building may download dependencies. Then run:

   ```sh
   coop eval run core codex --timeout 35m
   coop eval inspect
   ```

   A real run uses your provider account and may spend credits.

3. Change one thing and run the same suite again. Find the run IDs with `coop eval runs`, then
   compare them with `coop eval compare <before-id> <after-id>`. The first argument is the before
   run and the second is the after run; co:op does not use timestamps to choose the order. Inspect
   and compare show a short fingerprint of each configuration and its co:op build. This lets you
   spot a preset or recipe that changed while keeping the same name.

Use these flags to set the overall time limit, the number of repeats and how many trials can
run at once. `coop help eval run` lists every option.

| Flag | What it does |
|---|---|
| `--timeout 35m` | Limits the whole command from the moment it starts. At the limit, preparation, trials and grading stop. Cleanup and saving the final record may finish afterward. This limits time, not spending. |
| `--repeat 3` | Runs each case three times. |
| `--jobs 2` | Allows two agent trials at once. |

Each case also has its own timeout. More configurations or repeats mean more paid work.

`cloc` is optional. Install it for change-size figures. Grading and verdicts still work without it,
and co:op labels the missing measurement instead of reporting a false zero.

## Choose what to evaluate

- `core` has three small coding tasks for one agent. Independent checks catch deliberate near
  misses: changes that appear to solve the task but miss a requirement. Choose a provider target
  such as `codex:gpt-6.1-sol/xhigh`; this suite does not use presets.
- `queue` sends ten tasks through the real `coop loop`, including review and signoff. Use it to
  compare presets or loop recipes. Each trial works through its tasks in order and can take
  hours. For example, `coop eval run queue frontier --timeout 2h` uses your existing `frontier`
  preset. Add `--loop-config .agent/loop.yaml` to test your project's recipe.
- `coop eval init ./evals/my-suite` creates a custom suite from a working greeting example.
  Edit `suite.yaml`, `files/greeting/` and the separate `verifiers/greeting/verify.sh`. The
  verifier's README explains grading. The manifest includes a commented example for a loop.
- [`examples/evals/maintenance/`](../examples/evals/maintenance/) contains three independent
  maintenance loops for shell, Python and Node. Its offline checks confirm that unfinished
  starting versions fail and completed reference versions pass. It demonstrates a custom
  comparison; it is neither an official benchmark score nor a starter listed by
  `coop eval ls`.

Each target or preset after the suite is a separate configuration, not a fallback. This command
previews both configurations:

```sh
coop eval run core codex:gpt-6.1-sol codex:gpt-6.1-sol/xhigh --timeout 70m --dry-run
```

## Runtime profiles for custom agent cases

For an agent case that needs its own tools, use a trusted runtime profile: a dedicated image
you build explicitly instead of the shared image. Add these four fields to the case:

```yaml
runtime:
  profile: /absolute/path/to/clean-profile
  workdir: /app
  agent_timeout: 15m
  verifier_timeout: 15m
```

Set the case's `timeout` long enough to cover preparation, the agent's work and verification.
Loop suites do not support runtime profiles.

Keep the profile outside both the suite and co:op's evaluation state. It must contain no Git
metadata, symlinks or special files. Its project Dockerfile defaults to `.agent/Dockerfile` and
must inherit co:op's client image. The profile cannot add project environment variables,
services, published ports or extra network access.

Review the files used to build the profile. Then run `coop build --egress filtered` from the
profile directory before starting the evaluation. The build itself uses ordinary host
networking; it does not use the candidate's filtered network.

Profile cases run under the fixed `coop-adapted-offline-profile-v1` protocol:

| Setting | Value |
|---|---|
| Working directory | `/app` |
| CPU | 1 |
| Memory | 2 GiB |
| PIDs | 128 |
| Storage | declared as 10 GiB, but the quota is unenforced and usage is unmeasured |
| Candidate network | only its provider's core filtered networking |
| Grader | the same immutable image, with no network or credentials |

These settings adapt the workload to co:op's runtime. They do not establish that it runs under
the same conditions as an official benchmark. Exclude cases that depend on a disk quota or on
running out of disk space.

co:op saves a private copy of the profile. Before work starts, it records the profile's digest,
the measured image and platform, and the time allowed for each phase. This saved copy does not
approve a build: the original profile must remain available and unchanged for both phases. If
you rebuild a different image after preparation, co:op refuses to start the candidate.

The saved profile counts toward the suite's 2 GiB / 200,000-entry staging limit. A dry run does
not resolve the image, so its workload fingerprint is provisional.

## Understand a result

`coop eval inspect` shows the latest run. Add a run ID to inspect an older one. It shows why
trials did not pass and where to find the saved JSON records and work. See `coop help eval inspect`
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
A dry run exits 0 after showing the plan, without running trials. Code-size changes can help
you review the work, but they are not a quality score.

To measure what changed between two runs:

- Choose two different runs.
- Run the same tasks, with the same number of repeats for each task.
- Test one configuration, such as a model or preset, in each run.

co:op also checks the saved workload IDs, which identify the tasks and how they run and are
graded. Both runs must have an ID, and the IDs must match. If any of these requirements are not met, co:op shows
each run's results separately.

`coop eval compare` checks the saved plan for each run. A missing trial stays pending. If a
trial is recorded twice or was not part of the plan, co:op refuses to compare the results.

For each task, co:op reports the change only after every repeat in both runs has a pass or fail
grade. The overall change and its uncertainty range appear only when all tasks are fully graded
in both runs.

The report counts tasks and repeats separately. It shows the pass rate after the change minus
the pass rate before it, in percentage points. For example, going from 60% to 70% is an increase
of 10 percentage points.

The report also gives a 95% uncertainty range for that change. A wider range means a less precise
estimate. This applies only to the tasks you tested and assumes each trial runs independently.
The calculation uses a conservative Hoeffding bound: `sqrt(log(40)/(cases*repeats))`. The range
cannot extend below −100 or above +100 percentage points.

Repeating a task helps estimate performance on that task. It does not tell you how the model
will perform on new tasks. If you plan to make a decision from the comparison, run each task at
least three times. Before you start, write down how much improvement would matter and how you
will decide whether the results are good enough. co:op does not save those rules or choose a
winner for you.

co:op measures the finished work before running the verifier, so verifier build artifacts do
not count as model work. If a trial does not pass, co:op keeps the candidate's original workspace
for inspection and removes the writable copy used for grading. The grader receives only a
sanitized snapshot. The trial details list any external links or special files left out of it.

If a provider refuses a sign-in or reports a quota limit, check `coop credentials <agent>` before
starting another paid run. For a timeout, check both the case's timeout in `suite.yaml` and the
run's `--timeout`.

A run with no final summary may still be running or may have been interrupted. You can inspect
it, but you cannot compare or resume it. To try again, start a new run explicitly. Recorded
diagnostics may contain private provider output; review them before sharing.

Each trial gets a private workspace without the source repository's Git history. The model
cannot access the hidden verifiers. They run afterward in a separate container with no
credentials or network access.

co:op excludes your MCP servers and `COOP_RUN_ARGS` from trials. Those extra mounts or runtime
flags could change the task setup without co:op recording the change. Use co:op-managed
logins for provider credentials.

The results describe the workload you tested. They do not establish equal performance across
providers or a public benchmark ranking.

co:op turns off the native client's web search and fetch tools for every evaluation candidate,
including consultations and delegated work that co:op manages. Terminal, file and editing tools
remain available. co:op records this setting with the run and includes it when checking whether
workloads match.

This setting relies on the client following it. It is not a firewall that stops a model from
making its own authenticated provider requests. Regular co:op runs keep their normal web tools.

Before trials start, co:op saves private copies of the suite's named inputs and hidden
verifiers. The run uses those copies, so later edits to the source cannot change its tasks or
the workload ID used for comparisons.
The saved suite must fit within 2 GiB and 200,000 entries. If it does not, co:op refuses the run
before any provider work begins.
