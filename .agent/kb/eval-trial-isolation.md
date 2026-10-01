---
name: eval-trial-isolation
description: What isolates one coop eval trial from the next and from the grader — and why the obvious credential fix would break authentication
subsystem: eval
sources: [internal/cli/eval_cmd.go, internal/cli/eval_trial.go, internal/cli/eval_grade.go, internal/cli/eval_runtime_profile.go, internal/cli/eval_loop.go, internal/cli/eval_results.go, internal/loop/loop.go, internal/eval/compare.go, internal/eval/execute.go, internal/eval/fixtures.go, internal/eval/suite.go, internal/eval/stage.go, internal/eval/stage_copy.go, internal/eval/runtime_profile.go, internal/eval/stage_profile.go, internal/eval/workspace.go, internal/eval/snapshot.go, internal/eval/store.go, internal/eval/catalog.go, internal/eval/private_root.go, internal/runtime/runtime.go, internal/box/run.go, internal/box/mounts.go, internal/box/authority_mounts.go, internal/agent/codex.go, examples/evals/maintenance/suite.yaml, examples/evals/maintenance/qualification.sh, examples/evals/maintenance/runtime-qualification.sh]
updated: 2026-10-01
---

`coop eval` measures configurations against each other, so its whole value rests on two trials
differing ONLY in the configuration. The isolation is spread across several mechanisms; this card is
the map, and the reasoning behind the one place the obvious fix is wrong.

## What is isolated, and by what

- **Frozen workload** — `eval.StageSuite` first copies only named candidate inputs and hidden
  verifiers into owner-private run inputs through confined directory handles. It checks physical
  input/verifier separation, binds the source root to the loaded manifest, bounds aggregate staged
  bytes and entries, and fingerprints the copied trees. Nested submodule `.git` metadata is
  omitted while its working files remain; a selected tree rooted in `.git` is refused.
  Every trial uses those retained bytes, not a live suite source (`internal/eval/stage.go`,
  `internal/eval/stage_copy.go`, `internal/cli/eval_cmd.go`).
  The instruction field uses YAML flow style to quote multiline text. yaml.v3 block scalars
  can misalign indentation or drop a leading newline during encoding. Frozen instruction
  bytes stay unchanged; trimming them is not a fix.
- **Workspace** — `eval.PrepareWorkspace` copies a frozen case input into a private tree with a
  synthetic initial commit and no `.git` from the source, so no author history travels
  (`internal/eval/workspace.go`). Git uses a fresh HOME/XDG, so the operator's global ignore file
  cannot change that commit. An agent case without `files` starts empty, never from the suite
  directory (`internal/cli/eval_trial.go`).
- **Host mount boundary** — those generated workspaces live under Coop's private state. Box admits
  only the exact owner-private trial workspace for a candidate, and only its snapshot and selected
  read-only retained verifier for a credential-free grader. Sibling records, other trials and
  credentials remain fenced (`internal/box/authority_mounts.go`). Starter extraction and run
  creation both tighten the eval root to 0700; older roots created as 0755 are migrated before use.
- **Hidden material** — the verifier never appears in any candidate mount. The suite loader refuses
  lexical overlap across all cases, and staging checks physical aliases before copying. Grading
  alone receives the exact retained verifier at `/coop-verifier` through a read-only private-state
  mount; a candidate still cannot mount it (`internal/eval/suite.go`, `internal/eval/stage.go`,
  `internal/box/authority_mounts.go`, `internal/cli/eval_grade.go`).
- **Grader** — a fresh container from the trusted image, `Homes:false` (no model credentials), a
  config CLONE with `SetEgress("none")`, and `PolicyRepo` pointed at an EMPTY directory. That last
  one matters: a box reads `.agent/project.yaml` from the repo it mounts, so without it a candidate
  could ship a policy in its own workspace that re-opens the grader's network and rewrites its PATH.
- **Secret shadowing is OFF for the grader** (`RunSpec.GradeSnapshot`). Shadowing protects a MODEL
  from a credential someone left in a repo; a grader is trusted code that must see what the candidate
  actually wrote, or any case whose answer is a `.env`/key/cert file fails for no reason. The rail:
  `box.Run` refuses `GradeSnapshot` together with `Homes`.
- **Per-run config** — every trial gets `cfg.Clone()`. A plain `*cfg` is NOT enough: Config's per-run
  maps (explicit keys, per-agent profile/model/effort) are shared by a shallow copy, so one trial's
  selection would leak into the next and concurrent workers would race.
- **Image** — the trial selects the operator's `COOP_IMAGE` or the resolved shared base, not a
  project image from the checkout in which `coop eval` was invoked (`internal/cli/eval_cmd.go`).
- **Explicit agent profiles** — optional four-field runtimes freeze clean external build inputs,
  original canonical inode authority, immutable approved image/platform and phase budgets before
  records. The fixed protocol uses `/app`, 1 CPU, 2 GiB and 128 PIDs; declared 10 GiB storage stays
  unenforced/unmeasured and is disclosed in previews, inspection and comparisons. Original profile
  content/identity is checked before both phases; retained copies never authorize builds. Candidates
  use only selected-provider core filtered grants and the original `PolicyRepo`; grading uses the
  same immutable image but empty policy and offline execution. Phase deadlines inherit the command
  deadline. Ordinary suites retain their defaults (`internal/cli/eval_runtime_profile.go`,
  `internal/eval/stage_profile.go`, `internal/cli/eval_trial.go`, `internal/cli/eval_grade.go`).
- **MCP** — a trial runs with `MCPFile` cleared. Operator MCP servers are a route out of the trial and
  differ per machine, so a run using them would not be reproducible.
- **Native web tools** — eval config clones disable ordinary search/fetch through each adapter's
  native controls. The loop child pins the internal flag, and boxes carry it to Coop-managed fresh,
  resumed and delegated roles. Gemini merges exclusions into its existing generated settings, so
  effort overlays retain them. The policy is recorded in manifests and workload v3 fingerprints.
  This is a trusted-client control, not adversarial authenticated-request enforcement.
- **Ambient runtime args** — candidate and grader config clones clear `ExtraRunArgs`, and loop
  children pin `COOP_RUN_ARGS=`. Operator binds can expose host data or hidden verifiers to a
  candidate; even harmless flags would alter a trial without appearing in its fingerprint.
- **Loop trials** — a subprocess with a PINNED environment, not an inherited one. `coop loop` resolves
  its repository from configuration rather than its working directory, so an inherited `COOP_REPO`
  would run the loop against the operator's own checkout, with their credentials, making real commits
  (`internal/cli/eval_loop.go`). A small guardian pins its process-group ID until the final signal:
  a canceled loop gets cleanup time after TERM, but a surviving descendant cannot keep its output
  pipe and trial slot open indefinitely. The final group KILL occurs before releasing that ID.

## The credential home is SHARED, and must stay that way

Every trial mounts the same host credential directory read-write
(`~/.config/coop/agents/<agent>/profiles/<name>`). The obvious hardening — give each trial its own
copy — is WRONG and must not be implemented: these providers issue single-use refresh tokens, so N
copies of a credential store means the first refresh invalidates the rest. This is the same lesson
that produced the container-local `CODEX_SQLITE_HOME` instead of a split home.

The contamination that WOULD matter is already prevented by other means:

- the agent's instruction file (`CLAUDE.md`, `AGENTS.md`) is mounted `:ro` over the home
  (`appendROMounts(args, instructionMounts)`, `internal/box/run.go`), so a trial cannot write what
  the next trial is told — pinned by `TestTrialAttemptCannotRewriteTheNextTrialsInstructions`;
- the agent's config (`config.toml`, `settings.json`) is a `:ro` MCP mount;
- codex's session state is container-local via `CODEX_SQLITE_HOME`;
- no shared cache volume: eval sets `Cache:false` on both the attempt and the grader.

What remains shared is the credential material itself plus whatever new files a candidate chooses to
write into that directory. Before "fixing" that, read this section again: the fix that looks right
breaks authentication for every provider at once.

## Distinguishing no work from an unsuccessful attempt

After a nonzero candidate exit, the no-work shortcut compares two successful workspace signatures.
The signature includes paths, modes, regular-file bytes and literal symlink targets, excluding Git
and harness bookkeeping. Same-length edits and executable-bit or link changes still reach grading.
Reads are bounded by `SnapshotLimit`; unreadable or oversized content makes comparison unavailable,
so it cannot claim the candidate changed nothing. Links are not followed and special files are not
opened. The normal snapshot and verifier decide the outcome when comparison is unavailable.

A failed loop worker is not a loop usage error or human-blocked outcome. Exhausted work retries,
authentication refusal and exhausted rate/output retries return loop failure (1); the original worker
status remains in attempt telemetry. A custom `make loop-iter` can exit 2 after changing source, and
that attempted work must reach grading rather than be mistaken for startup refusal.

A loop stopped on a human decision bypasses this shortcut: its only work may be a task move and
decision under `.agent/tasks`, which signatures intentionally ignore. Exit 3 alone is insufficient:
the queue must agree with the subprocess status. The exception requires a readable, confined queue
with blocked tasks and no actionable work; linked `.agent`/queue/state/task paths are not evidence.
The grading snapshot includes the queue, and the independent verifier still decides pass or fail.
An unchanged agent exiting 3 is not a blocked loop; ordinary failed attempts and startup/interruption
errors retain their existing ungraded outcome. Source changes still reach grading independently.

## Reading retained results

The candidate's finished workspace is copied into a bounded, sanitized grading snapshot. Size
is measured before the verifier can write into that copy. For a non-pass, the original candidate
workspace is retained and the writable grading copy removed; it may include links or special
files omitted from the grader's snapshot, which the trial detail records. Never mount the raw
retained workspace for grading or present verifier build artifacts as candidate work.

The private eval state root contains both runs and a `starters/` cache. A run has a `run.json`
manifest; directory existence alone is not a run identity. Missing summaries mean running OR
interrupted, not proof that the process stopped. `coop eval inspect [<run-id>]` reads the newest
or named record and keeps absent trials in the requested denominator. Provider/verifier detail is
untrusted recorded evidence: bound and escape it for terminal display, never treat it as authority
or automatically retry a paid run. Errors before grading are not model-quality failures.

`compare` uses the sealed summary only as a completion marker. It validates records against the
manifest and fills absent requested trials as pending before counting. Matched effects require
distinct, single-configuration runs with equal nonempty workload identity, case set and repeats;
only a fully graded case gets a delta, and only full matched coverage gets an aggregate/bound.
The Hoeffding bound describes independent repeat executions on the fixed suite, not new tasks.
No campaign decision rule is recorded, so the report is descriptive and declares no winner.

`coop eval run --timeout` has one deadline from command entry through preparation, trials and
grading. Staging and runtime probes honor it; the executor does not grant a new full budget after
preparation. A timeout before run creation leaves no run record. Once a run exists, unstarted
trials remain pending in the full requested denominator and the summary is sealed as incomplete;
cleanup and sealing are best effort after the deadline.

## Sizing a loop scenario, and one trap

`examples/evals/maintenance` provides three distinct shell/Python/Node scenarios using the same
fixture/queue/verifier boundary. Its hidden reference completions include visible regression tests.
Local and offline-image controls reject unfinished baselines and missing-step/collateral mutants;
they establish grader behavior, while complete provider loops and paired comparisons still need
their own recorded qualification. Each control variant gets a separate workspace before mounting.

Measured on the shipped `queue` starter with `codex`: ten small tasks drained in **43 minutes**
(00:08:04 → 00:51:25) inside its 90m case budget. Budget generously — a trial that runs out of time
is recorded as a TIMEOUT, which is honest but useless for comparison, and a slower configuration
hitting the wall would otherwise read as a weaker one.

The trap: the queue does NOT move monotonically into `99_done`. Part way through that run, all ten
tasks sat in `10_in_progress` with `99_done` EMPTY — the loop's own signoff reopening work — and the
trial still ended in a clean pass. So a loop verifier must count BEHAVIOUR, never folder positions: a
folder-counting grader would have scored that same workspace as zero while the tool under test was
already fully correct. The shipped verifier runs each subcommand on inputs no task mentions, which is
also why it cannot be satisfied by a loop that moves folders without finishing anything.

## Changelog
- 2026-10-01 — actual query-optimize preparation exposed the pinned YAML block emitter's
  invalid indentation and leading-newline loss. A field-level flow tag avoids the block
  path; Load→StageSuite→Load regression checks exact indented multiline instruction bytes.
- 2026-10-01 — applied the user's ordinary search/fetch-disable decision. Scoped argv, executable
  role-shell, settings/effort, clone and loop-child controls pass; regular commands stay unchanged.
- 2026-10-01 — mapped external-profile retention/approval/image binding and fixed adapted protocol.
  Six credential-free reference/empty/mutant controls pass through the actual trial runner; this
  proves orchestration and grader discrimination, not provider-mediated retrieval or full admission.
- 2026-09-30 — mapped manifest-backed comparison coverage, pairing refusal and fixed-suite repeat
  uncertainty; incomplete records and multiple configurations cannot produce a matched score.
- 2026-09-30 — mapped the three maintenance loop examples and their independent offline controls;
  retained the distinction between reference/grader evidence and full provider-loop results.
- 2026-09-30 — traced eval-loop cancellation through Go's wait/pipe behavior and a red-before
  TERM-resistant child fixture. The trial now pins group identity through TERM, bounded cleanup,
  and final KILL; focused deadline tests passed 20 times under CPU load.
- 2026-09-30 — rechecked the trial image selector: it no longer calls `ImageForRepo` with an empty
  path, which had allowed the caller's Dockerfile to select a project image.
- 2026-09-30 — rechecked staging, runtime preflight, trial and record paths; documented the
  single command deadline and its pending-record/cleanup boundary.
- 2026-09-29 — rechecked trial copy, measurement and non-pass cleanup; the size and retained
  workspace now describe candidate work before the verifier's writable sandbox changes it.
- 2026-09-28 — rechecked workspace preparation and its staged caller. Replaced a vacuous
  fixture-local ignore test with a poisoned host HOME/XDG test that first proves the ignore is
  active; removed two fresh-init-only assertions. Kept the deliberate nested `.git` omission so
  submodule working files survive, while a top-level live checkout remains refused.
- 2026-09-28 — rechecked the current staging and mount paths while removing an obsolete starter
  verifier grant. Staging now drops special mode bits consistently from its digest and copy, skips
  nested Git metadata like workspace preparation, and keeps a selected tree's top-level `.git`
  refusal. Focused staging and mount tests cover the boundaries.
- 2026-09-28 — traced content-identity drift and two hidden-material exposures: no-files cases
  copied the whole suite, and another case's verifier could sit inside an input (including a
  case-folded alias). Frozen run inputs now use confined handles, physical separation and a bounded
  copy; the grader-only mount allowance follows the new retained verifier path.
- 2026-09-25 — a real starter run reached Box but inherited the operator's private MCP bind from
  `COOP_RUN_ARGS` and returned three prelaunch errors. Traced candidate, grader and loop inheritance;
  Eval now omits ambient runtime args on all three paths while ordinary Coop runs retain them.
- 2026-09-25 — documented the exact generated-state mount admission after real `core` runs
  failed before provider launch; the grader and candidate grants stay separate. Starter extraction
  had created 0755 roots, so creation now tightens both new and existing roots before use.
- 2026-09-22 — documented manifest-backed discovery, unsealed-state ambiguity and explicit local
  diagnostic inspection after a cache appeared as a run and provider quota refusals were opaque.
- 2026-09-22 — separated terminal worker failure status from loop status, retaining raw attempt
  telemetry. Controller regressions cover ordinary/auth/rate/output failures; real offline Docker
  make reproduction confirms that raw exit 2 after source edits was misclassified as a startup refusal.
- 2026-09-22 — required actual blocked-only queue evidence for exit3, which a custom command may
  also return on failure. Integrated regressions cover raw exit3, actionable/missing/empty/done/unsafe
  queues, unchanged genuine blocking and changed source; linked ancestor/queue paths are rejected.
- 2026-09-22 — exempted recognized blocked-loop outcomes from the no-work shortcut. Integrated
  subprocess tests prove queue-only blocking reaches both accepting and rejecting graders, while
  unchanged failed agents/loops, startup refusal and interruption stay ungraded.
- 2026-09-21 — replaced path/size-only no-work detection with bounded content and metadata
  comparison. Regression proves a same-length edit after exit 1 reaches the grader; unchanged
  attempts still take the no-work route, and unreadable inputs never produce a usable signature.
- 2026-09-21 — added loop-scenario sizing (43 min for ten tasks) and the signoff-reopening trap,
  from the full-length qualification run of the `queue` starter.
- 2026-09-20 — created while landing `coop eval` execution, grading and loop scenarios.
