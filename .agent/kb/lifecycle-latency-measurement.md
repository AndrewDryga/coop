---
name: lifecycle-latency-measurement
description: how to measure a box/editor start or stop change without fooling yourself — pair the two builds and alternate them, never bisect a noisy metric single-shot, build the comparison binary where its version stamp is honest, and know why an old binary cannot run in an approved project
subsystem: testing
sources: [tools/lifecycle_bench.py, tools/test_lifecycle_bench.py, Makefile, internal/cli/acp_cmd.go, internal/cli/cli.go, internal/runtime/docker_lifecycle.go]
updated: 2026-09-20
---

`make lifecycle-bench` (`tools/lifecycle_bench.py`) is the instrument for every start/stop claim in
this repo. It is deliberately OUT of `make check`: it launches real boxes, needs a runtime, and its
output is a measurement to compare against, not a threshold to fail on. Cases:
`repeat_start`, `filtered_start`, `acp_initialize`, `acp_switch_cold`, `acp_switch_warm`,
`normal_stop`, `filtered_stop`, `cancelled_stop`, `failed_start_preflight`,
`failed_start_after_create`. `WORKSPACE=` picks the workspace; a bare one-file git repo is the
control that separates what filtering costs from what the tree costs. What the stop boundary
covers — gateway containers AND volumes, both label families — is pinned by `OwnershipTest` in
`tools/test_lifecycle_bench.py`, because a stop number is only as honest as that question.

## Pair the builds and alternate them; do not subtract a stored baseline

A retained baseline is a reference, not a subtrahend. Build the old revision's binary, measure BOTH
back to back with TODAY's `lifecycle_bench.py` as the instrument, and alternate which arm goes first
between rounds (A B / B A / …). Report the per-round difference, not two pooled medians: drift lands
on both arms in a round and cancels in the difference, and flipping the order keeps warm-up from
masquerading as the result.

This is not theoretical. On 2026-09-20 `acp_initialize` measured 0.598 s on the old build and
0.886 s on the new one — populations that did not overlap — and a naive reading would have blamed
whatever commit happened to be measured next. Six paired rounds (each arm value the median of 3)
confirmed it: +0.231 s, slower in 6 of 6 including the three rounds where the new arm went first
(sign test, two-sided p = 0.031). The same day the old binary reproduced a two-day-old
quiet-machine baseline within 8% on every other bare case, the loaded day slightly FASTER — so the
load on the machine was not what moved the numbers, but a day-to-day effect of that size exists and
is in the direction that flatters an "after" arm. You only know which of those worlds you are in by
pairing.

**Do not `git bisect run` a wall-clock metric with a threshold predicate.** One measurement per
revision cannot separate commit order from clock order, and on this metric they coincide. A real
bisect over `acp_initialize` blamed a commit that only adds a struct field to the session evidence
export, because consecutive revisions shared the value band the threshold sat inside. If you must
bisect, make the predicate itself paired against a fixed reference arm measured in the same round.

**Knob probes overlap.** Turning the warm pool off (`COOP_ACP_WARM=0`) recovered ~0.19 s of that
0.23 s; pointing `COOP_MCP_FILE` at another absent path, pool still on, recovered ~0.21 s. Those sum
to more than the whole, so they are not two causes — they are two ways of skipping one thing that
is not yet named. The old revision already fanned the pool out concurrently, so "the fan-out competes
with the lead" does not explain the difference either. The cause is open.

## Build the comparison binary where its version stamp is honest

A plain `go build` of a tree checked out INSIDE this checkout (a worktree under a task's `tmp/`,
say) came out stamped `v0.0.0-<time>-ef86d383…` — the enclosing checkout's HEAD, not the revision
built. `resolveVersion` (`internal/cli/cli.go`) trusts that stamp when the Makefile's `git describe`
LDFLAGS are absent, so `coop --version` and the bench's `coop_version` name the wrong revision.
Build with `make build` in a worktree OUTSIDE the checkout, and prove which code a binary is by
strings the newer revision introduced (`strings coop | grep -F <new-only-string>`), not by
`--version`.

## An old binary cannot start in an approved project

Rebuilding a pre-batch revision and pointing it at this repo fails with `This box cannot start
because the project directory … was replaced since it was approved`. Approval records gained
reboot-survivability during the same batch, and the old binary does not recognise them. Re-approving
with the old binary would rewrite a record the user's live sessions depend on, so the before-arm in
an approved project is unobtainable at acceptable cost — use the bare control for the A/B and say so.

## Traps that cost more than the numbers

- **Cancellation must go through a real terminal.** Coop runs the client in its own foreground
  process group so the tty delivers `^C`; a script-delivered SIGINT hits coop, not the workload, and
  the case then reports that cancellation never happens.
- **A box occasionally starts with no working directory** — `sh: 1: cannot create
  ./.coop-bench-ready: Directory nonexistent`, or in the pty case just "the box never announced
  itself". Seen only in the bare `/tmp` control during bench runs, on BOTH builds (5/64 and 3/64), and
  0/48 in the project under `$HOME`; but a standalone probe of 12 plain launches in each of a `/tmp`
  and a `$HOME` one-file repo saw 0/24, so it is not the path alone, and the two bench workspaces
  differ in more than their path. Predates this batch. Unexplained.
- **A healthy run is occasionally reported failed** — `Docker attachment ended without a confirmed
  workload outcome` — about 1 sample in 18 in this repo on a loaded host, with nothing left behind.
  One terminal inspect at `internal/runtime/docker_lifecycle.go:245-249` did not confirm an exited
  container; a slow inspect, a `--rm` container already auto-removed, or a not-yet-terminal state
  all print this, and which one it was is not captured.
- **The first `filtered_start` sample in a fresh workspace is setup, not steady state** (22 s vs
  ~2.5 s). Warm with one throwaway sample before a measured run, and warm BOTH arms the same way.
- **A stop number below ~0.04 s is the probe's resolution**, not the product's: two runtime queries
  are the floor on any gone-yet answer.

## A comment is not a measurement

`internal/cli/acp_cmd.go` carried, in two places, the claim that the warm pool's fan-out left
"startup latency unchanged" — from the day the pool was written, never measured, and read as settled
until 2026-09-20's paired A/B put a number against it. Both now state the number.

## Changelog
- 2026-09-20 — created while proving the agent-lifecycle batch
  (task 2026-09-15-prove-agent-lifecycle-speedups-preserve-existing). Recorded the paired method,
  the invalid bisect, the overlapping knob probes, the version-stamp trap, the approved-project
  refusal, and the four intermittent traps. Review corrections taken: the mechanism was withdrawn
  (the old revision fanned out concurrently too), the baseline agreement restated as within 8% with
  the loaded day faster, the inspect fault restated as three possible conditions, the cwd fault
  restated with its real counts and the standalone probe's 0/24.
