---
name: fork-lifecycle-test-barriers
description: lifecycle race tests must observe lock entry after unlocked preflight, not infer it from a delay
subsystem: tests
sources: [internal/cli/fork_cmd_test.go, internal/cli/fork_cmd.go, internal/forkctl/testhelpers_test.go, internal/forkspace/state.go]
updated: 2026-09-05
---

`forkCreate --fresh` performs pinning, Git/lifecycle checks, confirmation, image resolution and
orphan sweeping before entering the lifecycle lock. A test that holds that lock and sleeps before
mutating the workspace has not proved the command captured its original state: under load, the
mutation can precede preflight and the test exercises a different race.

The CLI's `runForkCommandAcrossLockedMutation` waits for `LockStateContext` in the command
goroutine's stack before applying the mutation. An unrelated lock waiter does not count. The
existing failure deadlines remain bounded and dump stacks; elapsed time is not an ordering signal.
Keep callers nonparallel: the stack identifies this helper's command, not a unique invocation.
The virtual-time `TestForkLifecycleMutationWaitsForCommandPreflight` models slow preflight and an
unrelated waiter without adding wall-clock sleeps or production-only synchronization hooks.
The forkctl test helper already observes lock entry instead of sleeping; its stack match is broader.

This only proves the test's mutation ordering. A timeout after unlock or during runtime reaping
still needs its own diagnosis; a passing repetition does not establish why an earlier run stalled.

## Changelog
- 2026-09-05 — reproduced the CLI's 80 ms preflight assumption with virtual time and replaced it
  with the command-stack barrier. Kept production and test deadlines unchanged and separated
  this confirmed harness flaw from the release gate's unproven runtime-timeout cause.
