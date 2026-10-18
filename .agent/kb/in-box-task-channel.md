---
name: in-box-task-channel
description: the loop box reaches its task queue through a coop-owned MCP server over a helper-container unix socket, never a host-created one and never HTTP
subsystem: box
sources: [internal/taskmcp/taskmcp.go, internal/taskmcp/tools.go, internal/taskchannel/mux.go, internal/taskchannel/mux.js, internal/runtime/runtime.go, internal/box/taskchannel.go, internal/box/open_broker.go, internal/box/run.go, internal/box/filtered_mounts.go, internal/mcp/mcp.go, internal/loop/prompts.go, internal/cli/doctor.go, internal/tasks/channel.go, internal/tasks/clutter.go, internal/tasks/cmd.go, internal/tasks/template.go, internal/tasks/proposal.go, internal/tasks/projection.go, internal/loop/loop.go, internal/loop/review.go, internal/box/taskchannel_live_test.go, Makefile, internal/tasks/audit.go]
updated: 2026-10-18
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
exposes eight verbs, nothing shell/exec/file.

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
host's trusted completion/block (its own lease + receipt). `tasks_block` replaces decision.md through
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
- As a check only: `tasks_list` and every tool that names a task read the queue with
  `tasks.ReadTaskTreeBelow`, which runs the walk down to the queue root (a missing folder still
  reads as empty), then `ReadTaskTree` by name, which refuses a linked state or task folder. The
  mutations then open the queue root by name, a real folder and not a link (`openRealRoot`), and
  everything below it through handles: `MoveTaskDir` renames between the two state folders' own
  handles (`renameFolderBetween`), every metadata writer opens the state and task folders from the
  queue (`OpenTaskMetadataRoot`), `removeTaskTmp`, `coop tasks rm` and `coop backlog rm` delete
  through those handles, and `coop tasks block` creates its decision.md template `O_EXCL|O_NOFOLLOW`.
  A folder swapped above the queue root between the check and the operation is not ruled out
  (task 2026-10-17-run-the-in-box-task-tools-through-one-pinned-que); the reader has the same
  check-then-use at each state and task folder. `openRealSubroot` checks each folder with `Lstat` before `os.Root.OpenRoot`, which does
  not pass `O_NONBLOCK`, so a box that swaps in a FIFO between the two makes that call wait.

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
An empty/open checklist refuses without clearing existing completion authority or scratch.
Fork identity validation still permits an unfinished done projection so crash recovery can
reopen it; captured completion acceptance, publication, and landing enforce the prerequisite.
This checks task structure, not whether the agent actually ran a claimed verification.

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
`coop-box:<definition>` image ([[box-base-image-tags]]).

## Changelog
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
