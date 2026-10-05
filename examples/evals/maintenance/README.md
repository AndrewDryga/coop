# Maintenance loop comparison suite

This suite is an example in the source tree. It isn't a shipped starter or an official
benchmark.

It has three independent cases, and each one runs a complete loop:

- a POSIX-shell service-config writer
- a Python CSV/SQLite ledger
- a Node HTTP file server

Each case starts from its own small repository and three ordinary queued tasks. One entire
loop run is one trial. Task folders and reviewer passes don't count as extra samples. The
final verifiers check behavior in the accumulated workspace. They don't check where the
tasks ended up in the queue.

`fixtures/` and `queues/` are candidate inputs. `verifiers/` holds the hidden checks and the
reference completions. Never copy it into a candidate workspace or image. co:op's existing
staging and grader paths put each verifier outside the candidate mounts. Grading runs
afterward, in a separate box that is offline and has no credentials. Don't use these files
with a homegrown runner that lacks that boundary.

## Unpaid qualification

From the repository root:

```sh
sh examples/evals/maintenance/qualification.sh
sh examples/evals/maintenance/runtime-qualification.sh <immutable-coop-image-id>
coop eval run ./examples/evals/maintenance/suite.yaml codex --timeout 3h --dry-run
```

`qualification.sh` expects three unfinished baselines to fail, three reference completions
to pass and eight missing-step or collateral mutants to fail.

`runtime-qualification.sh` repeats the baseline and reference verdicts with no network, no
credentials, a read-only image root, resource limits and a read-only verifier mount. It
tests this image's interpreters and graders. It doesn't test a provider or co:op's full
loop.

The dry run runs the real suite loader and freezes the input and verifier bytes. It spends
no credits. It doesn't prove runtime readiness or model quality.

co:op's existing eval tests for staging, cancellation and cleanup cover the shared harness.
These controls cover the behavior specific to these scenarios.

## Before a comparison

Record these first:

- the exact source commit
- the workload fingerprint the dry run prints
- the build digest of the `coop` executable
- the image ID and architecture
- the Python and Node versions
- the target, preset and account identities
- the billing mode
- the case and total timeouts
- the trial count and concurrency

Use the allowance included in a subscription where you have one. Additional billed usage
needs an authorized spend cap. Pass an immutable image ID through `COOP_IMAGE`. Don't use a
mutable tag.

Any edit to a source file or verifier creates a new suite version, and every configuration
must run again. The shell, Python and Node fixtures use only tools included in co:op's base
image. Grading needs no package download and no live service.

## Deciding between configurations

The offline controls aren't a model trial, and they don't imply a winner. A decision
campaign needs:

- the same frozen suite and budgets for both configurations
- at least three independent repeats of each whole scenario per configuration
- complete matched coverage
- a preregistered useful-effect threshold

Report every timeout, every setup, grader or cleanup error and every incomplete trial
separately from the graded failures. If the uncertainty or the coverage doesn't support a
direction, report the result as inconclusive.
