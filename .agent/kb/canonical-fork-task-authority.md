---
name: canonical-fork-task-authority
description: the project owns one Markdown task; a fork owns only an exact-generation execution projection and reviewed candidate
subsystem: tasks
sources: [internal/tasks/identity.go, internal/tasks/landed_fork.go, internal/tasks/owner.go, internal/tasks/assignment.go, internal/tasks/assignment_registry.go, internal/tasks/projection.go, internal/tasks/candidate.go, internal/tasks/snapshot.go, internal/tasks/channel.go, internal/tasks/cmd.go, internal/tasks/queue.go, internal/tasks/watch.go, internal/forkspace/generation.go, internal/forkspace/execution.go, internal/forkctl/land.go, internal/forkctl/merge.go, internal/cli/fork_cmd.go, internal/loop/prompts.go]
updated: 2026-11-06
---

The canonical queue is the task database. Fork work never creates a second authority: the host
claims one exact `TaskInstance` (random queue ID + task ID, with the folder inode as an additional
replacement signal and the reboot-renumbered device diagnostic) for one immutable,
hardlink-anchored `forkspace.Identity`, moves the canonical folder to in-progress, and materializes only that task
under `.coop/task-executions/<generation>/<assignment>/tasks`. Existing loop prompts, receipts,
between-review, and signoff run against this projection; the canonical queue is never mounted into
the box. A fork takes its canonical queues in the order coop loop claims from them
(`forkCanonicalQueues`, internal/cli/fork_cmd.go; `forkAssignmentCandidates`): every `--tasks`
queue as given, else COOP_TASKS's, else project.yaml's, then work in progress first and todo tasks
by priority, queue order breaking ties before folder order. `forkCreate` resolves COOP_TASKS as
`tasks.TaskQueues` does for coop loop and makes the existing queues the loop's own `--tasks`, so a
detached worker and every Continue line carry them.

Projection state is evidence, not authority. The host opens it with rooted no-follow operations,
rejects extra tasks/symlinks/hardlinks/oversized trees, snapshots it immutably, then syncs only the
allowed Markdown/artifact surface while holding exact task authority. The box can swap the
assignment folder, or one above it, for a link while it runs, so the in-box task tools walk to the
projection from the fork workspace without following a link and refuse it when a link is on the way,
and write proposals through the outbox that walk opened. For the projection the walk is a check, not
a handle: task mutations still reach it by name and can race a swap ([[in-box-task-channel]]).
Blocked work updates the canonical blocked folder, and the fork scheduler (`runForkLoop`,
internal/cli/fork_cmd.go) then assigns its next task: a pass that exits 3 is accepted as it is,
never through `PrepareForkProjectionForRun`, because a resumed (`paused`) assignment's restore would
unblock the projection again and lose the new decision. The worker reads its answer from the
projection, never the canonical folder, and acceptance copies the projection's decision.md back over
the canonical one, but never an answer in it: the box can write that copy, and the canonical
Resolution line is the person's, so unless the copy is the canonical decision.md byte for byte, its
every Resolution line, indented or not, arrives as the unanswered placeholder, and the canonical
answer is written back in while the copy asks the same `# Decision:` question; a copy asking another
question, or a decision without that heading, waits for a new answer (`syncProjectionDecision`,
internal/tasks/projection.go, which landing goes through too). So
`resolveAndUnblock` (internal/tasks/cmd.go) copies the canonical decision.md
into the validated projection (`syncForkDecision`) under the canonical lease, before the task returns
to todo, and refuses, leaving the task blocked, when it cannot. The unblock also notes itself in the
projection's log.md as the canonical log does (`unblockedNote`), because acceptance copies that log
over the canonical one: once, and also when it changes no decision.md (the answer was already in
both copies, or there is none). The resumed restore repeats the copy for an unblock that stopped
before it, but only over a copy that lacks the answer to its own question: a worker that died after
parking a new question, before acceptance carried it here, keeps that question. `coop fork stop`
holds the lifecycle lock that acceptance waits on, so `PauseForkAssignments`
(internal/tasks/assignment.go) accepts a copy the stopped worker had parked instead of pausing it
(`forkCopyParked`). A resumed assignment stays `paused` while it works, and its copy stays blocked
with the answer until the pass restores it, so a paused one counts as parked only once its pass took
the canonical task back into progress, and only when it asks another question than the canonical
decision.md. Answered and waiting in todo, it is no new question, and acceptance refuses a blocked
copy of a todo task before any of the copy's files reach the canonical folder: copied first, the
copy's decision.md had replaced the person's answer. Otherwise only decision.md moves; the
projection's other notes, artifacts and scratch stay its own. When the task's decision.md holds an
answer, the loop prompt starts with it (`decisionResumePrompt`, internal/loop/prompts.go), and a
decision.md that cannot be read stops the launch.
A projected done task becomes `reviewing`; after all signoff and signing,
one generation candidate atomically binds HEAD/tree plus every assignment and projection digest.
The loop reviews and publishes it only once the scheduler drains, so a fork whose queue has more
work is never mergeable mid-run: the next task is assigned right after acceptance, before the
pass's summary prints. `--stop-after-review` (`ForkAssignmentRequest.StopAfterReview`) makes the
scheduler take no new task once the generation holds a `reviewing` or `ready` assignment, while it
still resumes one it holds, so the loop goes on to the candidate review and stops ready to merge.
Canonical tasks remain in-progress until merge lands that exact candidate. The one other done path
is a person's `coop tasks done <id> --from-fork` (internal/tasks/landed_fork.go): with the fork
stopped, no live candidate and exactly one reachable `Coop-Task` commit on the project's HEAD, it
imports the fork's proposals, completes the task, then removes proposal records, the index and the
owner record in that order, so a rerun finishes an interrupted one. A commit already in the
assignment's baseline (the loop's, say, before its completion was refused) also needs the project's
HEAD to reach every commit the fork's HEAD reaches beyond that baseline, trailer or not: any of them
may be the fork's work for the task. The baseline is the fork's HEAD when it took the task, which
the project lacks while the fork has unmerged work, so its history then comes from the workspace the
fork's generation names (`forkReachable`). Every one of these reads is the audit's raw walk, which
checks each object ID and parses parents itself, so a shallow file, graft or commit-graph a box
writes cannot change the answer, and a history it cannot read refuses. A crash between the index and
owner removal followed by `coop fork rm` leaves an owner record nothing clears; rerun --from-fork
before removing the fork.

The project's views of a fork's decision name the fork from the canonical owner record
(`taskForkName`, internal/tasks/cmd.go): `coop tasks decisions` and `-i` after the task id, the
board's Needs you in the detail line's `<agent> in fork <name>` part. The lookup only labels: a
record it cannot read leaves no label, and the commands that act on the task read the record
themselves. After an answer, unblock says the fork picks the task up on its next pass and that a
stopped loop resumes with `coop fork <name> --loop -d`, the board's restart command. A fork that
stops for its decisions names each task and what it still needs (`forkDecisionStep`,
internal/cli/fork_cmd.go): an answer in the project's decision.md waits only for unblock, and one
in the projection is named but never read as the answer. Resume never unblocks by itself: an
answer typed into the project's decision.md still waits for the person's unblock, which alone sends
the task back. Without `--tasks` or `COOP_TASKS`, unblock also searches the queues the assignment
index names (`forkQueuesBeyond`, internal/tasks/queue.go), so a fork's `--tasks` queue needs no
flag. Run in a fork's own checkout, unblock works on the project the host's generation record binds
that checkout to (`forkspace.ResolveProjectBinding`), never on a queue in the checkout, which the
box can write; the restart its note names carries `COOP_REPO=<project>` wherever the person is
outside the project (`forkResumeNote`).

Merge writes a replayable land intent before the parent fast-forward. Recovery can therefore finish
canonical task completion, receipts, proposal import, assignment removal, and workspace teardown
exactly once after a crash. Stop merely pauses assignments. Removal/fresh/session discard either
refuse unresolved ownership or write and replay an exact-generation discard. Trailers are human
labels and consistency checks only.

`ReadProjectSnapshot` is the shared read model: it joins canonical queues (including external
assignment roots), typed owners, reverse indexes, generations, candidates, land intents, detached
workers, sandbox executions, and remote-session reservations. Corrupt or unverifiable evidence is
rendered as a problem and blocks mutation; copied queue folders are never state-ranked into truth.
Execution records live beside fork state. The per-project fallback folder older builds could use
(`~/.local/state/coop/executions/v1/<key>/`) is no longer read: anything in it is reported by path
as a problem, and the service-launch locks beside it stay.

## Changelog
- 2026-11-06 — restoring a done projection (`prepareForkProjection`, which both the resume and the
  failed-pass restore use) first settles the projection's completion windows with
  `ReconcileCompletionWindowsWithActivity`, as a loop's own start does, after it has proved the
  owner and before the move. A worker killed in its final review leaves a window that archives the
  folder, and the resume's own move out of done then read as an unexplained departure
  ("completion ownership changed before recovery") and failed the first resume; the second worked
  only because that error consumed the window. The queue the task came from was never a cause: the
  windows are keyed by the projection. A window someone holds, and a change the journal cannot
  explain, are left as they were (task 2026-11-06-survive-a-killed-fork-worker-and-a-slow-docker-a).
- 2026-11-06 — a changed copy that still asks the person's question keeps their answer: any byte the
  fork changed, such as one line it added, had reset the canonical answer to the placeholder and
  lost it; an old answer still never carries onto a new question (task
  2026-11-06-keep-the-person-s-answer-when-a-fork-s-decision).
- 2026-11-06 — unblock run in a fork's own checkout answers the task in the project; it had read
  the checkout's queue, which a fork's clone lacks and its box can write, and the restart it named
  only worked in the project (task 2026-11-06-find-a-fork-s-task-when-coop-tasks-unblock-runs).
- 2026-11-06 — added `--stop-after-review`: a looping fork could not be merged until its whole queue
  drained, and a stop right after a review passed was already too late (task
  2026-11-05-let-a-fork-merge-its-last-reviewed-work-after-it). The merge refusal names the
  unreviewed tasks and the flag. Verified against assignment.go, fork_cmd.go and merge.go.
- 2026-11-06 — acceptance and landing copy the fork's decision.md without its answer unless the
  copy is the project's byte for byte: a Resolution the box wrote had reached the project's file,
  where unblock without an answer and coop loop's pre-flight took it for the person's (task
  2026-11-06-keep-a-fork-s-box-written-answer-out-of-the-proj). Verified against projection.go and
  candidate.go, the two callers of `syncProjectionTree`.
- 2026-11-06 — a stopped fork names each blocked task and what it needs, a blocked task's next
  action names the unblock command, and unblock finds a fork's `--tasks` queue through the
  assignment index; resume still never unblocks, since a box-written answer reaches the project's
  copy at acceptance (task 2026-11-05-unblock-a-fork-s-task-when-its-decision-is-answe).
- 2026-11-06 — `--from-fork` accepts a binding from the assignment's baseline once the project has
  every commit the fork made since; the loop's earlier commit had left a fork-held task with no exit
  but `coop fork rm --force` (task 2026-11-06-let-a-fork-held-task-close-when-its-only-commit).
  A review found that counting only the fork's trailer-carrying commits missed unbound rework and
  that a fork-side read failure counted as "brought over later"; both now refuse. The baseline check
  had asked only the project's repository, which lacks a fork's unmerged commits, and `merge-base`
  in a box-written repository follows its shallow file. Verified against landed_fork.go.
- 2026-11-06 — a fork loop without `--tasks` takes COOP_TASKS's queues, as coop loop does, and its
  Continue lines repeat them (task 2026-11-06-make-a-fork-loop-honor-coop-tasks-like-coop-loop).
- 2026-11-06 — a fork takes its queues in their configured order, not by folder name, and every
  `--tasks` it is given (task 2026-11-06-make-coop-fork-loops-honor-configured-queue-orde).
- 2026-11-06 — the unblock notes itself in the projection's log too, so the canonical "Unblocked:"
  line survives the fork's next acceptance, and `coop fork stop` accepts a copy its worker had just
  parked: paused, the question stayed in the fork and the next pass restored the copy as answered.
  Found the resumed-but-`paused` phase while fixing the stop. A review found the stop accepting a
  paused copy of a task answered and waiting in todo, which copied the box's decision.md over the
  answer before refusing todo: a paused copy now counts only once its task is in progress again, and
  acceptance checks the canonical state before copying (task
  2026-11-06-close-the-last-gaps-in-carrying-fork-decisions-a).
- 2026-11-06 — the project's decision views name the fork, and unblock says how the fork goes on
  (task 2026-11-05-let-a-person-answer-a-fork-s-blocked-decision-fr).
- 2026-11-06 — `coop tasks unblock` copies the answer into the fork's projection, the resumed restore
  repeats it, and the loop prompt carries it; the fork's worker had resumed from a copy with no
  answer (task 2026-11-05-carry-a-coop-tasks-unblock-answer-into-the-fork). Acceptance still copies
  the projection's log.md over the canonical one, so the canonical "Unblocked:" line does not survive
  the fork's next acceptance.
- 2026-11-06 — added the person-only `--from-fork` completion of a fork's task whose commit
  already landed (task 2026-11-05-let-the-owner-complete-a-fork-held-task-whose-co).
- 2026-11-05 — a fork loop goes on with its other tasks after one blocks, and a re-blocked resumed
  task stays blocked (task 2026-11-05-keep-a-fork-loop-working-its-other-tasks-when-on).
- 2026-11-04 — dropped the "fall back" sentence: the fallback registry reader is retired (task
  2026-11-04-drop-the-old-fork-and-task-record-readers). A copied queue in a fork is no longer
  refused: the snapshot never read one, and the fork loop takes its tasks from the canonical queue.
- 2026-10-17 — the in-box task tools check the projection, and open the outbox, from the fork
  workspace: a box that swapped its assignment folder or a folder above it for a link to another
  project's .agent had them list, read and change that project's tasks. Verified against
  channel.go, dir.go and taskmcp; the check-then-use race is recorded in [[in-box-task-channel]].
- 2026-09-23 — corrected the allocator assumption: tasks remain authorized by random queue/task
  identity plus locks, semantic records and live pins; durable fork generations now use a private
  hardlink anchor. No task record migration was introduced.
- 2026-09-18 — the fork workspace generation now binds the inode too, and the three places that
  compared whole task instances with `!=` (proposal.go, candidate.go, projection.go) go through
  `sameTaskInstance`. See [[identity-fences-compare-the-inode]].
- 2026-09-18 — the instance fence compares the inode, not the device (6d92825a for ownership; the
  completion receipts, windows and pending reviews followed). Verified against identity.go,
  completion.go and lease.go; the fork workspace generation still compares the device (queued).
- 2026-08-28 — created with canonical scheduling, one-task projections, generation candidates,
  replayable landing/discard, and the unified project activity snapshot.
