# Maintenance loop comparison suite

This suite is an example in the source tree, not a shipped starter or an official benchmark.

It has three independent cases, and each one runs a complete loop:

- a POSIX-shell service-config writer
- a Python CSV/SQLite ledger
- a Node HTTP file server

Each case starts from its own small repository and three ordinary queued tasks. One entire
loop run is one trial, so task folders and reviewer passes don't count as extra samples. The
final verifiers check behavior in the finished workspace, not where the tasks ended up in the
queue.

`fixtures/` and `queues/` are inputs for the candidate, the agent being evaluated. `verifiers/`
holds the hidden checks and the reference completions. Never copy it into a candidate's workspace
or image. co:op keeps each verifier outside the candidate's mounts when it copies the suite and
when it grades. Grading runs afterward, in a separate box that is offline and has no credentials.
Don't use these files with a homegrown runner that lacks that separation.

## Unpaid qualification

From the repository root:

```sh
sh examples/evals/maintenance/qualification.sh
sh examples/evals/maintenance/runtime-qualification.sh <immutable-coop-image-id>
coop eval run ./examples/evals/maintenance/suite.yaml codex --timeout 3h --dry-run
```

`qualification.sh` expects three unfinished baselines to fail, three reference completions to
pass and eight mutants to fail. A mutant is a reference completion with one deliberate fault: a
missing step, or damage to behavior that should stay the same.

`runtime-qualification.sh` repeats the baseline and reference verdicts in the image you give it,
with no network, no credentials, a read-only image root, resource limits and a read-only verifier
mount. It tests this image's interpreters and graders, not a provider or co:op's full loop.

The dry run uses the real suite loader and copies the exact input and verifier files. It spends
no credits, and it doesn't prove runtime readiness or model quality.

## Before a comparison

Record these first:

- the exact source commit
- the workload fingerprint the dry run prints
- the build digest of the `coop` executable
- the image ID and architecture
- the Python and Node versions
- the target, preset and account you use
- the billing mode
- the case and total timeouts
- the number of trials and how many run at once

If you have a subscription, stay within its limits. Any extra billed usage needs an authorized
spending cap. Pass an immutable image ID through `COOP_IMAGE`, not a tag that can change.

Any edit to a source file or verifier creates a new suite version, and every configuration
must run again. The shell, Python and Node fixtures use only tools included in co:op's base
image. Grading needs no package download and no live service.

## Deciding between configurations

The offline checks aren't a model trial, and they don't pick a winner. To decide between
configurations, you need:

- the same unchanged suite and budgets for both configurations
- at least three independent repeats of each whole scenario per configuration
- a pass or fail grade for every matched trial in both configurations
- a threshold for how much improvement is useful, written down before the runs start

Report every timeout, every setup, grader or cleanup error and every incomplete trial
separately from the graded failures. If the uncertainty or the coverage doesn't show which
configuration is better, report the result as inconclusive.
