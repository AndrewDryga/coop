---
name: in-box-task-channel
description: the loop box reaches its task queue through a coop-owned MCP server over a helper-container unix socket, never a host-created one and never HTTP
subsystem: box
sources: [internal/taskmcp/taskmcp.go, internal/taskmcp/tools.go, internal/taskchannel/mux.go, internal/taskchannel/mux.js, internal/box/taskchannel.go, internal/box/run.go, internal/box/filtered_mounts.go, internal/mcp/mcp.go, internal/loop/prompts.go, internal/cli/doctor.go]
updated: 2026-09-12
---

A loop work box changes task state through the `coop-tasks` MCP server (eight tools:
`tasks_list/get/update_state/append_log/set_subtasks/complete/block/propose`), not by moving folders
or writing outbox JSON. The coop binary is still absent from the box (doctor still asserts it) — the
server is the ONLY host control surface a box gets, and it exposes eight verbs, nothing shell/exec/file.

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

`mcp.BindTaskTools` (beside `BindResponderState`) merges the `coop-tasks` stdio server into the
validated snapshot BEFORE the per-run artifact is written (`run.go` ~line 350), so all four provider
projections carry it unchanged (see [[mcp-authority-projection]]). No bearer token: the socket is
mounted only into this run's box, so the mount is the authority, not a secret.

**Authority is explicit, never ambient.** `taskmcp.Authority{QueueRoots, Assigned, ProposalOutbox,
Owner, ValidateAssignedCompletion}` is built host-side by the loop (`internal/loop/loop.go`, where the lease lives). Every tool
works on every task in those queues — there is no per-run scope; the assigned task is what the prompt
says to work, not a permission. A mutation on a task
another live process leases is refused at the call (`tasks.TryTaskLease` / `ErrTaskLeased`), with a
legible "held by another live process" message — the assigned task runs under the launching
iteration's own lease and never leases twice. `tasks_complete`/`tasks_block` on another task use the
host's trusted completion/block (its own lease + receipt). In a fork, `tasks_propose` writes the
validated proposal into `Authority.ProposalOutbox` (the same file `tasks.ImportForkProposals` reads at
merge); in a plain loop it creates the `00_todo/`/`xx_backlog/` folder directly. Forks reach this same
`box.Run` plumbing — `internal/loop/iteration.go` builds one RunSpec for plain and fork iterations.

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

## Changelog
- 2026-09-12 — traced checklist feedback and freshness through MCP, host finalization and fork
  acceptance; documented the deliberate separation from recovery and independent test proof.
- 2026-09-12 — traced assigned completion from loop authority through tasks_complete and its
  post-exit audit; added pre-move feedback without changing the transport or trusted final audit.
- 2026-09-10 — created with the feature (task 2026-09-08-give-an-in-box-agent-a-coop-owned-task-mcp-serve).
  Records the transport spike outcome (host-mounted socket refused on OrbStack; helper-container named
  volume works), the multiplexer + allowHalfOpen, the lease-only refusal, and the fork path.
