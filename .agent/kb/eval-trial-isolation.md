---
name: eval-trial-isolation
description: What isolates one coop eval trial from the next and from the grader — and why the obvious credential fix would break authentication
subsystem: eval
sources: [internal/cli/eval_trial.go, internal/cli/eval_grade.go, internal/cli/eval_loop.go, internal/eval/workspace.go, internal/eval/snapshot.go, internal/box/run.go, internal/box/mounts.go, internal/agent/codex.go]
updated: 2026-09-22
---

`coop eval` measures configurations against each other, so its whole value rests on two trials
differing ONLY in the configuration. The isolation is spread across several mechanisms; this card is
the map, and the reasoning behind the one place the obvious fix is wrong.

## What is isolated, and by what

- **Workspace** — `eval.PrepareWorkspace` copies the suite's fixture into a private tree with a
  synthetic initial commit and no `.git` from the source, so no author history travels
  (`internal/eval/workspace.go`).
- **Hidden material** — the verifier never appears in any candidate mount. The suite loader refuses a
  manifest whose verifier overlaps `files`/`fixture`/`tasks` (`internal/eval/suite.go`), and grading
  mounts it at `/coop-verifier`, outside the workspace (`internal/cli/eval_grade.go`).
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
- **MCP** — a trial runs with `MCPFile` cleared. Operator MCP servers are a route out of the trial and
  differ per machine, so a run using them would not be reproducible.
- **Loop trials** — a subprocess with a PINNED environment, not an inherited one. `coop loop` resolves
  its repository from configuration rather than its working directory, so an inherited `COOP_REPO`
  would run the loop against the operator's own checkout, with their credentials, making real commits
  (`internal/cli/eval_loop.go`).

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

A loop stopped on a human decision (exit 3) bypasses this shortcut: its only work may be a task
move and decision under `.agent/tasks`, which signatures intentionally ignore. The grading snapshot
includes that queue state, and the independent verifier still decides pass or fail. An unchanged
agent exiting 3 is not a blocked loop; ordinary failed attempts and startup/interruption errors
retain their existing ungraded outcome.

## Sizing a loop scenario, and one trap

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
- 2026-09-22 — exempted recognized blocked-loop outcomes from the no-work shortcut. Integrated
  subprocess tests prove queue-only blocking reaches both accepting and rejecting graders, while
  unchanged failed agents/loops, startup refusal and interruption stay ungraded.
- 2026-09-21 — replaced path/size-only no-work detection with bounded content and metadata
  comparison. Regression proves a same-length edit after exit 1 reaches the grader; unchanged
  attempts still take the no-work route, and unreadable inputs never produce a usable signature.
- 2026-09-20 — created while landing `coop eval` execution, grading and loop scenarios.
- 2026-09-21 — added loop-scenario sizing (43 min for ten tasks) and the signoff-reopening trap,
  from the full-length qualification run of the `queue` starter.
