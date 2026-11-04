---
name: acp-box-wait
description: how a filtered editor session stays connected while its project box cannot start, and why no turn runs meanwhile
subsystem: acp
sources: [internal/cli/acp_box_wait.go, internal/cli/acp_cmd.go, internal/cli/acp_refresh.go, internal/cli/fork_acp.go, internal/acpproxy/proxy.go, internal/acpproxy/first_answer.go, internal/acpproxy/restarter.go, internal/acpctl/start_notice.go, internal/box/project_preparation.go, internal/box/run.go, internal/box/network_admission.go, internal/box/filtered.go, internal/box/filtered_launch.go, internal/box/authority_mounts.go, internal/cli/acp_serve.go, internal/cli/cli.go]
updated: 2026-11-04
---

A filtered `coop acp` no longer exits when its project box cannot start. Admission runs with
`box.DeferProjectBuild`, and the supervisor asks `box.EditorBoxReadiness` itself: a project file that
does not load (`*BrokenProjectFile`), a project build miss (`*ProjectBuildRequired`), services with no
rules (`*ServicesNeedRules`) or rules nobody approved (`*networkstate.PendingApproval`). Any other
error is a failure to look, and proves nothing.

Rules nobody approved come first, since no capture exists for them to run under. `HoldPending` admits
`holdingAdmission`: the person's flags, the host's MCP and the provider bundles, and nothing from the
project's file, approved or not (a service grant would start sidecars). `CapturedEgress.Pending` marks
it, `spawnBox` then holds prompts whatever the wait last heard, and only a real capture ends the wait:
`acpNetworkRefresh` adopts one when the generation moves, and `readmitHeld` tries on every wait check,
since taking a request back moves no generation. A withdrawal, a replaced folder and an offline mode
still refuse at connect, since the store refuses them or filtered would widen them. Fork ACP does not
ask for this: it has no refresh to end the wait, so it still refuses at connect. An approved open or
offline mode cannot be moved to (`errACPNetworkModeChanged`), so the thread says to reconnect.

A project file that does not load gets the same hold, at connect or later. `dispatch` checks no file for
`coop acp`: a real child refuses one in `resolveLaunchImage`, and `admitNetwork` reads it as one that asks
for nothing, so `CapturedEgress.BrokenFile` marks a capture with none of it, and its pending comparison,
made against that empty request, means nothing. A file that reads as rules up to the line that fails
gives none of them, and a tolerant parse would be the bug. Admitting again refuses (`HoldPending` is off)
until the file loads, then it is an ordinary admission, so `refuse` keeps what a person has to settle in
`heldBy` and `readmitHeld` hands that to the wait: the thread moves from the file to `coop approve`, or to
a reconnect. A file that loads while its admission still fails for another reason reads as
`ErrProjectNotAdmitted`, so the thread stops blaming the file. Open needs care: a holding child narrows
it, but `--egress open` never asked for the host's filtered setup, so with no setup on record it refuses
at connect, and once fixed it asks to reconnect. Nothing else in the supervisor reads the file until then:
`boxOpened` runs `readFixedProject`, which gives the serve router its ports and lists the services. The
router has to exist from the start: measured with no router, each box of the session published the stable
port itself, and the warm boxes lost that race with a Docker failure in the log.

The file's text reaches the thread only through `brokenFileReason`: `safeErrorText`, cut at 600
characters, in a `CodeBlock`. yaml quotes unknown keys as written, so an edit can put a fence, a link or
an escape sequence in the error, and the whole file can be a megabyte. The agent log gets the whole
reason through `safeErrorText`.

Trap: a first approval takes `LockServiceLaunch` exclusively, and every box holds the shared side for
its lifetime, so a holding child would deadlock the approval it waits for. A holding child therefore
sees the project read-only, passes the mount guard as if anchored (`authorityProtection`), and drops
the window once validated. A real box in the project still makes a first approval wait, in silence.

While blocked, `spawnBox` starts a child with `COOP_ACP_HANDSHAKE_BOX=1`: `RunSpec.HandshakeOnly` runs
the locked client image and reads no project file, without serve ports, services, task tools or a writable project,
under rules of its own (`box.AdmitHoldingNetwork`, admitted for each such child: the fingerprint differs
from the session's, so `coop net runs` shows it, and the refresh's `holding` keeps it out of what the
live box is said to run). `Child.HoldPrompts` tells the proxy no prompt may reach it. Everything else does reach it:
`initialize`, `session/new`, `session/load` and settings need a real adapter, which is why this is a
real box and not a fake answer. Prompts go into `restartHeld`, the queue a provider switch uses, so
cancel, duplicate and size limits are the existing ones. They pass through `FromEditor` only at
release. A resend stays reserved: `ResumePrompt` is not fed to a holding child, and a reserved id does
not stop the move (`movableOffHoldingLocked`). A replacement that dies during replay leaves held and
reserved prompts for the next child; `failAllPending` answers them if the proxy gives up.

`acpBoxWait` polls every 2 s. Asking the real question reads the build context, about 1.1 s on a 5,600
entry project, so it asks only when `EditorBoxStamp` changes (the review record, which every finished
project build rewrites; project.yaml; the Dockerfile; the policy fingerprint) or every 30 s, which is
what catches an undone edit to a copied file. Approved files rebuild once in the background, with the
wait's own context, never a factory's. A failed rebuild is not retried until a check clears.

Trap: a held prompt makes `Restarter.Idle` false, and the network refresh waits for idle to adopt an
approval. The services wait is settled by `coop approve`, so refresh also counts `Holding` as idle: no
turn is running. The move itself is the wait's job, through `MoveOffHolding`, since `RestartIfIdle`
would never fire with a prompt held.

Trap: warm boxes. A fill while blocked would park a box that can serve no switch, so fills return
`errACPBoxWaiting` and `boxOpened` starts the fan-out when the session clears. The warm-off check at
startup skips a blocked session, or the pool stays off for its whole life.

Fork ACP shares the wait, but its hooks have no start notice, so a thread hears about it only when it
sends a message. See [[restricted-project-builds-are-explicit]].

A services refusal comes in parts (`box.ServicesNeedRules`), since a thread cannot show the terminal's text
whole: Zed does not wrap a code block. `acpBoxWaitText` shows the reason and the remedy as text and only the
rules and `coop approve` as blocks, and the terminal and the agent log keep `Error()`. `coop approve` has nothing
to approve in that state, so it asks the same check (`box.ProjectServicesRefusal`) and shows the refusal too.

Readiness asks about those four things only, on purpose, and about the file first: read as a file that
asks for nothing, it would send a person off to build what it does not ask for. Any other refusal (an
approved service whose definition changed, a serve port the policy grants TLS on) shows up as a real
child ending by itself. The proxy decides that in its run loop, not in `spawnBox`'s wait goroutine,
because it stops a child as soon as its stdout ends, so "exited before stop was called" races: not a
switch, a move, a reload or a shutdown, and it never answered initialize or lived under
`ChildStartWindow`. `RunOpts.ChildFailed` then reaches `acpBoxWait.childFailed` before the next spawn,
so that spawn holds prompts. Only `poll` starts a retry, when the stamp changes or every 30 s, so a spawn's
own question cannot make a tight loop; only a real box that answered and outlived the window ends it
(`confirmStart`), and until then the warm pool stays shut (`waitingNow`) and threads hear nothing new.

With `ChildFailed` set, only children that hold prompts count toward the rapid-fail cap, and one that
never answered counts however long it took to die, or a slow refusal would respawn forever. Reaching
the cap is what a stopped Docker looks like: every open thread hears why, and the session ends. Open
and offline sessions have no wait and keep the old rule. An `initialize` the first child never answered
gets the replacement's answer (`answerInitialize`, or held in `restartHeld` under the empty name if it
arrives mid-replacement), since an editor reads an error there as a dead server. `poll` replaces a
holding child that still owes the editor something after `acpBoxWaitMoveDeadline`, for a hung adapter or
an unfinished sign-in.

A box that hangs goes the same way. `watchFirstAnswer` (one per child, from `startChild`, only with
`ChildFailed` set) stops a child that does not answer the first thing it is asked, and the run loop then
treats it like one that exited: `ChildFailed` hears `ChildSilent` or `ChildNotReady`, the next child holds
prompts, and its answer becomes the editor's. A hung child wrote no reason anywhere, so these two texts say
what Coop saw. A replay that goes quiet after its first answer (`replayIdleTimeout`) counts as `ChildSilent`
too; open and offline sessions still end there, and their first box is still not watched.

The minute (`childAnswerWindow`) opens at `RunSpec.OnRuntimeLaunch`, the boundary the loop's provider
watchdog arms at, and never before the child is asked. An inner child reports it by creating a file its
supervisor named (`COOP_ACP_LAUNCHED`, `acpReportLaunch`, `Child.Launched`). Everything before it is host work
a minute must not cut short: `LockServiceLaunch` has no limit of its own and a running box holds it for
life, `up --no-start` pulls missing images, and `up -d --wait` waits for health. Measured first answers: about
6 s in a plain project, 18 to 46 s after a move in a project with services, 48 to 76 s connecting in emisar.
Stopping one of those loses work the retry repeats, so they answer only to `replayStartupGrace`, which is
now `ChildNotReady` and a retry rather than the end of the session. Past the boundary only the adapter is
left: `coop-entry`'s toolchain provisioning is the base image's, and a filtered image has none. So a box waiting
before that point can be silent for 5 minutes: the editor gets no earlier answer, on purpose.

Trap: a holding child takes the shared side of `LockServiceLaunch` to validate its mounts, so whoever holds the
exclusive side keeps it waiting too. A running box holds only the shared side, so it blocks a box with services and
not the holding child. An exclusive holder is what can delay the holding child's answer past the 5 minutes.

Trap: while a real box is tried, no child is live. Prompts and a reopened thread's `session/load` wait, but a
`session/new` gets "The agent is switching" (`switchPendingResponse`). A try that hangs lasts over a minute,
against 30 s on the holding child, so a second thread opened then often has to ask again.

Limits. A refusal a holding child meets too (an unreadable `.coopignore`, a preset that no longer loads,
`COOP_RUN_ARGS` with a path inside the project) ends at the cap with its reason in the log. A broken
project.yaml still ends `coop acp` at connect where nothing filtered can hold it: an offline mode, a
runtime with no gateway, `COOP_IMAGE`, an open mode on a host with no filtered setup, a withdrawal or a
replaced folder.

A project's own client hooks do run in a holding child at session/new. Measured with Claude's SessionStart:
the hook ran, the project was read-only, and it could reach only the provider. The owner chose on 2026-11-03
to keep this rather than start the adapter with hooks off. It reaches a project that asked for offline in
a file that no longer loads too, since nothing can read that request.

## Changelog
- 2026-11-04 — a services refusal reads whole in a thread, and `coop approve` shows it when it has nothing to
  approve. Verified with a real `coop acp`, pasting the rule out of the thread's own block.
- 2026-11-04 — a box that hangs is stopped and held like one that exits, and says so. Verified on real
  Docker: an adapter that blocks at startup (answered at 68 s, retried, moved once fixed), a lock released at
  100 s (not cut short), and a lock never released (stopped at 5 min).
- 2026-11-04 — a broken project.yaml keeps the session connected too, on a capture with nothing of the
  file's.
- 2026-11-03 — recorded that a project's hooks run in a holding child, by the owner's choice.
- 2026-11-03 — a box that keeps refusing keeps the session connected, on a child with no project rules,
  no sidecars and no project file. Verified on real Docker against a serve port on TLS, a broken
  project.yaml and an approved service edited before the box was killed.
- 2026-11-03 — rules nobody approved wait too, on a holding capture. A real run on Docker hung `coop
  approve` on the lifetime lock, which the read-only mount and the dropped window fixed.
- 2026-11-03 — created with the wait.
