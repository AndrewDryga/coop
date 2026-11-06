---
name: in-box-task-channel
description: a loop work box or an interactive agent box reaches its task queue through a coop-owned MCP server over a helper-container unix socket, never a host-created one and never HTTP
subsystem: box
sources: [.github/workflows/task-channel.yml, internal/taskmcp/taskmcp.go, internal/taskmcp/tools.go, internal/taskchannel/mux.go, internal/taskchannel/mux.js, internal/runtime/runtime.go, internal/box/taskchannel.go, internal/box/open_broker.go, internal/box/run.go, internal/box/filtered_mounts.go, internal/mcp/mcp.go, internal/loop/prompts.go, internal/cli/doctor.go, internal/tasks/channel.go, internal/tasks/completion_record.go, internal/tasks/pin.go, internal/tasks/dir.go, internal/tasks/clutter.go, internal/tasks/cmd.go, internal/tasks/template.go, internal/tasks/proposal.go, internal/tasks/projection.go, internal/loop/loop.go, internal/loop/review.go, internal/box/taskchannel_live_test.go, Makefile, internal/tasks/audit.go, internal/tasks/session_claim.go, internal/cli/session_tasks.go, internal/cli/commands.go, internal/cli/acp_cmd.go, internal/cli/boxsweep.go, internal/box/sweep.go]
updated: 2026-11-05
---

A loop work box is told to change task state through the `coop-tasks` MCP server (eight tools:
`tasks_list/get/update_state/append_log/set_subtasks/complete/block/propose`), not by moving folders
or writing outbox JSON — its prompt says so (internal/loop/prompts.go). Nothing physically stops a
move: a work box mounts the repository writable, `.agent/tasks` included, and only review stages
remount the task queues read-only (`RepoReadOnlyPaths`, from `reviewReadOnlyPaths` in
internal/loop/review.go). The host catches a hand move after the run instead: a task the iteration
moved to done without holding its lease goes back to `10_in_progress/`
(`rejectUnownedCompletions` in internal/tasks/audit.go). The coop binary is still absent from the
box (doctor still asserts it) — the server is the ONLY host control surface a box gets, and it
exposes eight verbs (ten in an interactive session, below), nothing shell/exec/file.

**Interactive sessions get the same server, with nothing assigned.** An editor's `coop acp` inner
child and a `coop <agent>` box in a plain checkout whose project has a task queue set `TaskTools`
from `sessionTaskTools` (internal/cli/session_tasks.go). `Authority.Session` names the host process
the session's claims bind to: the ACP supervisor (the inner child's parent under
`COOP_ACP_SUPERVISOR`; its start token survives the exec a reload does, so claims outlive provider
switches, refreshes and child restarts) or the `coop <agent>` process. Fork sessions, controller
jobs, sign-in, `coop run -- <cmd>` and `coop shell` get nothing. A session server adds `tasks_claim`
and `tasks_release` — a loop box still lists exactly the eight, doctor's contract — and its
initialize instructions say the coop CLI is absent and these tools are how task state changes; the
box briefing gains a `# Tasks` note whenever a box serves task tools.

- One rule for every box, loop or session (`boxClaimRefusal` in internal/tasks/session_claim.go): a
  box changes, claims or releases only a task that is unclaimed, its own session's, or claimed by a
  process that provably stopped. A fork assignment, a person's claim and any other live claim
  refuse it, so `coop tasks claim --force` at a terminal stops a session at once; a loop box owns
  no claim, so every live claim keeps it off. `underLease` checks before the per-call lease and
  again once it holds it, and complete and block judge the claim under the locks they take
  (`CompleteBoxTask`, `BlockBoxTask`): a session claim needs that same lease authority, so a claim
  that lands after the first check still wins. Host `coop tasks claim` takes no flock, so a host
  claim racing a box write is the one window left.
- `tasks.ClaimSessionTask` holds the lease-authority flock and the owner lock, moves todo → in
  progress FIRST and writes the human-claim record second: a crash leaves an unclaimed in-progress
  task the loop resumes, never a todo task held by a claim nothing releases. It also refuses
  blocked and done tasks, a leased task, and a task halfway through a person's unblock (an
  `UnblockPending` audit record, on which no lease could start). `tasks_list`/`tasks_get` show a
  session who holds each live task (`claim`: `yours` or the owner's label).
- The loop re-reads a candidate's claim once it holds the lease (`releaseClaimedLease` in
  internal/tasks/audit.go; `beforeCandidateLease` is the test seam into that window), so a claim
  that lands while the loop is choosing wins. An owner record the loop cannot read is skipped with
  a warning instead of stopping every loop start — a box could otherwise break the loop by
  damaging the identity of a task it claimed.
- A session claim ends with its process: the queue shows the owner stopped and
  `coop loop --preflight` releases it. There is no per-session claim limit — a box can already
  hand-move the whole queue.

A write to a done task that is not the iteration's assigned one is refused (`underLease`): its
per-call lease would clear the completion receipt, and the loop's completion audit would then read
the task as an unowned completion and reopen it.

**A killed run's channel volume is removed by name or swept.** A run closes its channel on the way
out, but a killed one leaves the volume, while its helper exits with its client on stdin EOF. Every
replaced ACP child is killed (open and offline children are SIGKILLed), so the editor supervisor
names each child's volume (`COOP_ACP_TASK_CHANNEL`, checked by `box.ValidTaskChannelVolume`,
`RunSpec.TaskChannelVolume`) and removes it by that name once the child is gone, off the switch
path and retrying while the helper still mounts it (`removeACPTaskChannel`) — on any runtime,
Apple's included. The supervisor waits for those removals before its final reap and before a
reload's exec, either of which would otherwise end one mid-retry. For the rest — a closed terminal, a SIGKILLed loop — the volume is labeled
`coop.host` with the run's supervisor label, the box's own, and `box.ReapOrphanTaskChannels`
removes this workspace's volumes whose supervisor is provably dead: in the orphan sweep (loop,
fork, build) and in the ACP supervisor's start and exit reaps. A volume still mounted is refused
and left for a later sweep, and an unlabeled one is never touched; Apple's runtime has no label
inspection, so there the sweep does nothing. A channel that cannot start fails the launch, in an
editor as any child startup failure does (the proxy's respawn limit, the cause in the agent log); a
session whose tools cannot be set up (a bad `COOP_TASKS`, an unreadable claimant) starts without
them and warns.

Helper stdout is a caller-owned os.Pipe, not Cmd.StdoutPipe: background Cmd.Wait may reap before
the caller reads the final frames and must not close that reader. The parent closes its write
duplicate immediately after Start so the child's exit produces real EOF. Task-channel teardown
closes the reader after its bounded serving join; abnormal transport errors still fail cleanup.
The open MCP broker owns only one readiness line and closes/joins that reader on every outcome.

**Transport is a helper-container unix socket, decided by a spike (`internal/box/run.go` comments,
the task's decision.md).** A socket created ON THE HOST and bind-mounted into the box does NOT work
on OrbStack: the inode crosses the VM boundary (it `ls` as `srwxrwxrwx`) but `connect()` is refused —
the endpoint doesn't cross. HTTP is out too: a box runs `--network none` unless `egress: open`, and
there is no `host.docker.internal`. So the listener runs where the box's kernel is:

- `internal/box/taskchannel.go` creates a run-private NAMED VOLUME and starts a HELPER container from
  the box's own image (`--network none`, `--user root` inside its own namespace, `--cap-drop ALL`),
  running `node /coop/mux.js` (`internal/taskchannel/mux.js`). It listens on `/coop/tasks/mcp.sock`
  on the volume. A socket created by a LIVE container on a shared named volume IS connectable from
  another container mounting that volume — the transport rests on that fact.
- The box mounts the same volume READ-ONLY at `/coop/tasks` and its MCP clients connect with
  `socat STDIO UNIX-CONNECT:/coop/tasks/mcp.sock` (socat ships in the image). Read-only is fine:
  `connect()` needs write permission on the socket inode (the helper `chmod 0666`s it), not on the mount.
- The helper is a MULTIPLEXER, not a raw `socat` listener: the MCP snapshot fans out to EVERY agent
  in the box (lead + consult peers + preset roles), so several clients connect at once. `mux.js`
  tags each connection and carries them all over the helper's single stdio; `internal/taskchannel`
  (`ServeMux`) demultiplexes on the host and hands each connection to `taskmcp.Server.Serve`. It runs
  `net.createServer({allowHalfOpen:true})` so a client that half-closes its write side still receives
  its reply.
- The helper lives in its OWN pid namespace, so coop-entry's descendant drain (`live_jobs()` in
  `internal/box/image.go`) never counts it as agent background work — the reason a `docker exec`
  helper INTO the box was rejected.

Under `egress: filtered` the box's volume mount is validated by `filtered_mounts.go`. The
run-private channel volume is exempt from that host-path exposure check (`filteredExecution.taskVolume`):
it is coop-created this run, holds only the socket, is mounted read-only, and its backing mountpoint
lives inside the daemon VM — not a host path an agent could redirect — so it is trusted like the
run's generated files. Any OTHER named volume still goes through daemon exposure inspection.

`mcp.BindTaskTools` (beside `BindControllerTools`) merges the `coop-tasks` stdio server into the
validated snapshot BEFORE the per-run artifact is written (in `runWithCompositionArtifacts`,
internal/box/run.go), so all four provider projections carry it unchanged (see
[[mcp-authority-projection]]). No bearer token: the socket is mounted only into this run's box, so
the mount is the authority, not a secret.

**Authority is explicit, never ambient.** `taskmcp.Authority{Workspace, QueueRoots, Assigned, ProposalOutbox,
Owner, ValidateAssignedCompletion}` is built host-side by the loop (`internal/loop/loop.go`, where the lease lives). Every tool
works on every task in those queues — there is no per-run scope; the assigned task is what the prompt
says to work, not a permission. A mutation on a task
another live process leases is refused at the call (`tasks.TryTaskLease` / `ErrTaskLeased`), with a
legible "held by another live process" message — the assigned task runs under the launching
iteration's own lease and never leases twice. `tasks_complete`/`tasks_block` on another task use the
host's trusted completion/block (its own lease + receipt). `tasks_complete` refuses a blocked task,
the box's own included, as `coop tasks claim` does: only the host's `coop tasks unblock` returns it to
work. `tasks_block` replaces decision.md through
`tasks.ReplaceDecision`: a decision the human already answered goes into log.md first, and the
result tells the agent it had been answered. In a fork, `tasks_propose` writes the
validated proposal into `Authority.ProposalOutbox` (the same file `tasks.ImportForkProposals` reads at
merge); in a plain loop it creates the `00_todo/`/`xx_backlog/` folder directly. Forks reach this same
`box.Run` plumbing — `internal/loop/iteration.go` builds one RunSpec for plain and fork iterations.

**Every queue is walked to from the workspace the box mounts.** The box can replace any folder
inside its mount while the tools run, and a queue found by name follows a folder swapped for a link
(`.agent`, a subproject, a fork's assignment folder or one above it, a state folder) into another
project. `Authority.Workspace` names that mount (the repository, or the fork workspace), the one
folder the box cannot replace, and `New` refuses a queue root or outbox outside it. The walk is
`tasks.openRootBelow`: `os.OpenRoot(workspace)`, then `openRealSubroot` opens each folder from its
parent and refuses one that is a link. Two kinds of use follow:

- Through the walk's handle: `tasks_get` (`ReadTaskFiles`), the fork outbox (`WriteForkProposal`)
  and plain proposals. `CreateDraftTask` walks with `ensureRealSubroot`, which also makes a missing
  queue, then `createTaskFolderIn` refuses a linked state folder or a README.md that is a symbolic
  link, writes the new files `O_EXCL` through the handle, and removes what it wrote if a write
  fails. A hard-linked README.md is read: the template is only read, and a box can hard-link only
  files in its own mount. `coop tasks add` shares that creator; the fork proposal import shares the
  README reader (`readTaskTemplate`) and publishes through handles too (`publishProposalTask`): it
  stages the files in a fresh real folder under `.coop-proposal-staging` and moves it into its state
  folder with `renameFolderBetween`, a `renameat` between the two folders' own handles.
- Through the call's pin: every tool call first pins the workspace and each queue root in it
  (`tasks.PinQueues`, from `Server.call`), opened from the workspace through real folders only; a
  call that starts while one of them is a link is refused. Until the call ends, every task read and
  write in the process that names a path inside a pin opens it through that handle (`openPinned`):
  the tree reader (`readTaskTreeOnce` lists state folders from one queue handle), every metadata
  writer (`OpenTaskMetadataRoot`), moves (`MoveTaskDir`, a `renameat` between the state folders'
  own handles), deletions, completion receipts and fingerprints (`lstatTaskPath`, the walk through
  the task folder's handle), and the lease key and queue identity (`pinnedCanonical`, the
  workspace's real path plus the link-free rest). A folder the box swaps after the pin therefore
  redirects nothing; `TestToolsStayInThePinnedQueueWhileFoldersAreSwapped` swaps `.agent`, the
  queue root and a state folder in a loop during 900 calls. A queue missing at the pin is reached
  through the workspace pin, so it reads as empty and a task created in it is still found. Host
  commands use the same helpers without a pin: they open the queue root by name, a real folder and
  not a link (`openRealRoot`), and everything below it through handles, and `coop tasks block`
  creates its decision.md template `O_EXCL|O_NOFOLLOW`. Every walk opens a folder
  `O_DIRECTORY|O_NONBLOCK` first and makes the root from that descriptor (`openSubrootNoWait`), since
  `os.Root.OpenRoot` waits on a FIFO a box swaps in after the folder was checked.

The loop runs the same `ReadTaskTreeBelow` at startup, so a linked subproject stops it there. The
completion bulk note measures from the workspace with its own `openat(O_NOFOLLOW|O_NONBLOCK)` walk.

Assigned completion also gets early host feedback: the loop supplies an immutable callback tied
to that iteration's base and audit authority. `tasks_complete` invokes it before the done move;
a refused binding leaves the task untouched and the agent can repair it in the same session.
The complete post-exit ref/lease/raw-history audit remains authoritative. Standalone diagnostic
servers have no iteration to bind and omit the callback. This does not expose a new tool or let
provider input choose a validation policy.

Every official completion path also requires a nonempty, fully checked current checklist.
Assigned MCP rereads it after binding validation; the host finalizer checks again after exit.
An empty/open checklist refuses without clearing existing completion authority or scratch;
the refusal names up to five open items (`tasks.IncompleteChecklistError`).
No task tool can force completion. The host's `coop tasks done --force` needs a terminal, and
it leaves a log.md note.
Fork identity validation still permits an unfinished done projection so crash recovery can
reopen it; captured completion acceptance, publication, and landing enforce the prerequisite.
This checks task structure, not whether the agent actually ran a claimed verification.
`tasks_complete` also requires `unrun_checks`, the worker's own list of required checks that did
not run after its final edits: `[]` completes, and any entry refuses after the checklist check and
before binding validation, pointing at `tasks_block`. An omitted list is a missing field and null is
refused, so neither reads as "every check ran". It is a report, not proof.
Completion also refuses Coop's scaffold in the record: state.md's Done so far or Traps still
holding the template's `<…>` text (`tasks.StateTemplateFields`), or a log.md with no entry past
the template and Coop's own notes (`tasks.LogHasEntry`; a new host note's opening words go in
`coopLogNotes`). A no-change outcome needs no entry first, since its closure writes one.
`tasks_update_state` refuses a complete status with such a field, and `coop tasks lint` names done
tasks whose state.md does. Lint skips empty logs: the archive held 170 from before the refusal,
which would bury findings someone can act on.

Tool input omissions are classified centrally from the same ordered required fields
advertised by tools/list. Every missing top-level field is reported together, with its static
description; submitted prose is never included in this repair. State updates replace
all four fields on every call, including literal "none"/"—" when appropriate.
Present null, empty, unsafe or overlong values still reach strict field validation.
Text bounds and descriptions share the tasks package's UTF-8 byte limits: JSON Schema
maxLength would count characters, so it cannot express those limits. The MCP decoder
rejects invalid raw UTF-8 before encoding/json can replace it and hide it from validation.
Optional fields default only when omitted: explicit null is refused, including checklist
done booleans. A fork proposal also checks its serialized JSON size before publication so
a successful write fits the importer's file bound, even when JSON escaping expands text.

`tasks_list` accepts an optional literal, case-insensitive `query` over ID/title,
combined with `state`, using only QueueRoots and their existing ordering. Query
input is validated before trimming (nonempty, control-free UTF-8, at most 256 bytes).
Searched calls return a complete prefix of at most 20 summaries and 64 KiB of serialized
MCP result, with matched/returned counts and explicit truncation/narrow-query guidance.
If any matching summary cannot fit on its own, the tool refuses with its ID for `tasks_get`;
titles are never silently shortened. Omitted query retains the original unbounded
tasks-only list. The fork proposal outbox and backlog are never searched.

The loop prompt uses that bounded lookup before proposing related work, reads plausible
matches with `tasks_get`, and records an existing owner's ID/new evidence in the assigned
log without modifying the other task. Proposal context/acceptance descriptions distinguish
observations from suspected causes and preserve failing behavior/denial assertions. The
server validates structure and authority, not diagnostic truth or semantic duplication;
an accepted proposal is not evidence that its diagnosis is correct.

Two checks reach the channel from a real box, and both need a built image: `coop doctor`'s task
section, skipped when doctor falls back to its alpine stand-in (CI's doctor job has no `coop build`),
and `make task-channel-e2e` (`TestTaskChannelLiveInARealBox`: completes its own task, is refused a
held one, finds no coop binary), which fails, never skips, without a runtime or this checkout's
`coop-box:<definition>` image ([[box-base-image-tags]]). CI runs it once a day on main
(`.github/workflows/task-channel.yml`, also by hand): it builds Coop, builds the shared image from
an empty folder so the repository's own Dockerfile is not picked, then runs the target. The owner
chose daily over every push, since the image takes minutes and many downloads to build.

## Changelog
- 2026-11-05 — completion refuses template text in Done so far/Traps and a log.md without an entry;
  lint names done tasks with template text, not old empty logs (task 2026-11-05-keep-template-placeholders-out-of-a-completed-ta).
- 2026-11-05 — `tasks_complete` takes a required `unrun_checks` list and refuses a nonempty one
  (task 2026-11-05-refuse-completion-when-the-worker-reports-a-requ).
- 2026-11-03 — interactive agent sessions (`coop acp`, `coop <agent>`) get the task tools plus
  `tasks_claim`/`tasks_release`, claims bound to the session's host process; session writes respect
  other live claims; done tasks refuse non-assigned writes; the loop re-checks a claim after it
  leases; channel volumes carry `coop.host` and dead runs' volumes are swept. A design critic found
  the volume leak, the receipt-clearing late write, the adoption race and the record-before-move
  phantom claim before landing. An independent review then found that loop boxes and person claims
  still let a box close another's task, that the check ran before the lock, that Apple runs leaked
  every volume, the pending-unblock claim and the unreadable-record loop stop. All are fixed: one
  `boxClaimRefusal` rule for every box, checked under the lock, a supervisor-named volume
  (task 2026-11-03-let-coop-acp-and-coop-run-agents-change-task-sta).
- 2026-11-03 — `tasks_complete` refuses a blocked task (it had moved one to done past its open
  decision); `coop tasks claim` refuses blocked and done tasks the same way.
- 2026-11-03 — `coop tasks done --force` is host-only; `tasks_complete` stays strict (task
  2026-11-03-let-a-person-force-close-a-task-with-open-checkl).
- 2026-11-03 — checklist refusals name the open items; the labels travel with `tasks.Item` (task
  2026-11-03-name-the-open-checklist-items-when-coop-tasks-do).
- 2026-10-18 — a daily CI job runs `make task-channel-e2e` in a freshly built box image (task
  2026-10-17-exercise-the-task-channel-in-a-real-box-in-ci).
- 2026-10-18 — each tool call pins its workspace and queues and every read and write goes through
  the pin; folder walks no longer wait on a swapped-in FIFO (task
  2026-10-17-run-the-in-box-task-tools-through-one-pinned-que).
- 2026-10-18 — host movers, metadata writers, tmp cleanup and the two task deletions go through
  handles below the queue root; the block template is created no-follow (task
  2026-10-17-write-task-metadata-and-moves-through-the-state).
- 2026-10-17 — the fork proposal import stages and publishes through folder handles
  (`publishProposalTask`, internal/tasks/proposal.go), not by path.
- 2026-10-17 — corrected the MCP-only claim: a work box mounts `.agent/tasks` writable (only review
  stages get `RepoReadOnlyPaths`), so the tools are what its prompt allows, not the only thing that
  can move a folder; the host's `rejectUnownedCompletions` undoes an unleased move. Renamed
  `BindResponderState` to its successor `BindControllerTools` (0e7c5c45) and replaced the
  approximate run.go line citation with `runWithCompositionArtifacts`. Verified against
  internal/loop/review.go, internal/loop/iteration.go, internal/box/run.go,
  internal/tasks/audit.go, internal/mcp/mcp.go and internal/loop/prompts.go (added
  internal/loop/review.go to sources).
- 2026-10-17 — `make task-channel-e2e` runs the real-box test, which no target had run since it was
  written; verified doctor's skip on the alpine stand-in against internal/cli/doctor.go. The test
  had rotted: it looked for an untagged `coop-box` (now resolved like a launch) and completed an
  unchecked checklist, which completion refuses; it now fails instead of skipping.
- 2026-10-17 — the tools now walk to every queue from the workspace the box mounts
  (`Authority.Workspace`): before, a fork box that swapped its assignment folder or a folder above
  it, or any box that swapped `.agent`, a subproject or a todo/backlog folder, for a link had the
  tools list, read, change and add to another project's tasks. Reads check the walk, then go by
  name; creation, `tasks_get` and the outbox go through its handle. Regressed through real JSON-RPC
  calls for each swap; the residual check-then-use race is recorded above.
- 2026-10-13 — hosted release CI caught a helper-output ownership race during normal shutdown.
  Reaping previously closed the reader early, losing buffered output or yielding ErrClosed.
  Caller-owned stdout and explicit consumer cleanup preserve final data and EOF without
  suppressing transport errors; deterministic delayed-read and readiness cleanup regressions
  cover the source contract. No credential or network authority changed.
- 2026-09-19 — `tasks_block` keeps an answered decision in log.md before replacing it.
- 2026-09-13 — shared loop discovery now checks existing owners; proposal descriptions/example
  preserve uncertainty and abnormal behavior. Scripted lookup/reuse and separate-new-proposal
  tests do not claim native model compliance or semantic deduplication.
- 2026-09-13 — added bounded literal ID/title search; real JSON-RPC regressions cover
  authority, unchanged unfiltered calls, invalid-query recovery and escaped-size limits.
- 2026-09-12 — verified omission repair and byte-limit descriptions against real JSON-RPC
  plain/fork calls; raw malformed UTF-8 previously reached a task log as replacement text.
- 2026-09-12 — traced checklist feedback and freshness through MCP, host finalization and fork
  acceptance; documented the deliberate separation from recovery and independent test proof.
- 2026-09-12 — traced assigned completion from loop authority through tasks_complete and its
  post-exit audit; added pre-move feedback without changing the transport or trusted final audit.
- 2026-09-10 — created with the feature (task 2026-09-08-give-an-in-box-agent-a-coop-owned-task-mcp-serve).
  Records the transport spike outcome (host-mounted socket refused on OrbStack; helper-container named
  volume works), the multiplexer + allowHalfOpen, the lease-only refusal, and the fork path.
