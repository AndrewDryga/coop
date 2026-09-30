# Maintenance loop comparison suite

This is a source-tree example, not a shipped starter or an official benchmark. It has three independent complete-loop cases: a POSIX-shell service-config writer, a Python CSV/SQLite ledger, and a Node HTTP file server. Each case starts from its own small repository and three ordinary queued tasks. One entire loop run is one trial; task folders and reviewer passes are not extra samples. The final verifiers check behavior in the accumulated workspace, not where tasks ended up in the queue.

`fixtures/` and `queues/` are candidate inputs. `verifiers/` contains hidden checks and reference completions; never copy it into a candidate workspace or image. Coop's existing staging and grader paths put each verifier outside candidate mounts and run grading afterward in a separate offline, credential-free box. Do not use these files with a homegrown runner that lacks that boundary.

## Unpaid qualification

From the repository root:

```sh
sh examples/evals/maintenance/qualification.sh
sh examples/evals/maintenance/runtime-qualification.sh <immutable-coop-image-id>
coop eval run ./examples/evals/maintenance/suite.yaml codex --timeout 3h --dry-run
```

The first command expects three unfinished baselines to fail, three reference completions to pass, and eight missing-step/collateral mutants to fail. The second repeats baseline/reference verdicts with no network, no credentials, a read-only image root, resource limits, and a read-only verifier mount. It tests this image's interpreters and graders, not a provider or Coop's full loop. The dry run validates the real suite loader and freezes input/verifier bytes without spending credits; it does not prove runtime readiness or model quality. Existing eval staging, cancellation and cleanup tests cover the shared harness; these controls cover the scenario-specific behavior.

Record the exact source commit, workload fingerprint printed by the dry run, Coop executable/build digest, image ID and architecture, Python/Node versions, target/preset/account identities, billing mode, case and total timeouts, trial count and concurrency before a comparison. Use included subscription allowance where available; additional billed usage needs an authorized spend cap. Use an immutable image ID through `COOP_IMAGE`, not a mutable tag. A source or verifier edit creates a new suite version and requires all configurations to be rerun. The shell, Python and Node fixtures use only tools included in Coop's base image; no package download or live service is needed for grading.

No model trial or winner is implied by the offline controls. A decision campaign needs the same frozen suite and budgets for both configurations, at least three independent repeats of each whole scenario per configuration, complete matched coverage, and a preregistered useful-effect threshold. Report every timeout, setup/grader/cleanup error and incomplete trial separately from graded failures. If uncertainty or coverage does not support a direction, report inconclusive.
