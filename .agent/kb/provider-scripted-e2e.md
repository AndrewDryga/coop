---
name: provider-scripted-e2e
description: Drive the external Coop CLI through strict runtime/provider fixtures without ambient state
subsystem: testing
sources: [Makefile, internal/box/run.go, internal/box/run_test.go, internal/testutil/procharness/harness.go, internal/cli/controller.go, internal/cli/fork.go, internal/cli/fork_merge.go, internal/cli/tasklease.go, internal/cli/streamjson.go, internal/cli/telemetry.go, internal/cli/scripted_process_e2e_test.go, internal/cli/direct_process_e2e_test.go, internal/cli/scripted_fork_process_e2e_test.go, internal/cli/scripted_loop_process_e2e_test.go, internal/cli/scripted_loop_recovery_process_e2e_test.go, internal/cli/scripted_consult_process_e2e_test.go, internal/cli/scripted_delegate_process_e2e_test.go, internal/cli/scripted_preset_process_e2e_test.go, internal/cli/testdata/providerfixture/main.go, internal/cli/testdata/providerfixture/loop.go, internal/cli/testdata/providerfixture/delegate.go]
updated: 2026-07-16
---

`make provider-scripted-e2e` builds fresh Coop and fixture executables inside a disposable root,
then drives a direct process matrix for every registered provider. The child environment is built
from an empty allowlist, all mutable/config paths live below that root, and the harness owns the
outer process group and bounded output (`internal/testutil/procharness/harness.go`). The matrix
proves marked-default and explicit
account selection, configured and inline model/effort precedence, exact adapter argv/env, forwarded
arguments, credential stripping, output passthrough, fail-fast validation, nonzero exit propagation,
and ready-synchronized cancellation (`internal/cli/direct_process_e2e_test.go`).

The same target also crosses the full consult boundary: external CLI dispatch, `box.Run`, exact
generated wrapper/persona mounts, scoped homes, native provider argv/output, continuation state,
fallback, and telemetry. It covers every provider arm and all 12 ordered distinct fallback pairs;
see [[provider-consult-e2e]] for the state and live-ring contracts.

Fork coverage uses disposable parent and fork repositories to cross the external CLI/runtime
boundary without a second emulator. It proves fresh, resume, and new sessions; all four native
launch shapes; provider, account, cwd, and explicit-ID isolation; remembered-provider behavior
across `--fresh`; task seeding; confirmation; merge; and parent queue reconciliation. Claude,
Gemini, and Grok use Coop-owned IDs scoped by provider and account. Coop records Codex's native
post-run ID and later requires that exact account/cwd match; a legacy hint under a shared
`COOP_WORKDIR` starts fresh instead of guessing. All Coop-owned interactive Codex producers for
one profile/cwd take the same host-only ConfigDir lock, even across parent repos; contention fails
before provider launch because a TUI can remain open for hours. External Codex processes cannot
participate, so native-ID attribution remains best-effort if one mutates the same profile/cwd
during a fresh run. Merge assertions use only the newly landed commit range so an older reused
task trailer cannot settle unrelated current work.

The registry-derived loop matrix runs the external `coop loop` binary once per provider against a
closed fixture worker. It requires the host to claim and flock-lease the exact task before provider
start, validates the canonical provider/model/account target in lease metadata and native argv,
then requires one task-bound commit, final state/log, done move, host cleanup of task scratch, a
clean worktree, and one exact work-stage telemetry row. The fixture accepts no command, arbitrary
path, or shell fragment; its v5 attempts are only closed semantic lifecycle outcomes. Provider-native
event schemas stay in adapter decoder tests, while `script(1)` gives focused recovery rows the real
terminal boundary for streaming and two-stage Ctrl-C behavior.
Unbound-completion denials also prove host recovery never follows provider-created task metadata
links outside the repository. A finalization failure restores the task to in-progress, and the
state-link case reruns the loop to repair the commit binding, clean scratch, and finish normally.

The recovery matrix drives the real external controller through authentication, provider and output
limits, ordinary errors, exact diagnostic phrases in assistant prose, malformed/truncated streams,
account/model cooling, all 12 directed rotations, all-limited waiting and process-group cancellation, and
interruption/resume. The fixture records exactly one expected provider/target/result per attempt and
never selects the successor. Exact wait arithmetic remains in injected-clock unit tests. Telemetry
asserts a closed outcome per attempt, and completion reconciliation returns both bound and unbound
crash-left work to the normal range-validated resume path after its lease is released. The task-local
lock is compatibility evidence; the authoritative flock and active marker live in a host-only cache
registry, so replacing provider-writable `tmp` cannot create a second owner. The controller stops
heartbeat writes while retaining that flock, then validates and finalizes only its assigned queue root
and task ID. Unit tests own oversized stream and stderr bounds.

The delegate matrix crosses the corresponding write-capable boundary for all four provider arms
and all 12 ordered distinct fallback pairs. It verifies the exact generated wrapper and one invoked
role contract, scoped credential homes, native write-capable argv, depth, fallback, serialization,
process cleanup, and preserved repository evidence. Safety cases cover tracked, staged, ignored,
ref, commit, and commit-reset mutations, dirty baselines, timeout, recursion, unmounted targets, and
a delegate calling its configured read-only consult. The single-role scenario shape is intentional;
preset composition tests own interactions among multiple roles.

Preset/Fusion composition uses four dense registry-derived runs: every lead gets one mounted
consult role on every provider, so the suite launches all 16 lead-provider x role-provider
relationships while starting only four boxes. It proves distinct same-provider lead/role models,
effective effort, exact ordered council labels, personas, wrapper cardinality, credential scope,
and repository immutability. Focused rows cover explicit peers plus multiple roles on one provider,
native-role degradation, terminal first-rung pinning, and missing role authentication. ACP's inner
child has a separate assembly test proving the supervisor-selected preset and concrete target
replace stale launch state; the ACP supervisor matrix remains the owner of migration and rate-limit
rotation.

Declared orchestration artifacts fail closed. A Fusion/preset lead instruction, consult/delegate
wrapper, delegate contract, native-role directory, or nonempty consult persona that cannot be
created returns before the box or provider starts; partial temp artifacts are removed. `internal/box`
injects only these three filesystem operations in tests and leaves unrelated best-effort MCP,
ordinary instruction, git, and skill wiring unchanged.

The fixture is deliberately not a container emulator. It accepts only Coop's tested runtime verbs
and flags, validates every host bind/env-file/workdir before invoking its own explicit provider mode,
and writes a versioned 0600 JSONL trace. Unknown syntax, images, commands, symlinks, and root escapes
fail closed (`internal/cli/testdata/providerfixture/main.go`). Real runtime isolation belongs to
`coop doctor`: CI blocks on Docker and Podman (`.github/workflows/ci.yml`) and separately exercises
the review-write boundary; Apple's runtime still requires a local doctor
run because the hosted matrix cannot cover it.

Scenarios are a closed data contract: bounded plain output, exit status, or wait-for-signal. They
cannot contain commands, shell fragments, unknown fields, or control characters. To extend it, add
scenario data and semantic assertions to the process suite; never branch on a Go test name or
execute the provider tail received from runtime argv. A failure diagnostic includes the
bounded stdout/stderr and trace. Trace host paths are fixture-relative, environment values are
default-redacted, and free-form label/provider values use deterministic digests; the whole trace is
deleted with the test root (`internal/cli/testdata/providerfixture/main.go`,
`internal/cli/scripted_process_e2e_test.go`).

## Changelog
- 2026-07-16 - added fork session isolation and merge lifecycle process coverage
- 2026-07-16 - moved authoritative leases to host-only state and scoped finalization to the assigned task
- 2026-07-16 - added external recovery, rotation, PTY interruption/stream-bound, and telemetry outcome coverage
- 2026-07-16 - made unbound recovery metadata root-bounded and no-follow
- 2026-07-16 - added the complete four-provider loop claim, task, commit, cleanup, and telemetry lifecycle
- 2026-07-16 - added the 16-relationship composition matrix and fail-closed artifact contract
- 2026-07-15 - added the mounted-wrapper delegate matrix and write-safety contract
- 2026-07-15 - added the mounted-wrapper consult matrix and cross-linked its focused contract
- 2026-07-15 - expanded to direct target/account/model/effort, failure, exit, and cancel behavior
- 2026-07-15 - created with the strict all-provider direct-process harness
