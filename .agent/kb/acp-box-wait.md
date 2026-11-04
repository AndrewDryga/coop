---
name: acp-box-wait
description: how an editor session stays connected while its project box cannot start, and why no turn runs meanwhile
subsystem: acp
sources: [internal/box/service_launch_wait.go, internal/box/filtered_services.go, internal/box/service_observation.go, internal/runtime/service_observation.go, internal/cli/acp_box_wait.go, internal/cli/acp_cmd.go, internal/cli/acp_refresh.go, internal/cli/fork_acp.go, internal/acpproxy/proxy.go, internal/acpproxy/first_answer.go, internal/acpproxy/restarter.go, internal/acpctl/start_notice.go, internal/box/project_preparation.go, internal/box/run.go, internal/box/network_admission.go, internal/box/filtered.go, internal/box/filtered_launch.go, internal/box/authority_mounts.go, internal/cli/acp_serve.go, internal/cli/cli.go, internal/forkspace/execution.go]
updated: 2026-11-04
---

A filtered `coop acp` no longer exits when its project box cannot start. Admission runs with
`box.DeferProjectBuild`, and the supervisor asks `box.EditorBoxReadiness` itself: a project file that
does not load (`*BrokenProjectFile`), a project build miss (`*ProjectBuildRequired`), services with no
rules (`*ServicesNeedRules`), rules nobody approved (`*networkstate.PendingApproval`), or another box
holding the service launch lock (`*box.ServiceLaunchBusy`). Any other error is a failure to look, and
proves nothing.

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
the window once validated. A real box in the project still makes a first approval wait, and says so.

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
`errACPBoxWaiting`, and the warm-off check at startup skips a blocked session, or the pool stays off for
its whole life. The fan-out comes when the box moved onto has answered (`confirmMove`), not when the wait
clears: every box takes `LockServiceLaunch` exclusively to start services and then holds the shared side
for its life, so a warm box that got there first left the session's own box waiting on the lock with its
messages unsent, for as long as the warm box stayed parked. For the same reason a plain connect in a
project with a Compose file has no `leadAnswerGrace` timer (`acpFanOut.arm`): the lead answers first, however
long its services take, and a lead that never answers leaves the pool empty there.

Trap: a later box of a session must not start services at all. `up --no-start --force-recreate` runs in
preparation and again at launch on the session's own Compose project, with the exclusive lock dropped in
between. That gap explains `db` going away under a running box, seen once in 2026-11 and read from the code, not caught
live: a warm box can recreate the services stopped before the lead takes its shared side, then wait at launch,
leaving `db` created and not started. And with the lead holding the shared side, a warm box could never get the exclusive lock, so it
would never park and a switch to it would race the other warm boxes for the lock. So a box of a supervised
session whose services already run the same approved bytes and closure (`coop.service-definition`, checked
under the shared lock by `joinSessionServices`) joins them: no Compose, no exclusive lock, no bind source
opened, which is all that lock guards. Anything else takes the full path, and its wait. If only the session's
own parked boxes keep its own box out (a service stopped, say), the child says so on the launch-wait pipe
(`OnOwnBoxesHoldLaunch`) and the supervisor drains the pool, since those boxes never stop by themselves. A
parked box never asks this, or they would stop each other. After any cold box in a project with services the pool
stays shut until it answers (`awaitAnswer`), or the refill would race it for the lock just freed.

An open session joins too (`joinOpenSessionServices`). It has no approval, so it matches what it started: a
start under a supervisor labels every service with the file's bytes and service list (`markServiceDefinitionKey`,
never a loop's, where editing a comment would then recreate everything at the next `up`). Every service has to be
up and healthy with its published ports, or a declared one-shot that exited 0 (`declaredOneShots`), or the box takes
the full path. Exit 0 alone proves nothing: `redis SHUTDOWN` or a `docker stop` ends the same way. So a step counts only
when another service waits for it with `service_completed_successfully` or it says `restart: "no"`; an undeclared
standalone step waits behind the running box like any stopped service.
So after an edit to the file a refill cannot join and waits behind the running box, and nothing recreates a service
under it. A switch to that waiting box has no `OnOwnBoxesHoldLaunch`, since it was started parked, so `warmTaken` sees
it has not started (`Child.Launched`, an open child's cidfile) and drains its neighbours itself, and either way the
refill waits for the answer. Measured: 60 s parked behind the lead, then live 17 s after the switch, with the services
recreated only once the other boxes had stopped. A filtered session still takes only `heedLaunchWait` for this.

Trap: `docker inspect --format` runs over decoded JSON and fails on a missing key, and a container with no
healthcheck has neither `Config.Healthcheck` nor `State.Health`. `{{if .Config.Healthcheck}}` made
`ObserveServiceContainer` fail for any such service on Docker 29.8; `index` is what returns nothing instead.

Fork ACP shares the wait, but its hooks have no start notice, so a thread hears about it only when it
sends a message. See [[restricted-project-builds-are-explicit]].

Trap: the lock's other waiter is the session's own next box. A box that starts the project's services
takes the lock exclusively, so it waits for every other running box in the project, for as long as they
run, and it answers nothing meanwhile: with no thread open the editor shows only a spinner, and a
replacement would end the session once `replayStartupGrace` ran out. So readiness also reports
`*box.ServiceLaunchBusy`, when this box would wait now, and that holds in every network mode: an open or
offline session has this wait and no other (`EditorUnfilteredBoxReadiness`, `filtered` false). Every open
thread hears it at once (`announce`, `Restarter.TellOpen`), since it can begin with all of them idle. The
lock can also be taken just after the check, so a real child says it waits on the descriptor named by
`COOP_ACP_LAUNCH_WAIT_FD` (never stdout, the wire, or stderr, which carries what a box writes), and
`heedLaunchWait` puts the session on a holding child (`Restarter.ReplaceUnstarted`). A parked box's word is
nobody's news until the session takes that box. Only a session's own parked boxes holding the lock is not
a wait to report: nobody can close them. `EditorBoxStamp` watches the lock, so the session moves when it
comes free. A holding child in any mode drops the mount window once validated, or it would hold the lock
it waits for. The terminal and loop side of this is `box.lockServiceLaunch`: try for two seconds, say what
it waits for, wait, say it ended. Every caller goes through it, and `forkspace.LockServiceLaunch` is only
for code nobody is waiting on.

A services refusal comes in parts (`box.ServicesNeedRules`), since a thread cannot show the terminal's text
whole: Zed does not wrap a code block. `acpBoxWaitText` shows the reason as text and only `coop approve` as a
block, and the terminal and the agent log keep `Error()`. `coop approve` has nothing to approve in that state, so
it asks the same check (`box.ProjectServicesRefusal`) and offers the rules (`offerServiceRules`). Yes writes first
and reviews after, so `ProjectNetworkApproval.AddsOnly` checks the review against the rules shown and the
definitions pinned before the question: a box can edit either while the question waits. A service with no stated
port is left out of `Offer()`; with none to offer it asks nothing and prints the paste text. Any one approved
service rule ends the refusal, so a service left out does not keep the box blocked.

Readiness asks about those five things only, on purpose, and about the file first: read as a file that
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
and offline sessions wait only for the lock and keep the old rule for a box that fails. An `initialize` the first child never answered
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

Trap: who holds the lock. flock cannot say, and a record proves nothing: `BeginExecution` publishes one before
any lock is taken, so a box that waits and a holding child both have one, in this session or another. Counting
records let two sessions wait on each other: A runs with a parked box, B needs the exclusive side and goes on hold,
A switches cold and is told to wait for B's holding child, and A's parked box never yields because `own` is false.
So a box that holds the lock says so (`noteLaunchHeld`, an empty file in `launch-holders/` beside the records, named
by the record), and `serviceLaunchHolders` counts only those. It is a file and not a field because the supervisor
rewrites the record from another process to change its role, and a running older build refuses a record it cannot
fully read. A box of an older build holds without saying so, so it reads as "another Coop command" until it stops.

Readiness asks what the launch will ask. A cold box that would join needs only the shared side, so
`editorServiceLaunchBusy` runs the join's own check (`openSessionServicesRunning`, `sessionServicesRunning`) when the
shared side is free now. It is a forecast: a box that turns out not to join says so on the launch-wait pipe. Only
a spawn's readiness asks; `heedLaunchWait` already knows its child did not join, so it uses `EditorServiceLaunchBusy`.

Two more rules keep a wait from lasting for good. A session on hold for the lock drains its parked boxes
(`settle`): a spawn would refuse them anyway, and two sessions that each kept theirs would hold each other out. A
wait re-reads the holders every `serviceLaunchWaitNotice` for as long as it lasts: parked boxes can stop and another
session's box get the lock first, and a verdict read once left that waiter with no message, no first-answer shield
and no second yield. `EditorBoxStamp` carries the holders and whether the exclusive side is held, so a session on
hold looks again within one poll, a thread hears who it waits for now, and a box that could join goes on once
whoever was starting services has finished.

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
- 2026-11-04 — `coop approve` offers the missing service rules, and the thread sends the person there instead of
  showing rules to paste. Verified with a real session moving on after the approval.
- 2026-11-04 — exit 0 no longer makes any service a finished step: the file has to declare it. A broken file's reason
  is one escaped line of text, since an editor does not wrap a code block.
- 2026-11-04 — two sessions in one project no longer wait on each other: readiness asks whether a box would join,
  only boxes that say they hold the lock count, a session on hold drains its parked boxes, and a wait re-reads who
  holds it. Verified with two real open `coop acp` sessions on fake API keys in a private config folder, no prompt:
  before, A's cold switch went on hold and neither moved for 120 s; now it runs in 2 s, and in 3 s when the compose
  file changed (its parked boxes stop and it restarts the services), and B moves on when A closes.
- 2026-11-04 — open sessions join their own services too, and a warm box a switch takes no longer waits behind its
  neighbours. Verified with a real open `coop acp` on fake API keys in a private config folder, no prompt: a warm
  switch was live in 0.1 s with every service container unchanged, the refill joined, and the same after an edit.
- 2026-11-04 — the session's own box starts first after a wait and at a plain connect, and later boxes join
  its services. Verified with driver runs (7 clean in a row, then one refused over a changed grok account) and a
  plain connect then a warm switch: `db` kept its container and stayed reachable from every box.
- 2026-11-04 — a box that waits for the service launch lock says so in a terminal, a loop and a thread, and
  open and offline sessions wait on a holding child too, as `run.go` takes the same lock for them. Verified
  on real Docker: open and filtered terminals and an open editor thread beside a running box.
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
