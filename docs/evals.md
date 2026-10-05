# Evaluations

`coop eval` runs repeatable tasks and grades the finished work independently, so you can tell
whether a different model, preset, loop recipe or co:op build helped. Keep the suite and trial
matrix the same before and after your change, then compare the recorded results. The
[website guide](https://coop.dryga.com/docs.html#evals) walks through a first run.

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

3. Change one thing and run the same suite again. Use `coop eval runs` to find the two run IDs,
   then compare them with `coop eval compare <before-id> <after-id>`. The argument order decides
   which run is before and which is after; co:op doesn't infer it from timestamps. Inspect and
   compare show each configuration's short fingerprint and co:op build, so you can see when a
   preset or recipe kept its name but was edited.

Three flags set a run's time limit, repeat count and parallel trials. `coop help eval run` lists
every option.

| Flag | What it does |
|---|---|
| `--timeout 35m` | Sets a time limit for the whole command, starting at command entry. At the limit, preparation, trials and grading stop. Cleanup and record sealing may finish afterward. It limits time only; it doesn't cap what you spend. |
| `--repeat 3` | Runs each case three times. |
| `--jobs 2` | Allows two agent trials at once. |

Each case also has its own timeout. More configurations or repeats mean more paid work.

`cloc` is optional. Install it for change-size figures. Grading and verdicts still work without it,
and co:op labels the missing measurement instead of reporting a false zero.

## Choose what to evaluate

- `core` has three small single-agent coding tasks with independent checks and deliberate near
  misses. Use provider targets with it, such as `codex:gpt-6.1-sol/xhigh`, not presets.
- `queue` has ten tasks that the real `coop loop` works, including review and signoff. Use it to
  compare presets or loop recipes. Each trial works its tasks in sequence and can take hours. For
  example, `coop eval run queue frontier --timeout 2h` uses your existing `frontier` preset. Add
  `--loop-config .agent/loop.yaml` to evaluate your project's recipe.
- `coop eval init ./evals/my-suite` creates your own suite from a working greeting example. Edit
  `suite.yaml`, `files/greeting/` and the separate `verifiers/greeting/verify.sh`. The verifier's
  README explains grading, and the manifest includes a commented loop example.
- The source tree's [`examples/evals/maintenance/`](../examples/evals/maintenance/) suite has three
  independent shell, Python and Node maintenance loops with offline baseline and reference
  controls. It's a custom comparison example. It isn't an official benchmark score or a shipped
  `coop eval ls` starter.

Each target or preset after the suite is a separate configuration, not a fallback. This command
previews both configurations:

```sh
coop eval run core codex:gpt-6.1-sol codex:gpt-6.1-sol/xhigh --timeout 70m --dry-run
```

## Runtime profiles for custom agent cases

An agent case can use a trusted runtime profile: an explicitly built, dedicated tool image instead
of the shared image. Put these four fields under the case:

```yaml
runtime:
  profile: /absolute/path/to/clean-profile
  workdir: /app
  agent_timeout: 15m
  verifier_timeout: 15m
```

Give the case's `timeout` enough time for both phases and preparation. Loop suites don't support
profiles.

The profile must live outside the suite and co:op's eval state, with no Git metadata, symlinks or
special files. Its project Dockerfile defaults to `.agent/Dockerfile` and must inherit co:op's
client image. A profile can't add project environment, services, published ports or extra network
grants.

Review the profile's inputs, then explicitly run `coop build --egress filtered` from the profile
directory before the eval. The build uses ordinary host networking, not the candidate's filtered
network.

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

This is a disclosed adaptation, not official benchmark parity. Exclude cases that depend on disk
quota or exhaustion.

co:op retains the profile bytes privately. Before work, it records their digest, the measured image
and platform, and the phase budgets. The retained copy never grants build approval: the original
profile must stay unchanged and available for both phases. Rebuilding a different image after
preparation also fails the candidate launch. Profile bytes share the suite's 2 GiB / 200,000-entry
staging limit. A dry run doesn't resolve the image, so its workload fingerprint is provisional.

## Understand a result

`coop eval inspect` reads the latest run; add a run ID to inspect an older one. It shows the
recorded causes for trials that didn't pass, and the directory with the JSON records and retained
work. `coop help eval inspect` explains what results mean.

| Result | Meaning |
|---|---|
| `failed` | The independent verifier rejected the work. |
| `error` | Execution or grading didn't produce a verdict, so grading is incomplete. It doesn't mean the model failed the task. |
| A timeout, or a trial that never started | Grading is incomplete here too. |

An incomplete comparison has no definitive winner. A sealed run with complete grading exits 0, even
with graded failures. A run with incomplete grading exits 1 and keeps its record for inspection. A
dry run exits 0 after previewing without trials. Code-size changes are a review signal, not a
quality score.

`coop eval compare` rebuilds each run's full requested matrix from its manifest. Missing records
stay pending, and `compare` rejects duplicate or out-of-matrix records as damaged evidence.

Paired effects require two distinct runs with the same nonempty workload identity, case set and
repeat count, and one configuration per run. Other runs keep their separate summaries; co:op
doesn't guess which configurations correspond. Each case's change needs all its repeats graded.
The aggregate change and the uncertainty bound need every requested pair graded.

The report separates case count from repeats. It shows the pass-rate change in percentage points
(after minus before). Its conservative 95% Hoeffding bound measures repeat uncertainty on this
fixed suite, assuming independent trial executions: `sqrt(log(40)/(cases*repeats))`, clipped to
the possible −100 to +100 percentage-point range.

More repeats don't add independent tasks, and they don't establish performance on unseen tasks. A
decision campaign needs at least three repeats per case, plus a preregistered useful-effect
threshold and decision rule. Retained runs don't record that rule, so this descriptive report stays
inconclusive and doesn't announce a winner.

co:op measures the finished snapshot before the verifier runs. For a trial that didn't pass, it
keeps the candidate's original workspace for inspection and removes the writable grading copy. It
doesn't count verifier build artifacts or present them as model work. The grader still sees only a
sanitized snapshot, so the trial detail notes any skipped external links or special files.

For a provider sign-in or quota refusal, check `coop credentials <agent>` before another paid run.
For timeouts, check both the case timeout in `suite.yaml` and the run's `--timeout`. A run without
a final summary may still be running, or it may have been interrupted. You can inspect it, but you
can't compare or resume it; any rerun is an explicit new invocation. Recorded diagnostics can
contain private provider output, so review them before you share them.

Trials use private workspaces with no source Git history. Hidden verifiers stay outside model
mounts and run afterwards in a separate container with no credentials and no network. co:op leaves
operator MCP servers and `COOP_RUN_ARGS` out of trials: ambient runtime mounts or flags would change
the workload without appearing in its fingerprint. Use co:op-managed logins for provider
credentials. These checks are evidence about the tested workload. They don't promise general
provider parity or a public benchmark ranking.

Every eval candidate runs with normal native web search and fetch turned off, including the
consults and delegates co:op manages. Terminal, file and editing tools stay available. The run and
workload identity record this policy. It's a trusted-client control, not a firewall against a model
crafting its own authenticated provider requests. Regular co:op runs keep their normal web tools.

Before trials, co:op freezes the suite's named inputs and hidden verifiers under private run state,
so later source edits can't change that run's work or comparison identity. The whole frozen suite
must fit within 2 GiB and 200,000 entries, or co:op refuses the run before provider work.
