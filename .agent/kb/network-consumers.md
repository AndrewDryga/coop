---
name: network-consumers
description: how the loop, direct/ACP runs and remote sessions consume one frozen network capture per box, and which surface reads which evidence
subsystem: networking
sources: [internal/networkstate/execution.go, internal/networkreport/report.go, internal/box/run.go, internal/box/launch_sections.go, internal/box/network_summary.go, internal/box/network_admission.go, internal/cli/launch_box.go, internal/loop/network.go, internal/loop/host.go, internal/cli/commands.go, internal/cli/acp_cmd.go, internal/acpproxy/proxy.go, internal/acpctl/warm.go, internal/cli/fork_cmd.go, internal/cli/fork_acp.go, internal/cli/boxsweep.go, internal/forkctl/meta.go, internal/forkctl/merge.go, internal/forkctl/status.go, internal/forkctl/ls.go, internal/cli/net_cmd.go, internal/cli/net_diagnostic.go, internal/box/network_session.go, internal/box/network_recover.go, internal/sessionsvc/network.go, internal/sessionsvc/service.go, internal/sessionsvc/acp.go, internal/sessionsvc/http.go, internal/networkstate/admission.go, internal/networkstate/job.go, internal/networkstate/execution_retention.go, internal/networkstate/snapshot_retention.go, internal/networkstate/qualification.go, internal/box/network_setup.go, internal/session/schema.go, internal/workerproto/protocol.go, internal/cli/acp_refresh.go, internal/networkgateway/resolver.go, internal/networkgateway/collector.go, internal/networkstate/inspection.go, internal/box/project_preparation.go, internal/box/derived_image.go]
updated: 2026-11-06
---

Admission happens ONCE per unit of work and the resulting `*box.CapturedEgress` is passed down; a
box never admits for itself and its capture never changes. The one consumer that admits again is a
filtered `coop acp` supervisor after an approval change, and it replaces boxes rather than changing
one (below). See [[restricted-networking]] for what admission decides.

**Direct runs** admit in the CLI and hand the capture to `box.Run` on the same host process
(`cli/commands.go`). Interactive forks and remote/restricted fork ACP children do the same in
`cli/fork_cmd.go`. A direct local fork ACP editor admits once in its outer process; its pinned
child re-proves the capture reference and exact credential bindings without re-admitting
(`cli/fork_acp.go`, `cli/fork_cmd.go`). Open/offline children inherit the resolved posture.
Fork review/merge gates use the trusted parent policy in `forkctl/merge.go`. A fork loop re-renders
the operator's flags in their exact spelling so the detached worker admits what its foreground twin
would have (`cli/network_flags.go`). Before detaching, the parent fully admits the actual fork
workspace (`preflightForkLoop`, `cli/fork_cmd.go`) on a copy of the configuration and closes the
capture. It refuses what the worker would, network requests and project build files no person
approved; it rebuilds approved build files and may qualify the host. The read-only
`box.CheckNetworkApproval` it replaced never saw a build miss, so a detached start reported success
while its worker stopped later, holding a task. Before every canonical assignment the worker checks
the workspace's current policy with that read-only preview, and its build files with
`box.CheckProjectBuild` (`box/project_preparation.go`), since either may change after the parent's
check or an earlier task. That check is read-only too: it resolves the mode through the same
preview, refuses only files no person approved, and lets approved files through for the pass's own
admission to rebuild, so the scheduler tests that fake the pass (`app.forkLoopRun`) need no runtime
that runs filtered boxes; with no network state on the host it refuses without asking Docker. Every
build miss names the command that builds it for this process's person: `withBuildCommand` sets
`ProjectBuildRequired.Command` to `BuildCommand(approvingProject(cfg), folder)`, the `ApproveCommand`
rule, in `AdmitNetwork`, `EditorBoxReadiness` and the check, and the launch's last refusal in
`filteredProjectImage` names it too, so a fork loop, an interactive fork and a fork editor session
started from the parent all say `COOP_REPO=<fork> coop build --egress filtered`. `coop build` builds
the checkout it runs in. The capture never crosses to the worker, and the ordinary loop still fully
admits each run before launching boxes, so neither check is a launch grant or a policy identity
transfer. `box.Run` reloads project policy before its final capture check, so a future caller that
misses admission fails before mounts or runtime execution.

**An ACP session** admits in the outer supervisor, before the first child (`cli/acp_cmd.go`
`cmdACP`): its scope unions every provider the toolbar could switch to, so a provider switch reuses
that capture instead of meeting a mid-session denial. That scope is frozen, with the rest of the
startup admission's spec and options. When `coop approve` or `coop net forget` advances the host's
approval generation, a filtered `coop acp` supervisor (not fork ACP) re-runs that exact admission —
existing host setup only — once every thread is idle, or at once when its factory starts a box anyway
(a respawn, a switch), and replaces its boxes under the new capture instead of changing a running
one (`cli/acp_refresh.go`; see [[restricted-networking]]). A project whose request nobody approved
connects on a capture holding none of it, which serves only children that hold prompts until a person
approves, and any such child runs under rules like that, with none of the project's, so its fingerprint
differs from the session's ([[acp-box-wait]]). Each spawned child receives a
per-child REFERENCE through `COOP_NETWORK_CAPTURE` (`acpChildCapture` in `cli/acp_cmd.go`) — the same envelope a
remote session uses — and an inherited one is stripped from the child environment. A filtered ACP
child gets no `--cidfile` (the gateway engine owns its container ids) and a SIGTERM-cancelable
context, and the supervisor's stop (`stopACPChild`) sends SIGTERM and waits up to 30 s
(`acpFilteredStopGrace`) before SIGKILL, so an editor closing the session lets every box — active and
parked, together — tear its own gateway down. Until 2026-09-19 it sent SIGKILL at once: every closed
filtered editor session stranded each box's guard, controller and volumes until the next filtered
launch settled them.

**The loop** admits once, right after the review ladders are built, and every box it launches —
pre-flight, work, review, verify, debug shell — attaches that same capture through one `runBox` seam
(`Control.runBox` in loop/host.go). The admission spec unions every rung of every stage's ladder
plus the named peers (`networkAdmissionSpec` in loop/network.go): a provider that rotates in
mid-drain must already be in the frozen policy, or it would meet a denial instead of a refusal at
launch. Refusals accumulate during an iteration and print between iterations, never over the live
bar, plus one ranked closing summary (`networkLog.finishIteration` and `networkLog.summary` in
loop/network.go).

**Remote sessions** receive one canonical controller-authored job at create. The job supplies
network mode and explicit rules; no local policy registry, project requests or remembered operator
approval participates. `box.AdmitControllerJobNetwork` freezes the rules plus provider dependencies
in an owner-private snapshot scoped to job digest and session id (`networkstate.CaptureJob`). The
session row stores mode, fingerprint and qualification; cold turn, warm child, resume and replay
reuse that exact snapshot. Repository settings cannot change its reach. Then:

- The daemon is the child's HOST parent and scrubs every `COOP_*` from the environment it builds, so
  `COOP_NETWORK_CAPTURE` (`box.SessionNetworkCaptureEnv`) can only be set there
  (`networkChildEnvironment` in sessionsvc/network.go). It carries a *reference* — project,
  fingerprint, qualification, session, attempt — not authority.
- The child proves it: `OpenExisting` (never `Open`, so a child cannot create an owner key) plus
  `LoadJobSnapshot` of the exact job digest, session id and fingerprint. A forged or cross-job
  reference produces no snapshot and no launch. Local ACP continues to prove a project snapshot.
- After the child exits the daemon checks every run registered against the session against the
  immutable row's fingerprint and FAILS the turn on a mismatch (`verifySessionNetworkRuns`). An
  inventory it could not read WHOLE fails the turn too (`sessionTurnRunner.sessionNetworkOutcome`):
  the record it skipped is exactly where a mismatching run would hide.
- A filtered child owns a gateway, its volumes and its receipt, so it gets a 30 s stop window
  instead of the ordinary 250 ms (`sessionACPFilteredStopGrace` in sessionsvc/acp.go). Killing it
  fast leaves two running containers and an open receipt until recovery settles them — the next
  filtered launch does, child or not, through `runBox` (`cli/boxsweep.go`).

The API adds four reads under `GET /v1/sessions/{id}/network`: the live summary, `/connections`
(the newest run's bounded rows), `/explanations/{event}` (one retained refusal) and `/receipt`
(aggregate, final once closed) (`sessionHTTPHandler.serveSessionPath` in sessionsvc/http.go), a
`network` event on the session stream whose payload is bounded by construction (refusals grouped and
capped, alerts capped),
`SessionDTO.network`, and the same reads through the generic worker `api_request` transport.
There is no second worker command vocabulary. None of these reads grants authority: the saved job
controls whether destination names are exported, and the daemon applies that projection.

Summary and connection reads require a complete execution inventory; an unreadable record returns
`network_unavailable`, never an invented "no run yet". Explanation lookup distinguishes missing
or retained-away events from malformed evidence. A directly verified event owned by this session
can still be returned when another record is unreadable; an unsuccessful search cannot claim
`event_not_retained` from a partial inventory. Destination projection applies in either case.

`coop net` is the host operator's read surface, grouped by the job a person arrives with (the order
of `netCommands` in cli/net_cmd.go): ACCESS (what a new run may reach), RUNS (what recorded runs
did), REPAIR (what coop normally does itself). Its verbs differ in what they read:

| verb | reads |
| --- | --- |
| bare `coop net` | this project's mode and its cause, the approved project rules, and — only when the file and the approval differ — the same access change `approve` would show; creates no owner key. Setup health and history cost no line: a launch sets the host up itself |
| top-level `coop approve` | TTY only; the exact snapshot of `.agent/project.yaml` (mode, rules, service digests) diffed against the remembered approval, digest-fenced. Nothing pending → `No approval needed: …`, nothing written, exit 0, and when a filtered box would still refuse on services with no rules (`box.ProjectServicesRefusal`, the check the editor's wait asks) that refusal as a caution, since the line alone reads as if nothing is wrong. Only explicit `--case-exposure` also reviews mount-fallback file access; ordinary network approval never computes or saves that permission. |
| `check <url-or-host>` | with no `--run`, the approved project rules plus each agent's provider bundle (`cli/net_diagnostic.go`, `netCurrentCheck`); with `--run`, that run's captured policy evaluated hypothetically. Never sends a packet. The exit status is the answer (`netCheckExit`): 0 allowed, 3 blocked or pending, kept apart from 1 (could not check) and 2 (input), with `--json` and `--run` alike |
| `forget` | one of three mutating verbs: writes a withdrawal marker, THEN removes one project's approval (`networkstate/approval_withdrawal.go`). While the marker stands every ordinary launch of that project but an offline one is pending — the built-in default cannot reopen it — and only `approve` clears it |
| `runs` · `inspect` · `watch` · `export` | the keyless `Evidence` handle over retained execution records — no runtime, no DNS, no key. Any unique prefix of a run id resolves (`netResolveRun`); an ambiguous one is refused with the prefixes that settle it |
| `blocked <host>` | the newest RETAINED denial of that host in this project's runs (or in `--run`), grouped by boundary/reason/port; the exact event id still works with `--run`. A record that proves name, transport and port drafts its exact rule; one whose every cause is a DNS lookup no rule named drafts TLS/443 labelled as the likely fix (`netLikelyRule`), since a lookup records no port; any other port-less record drafts nothing. A direct connection to an address the run's resolver answered for exactly one name carries that name beside its peer (`Resolver.NameFor`, read by the collector when it records the attempt, which for IPv4 is the filter's own `refused4` record — see [[network-gateway]]), so it is found by host and shown with `address <ip>`; one on a port other than 80 drafts TLS on its port as the likely fix, since the record proves the name and port but not TLS |
| `setup` | mutating: builds the image pair and records the host qualification — the same work a filtered launch performs itself when this host has no current proof, so it is a prepare-ahead/recheck verb, not a prerequisite |
| `recover [<run>]` | settles a run whose supervisor died — exact-owned removal, then a final `supervisor_lost` receipt (`RecoverNetworkRuns` in `box/network_recover.go`). The same pass runs from the orphan sweep at loop/fork start AND from `coop net inspect` before it reports a cleanup as incomplete (`cli/net_cmd.go`, `netSettleCleanup`). The unscoped pass also removes finished records past `networkstate.ExecutionRetention` (90 days) whose cleanup is complete, re-read under the run's lock; a record still owed cleanup, unreadable or a link is never removed, so the listing stays small without a scan cap. A record it cannot read is skipped and counted (`UnreadableNetworkRuns`, returned beside the recoveries): the other runs still settle, and `coop net recover` names each unreadable record's file and exits 1. A file no run is named after counts once, on the first page of the listing. An unnamed `coop net recover` lists only runs it cleaned or could not; a `Live` result (its own supervisor, here or elsewhere) is that process's job and is printed only when the run was named |

The execution census validates records with at most four concurrent readers, retains only
summaries/errors, and joins every reader before reducing results in cursor order. It caches no
record or authority: recovery still rereads pending runs, and retention rereads under its lock.

**Policy snapshots** (`snapshot-<fingerprint>.json`, one per distinct capture) go last in the same
unscoped pass, and only from a listing of every run record read whole: an unread record may be the
run that names one (`networkstate/snapshot_retention.go`, `RemoveUnusedSnapshots`). Only a snapshot a
project admission CREATED is removable: `saveSnapshot` writes `admitted-<fingerprint>.json` beside it
on that exclusive create, and never for `CaptureJob`. A session row names its job snapshot forever,
rows are never deleted, and they live in session stores under any `--state` the pass cannot find, so
job snapshots and every snapshot from before markers (possibly a pre-job session's) stay; whoever
adds session deletion must remove the job snapshot in that change. The one pre-marker snapshot that
goes is a `coop net setup` check's: only the check's own run names or loads it, nothing else admits
a rule with `networkstate.SetupOrigin()`, and a crashed setup is rerun in a new scratch project,
never resumed (`QualificationSmoke`); `SetupNetwork` also deletes that scratch project when it
returns, though not when it is killed. The smoke admits its one rule with that origin
(`box/network_setup.go`, `admitSetupSmoke`; marked like any admission), so it goes like any marked
snapshot. A check snapshot an earlier coop wrote without a marker is no longer marked when its run
record expires: it stays on disk, unused. A marked snapshot stays while a
listed run names it (it goes one pass after its last record) or while its marker's mtime is inside
the 90 days. The mtime is the last use: re-admitting an unchanged policy (`saveSnapshot`'s EEXIST
branch) and loading a marked snapshot (`loadSnapshotScope`) refresh it under `snapshot-lock`, where
the removal also looks again; a load of an unmarked snapshot takes no lock and writes nothing. The
removal unlinks the snapshot before its marker, so a load that finds no marker can trust it was
never marked, and a removal cut short leaves a marker the next pass clears. Trap: a launch settles
AFTER it admits (`runBox`), so without the re-admission refresh a project reopened after 90 days
lost the snapshot it had just admitted. Accepted limit: a process holding a capture that starts no
box for 90 days after its last run ended (an idle `coop acp` supervisor) can find its snapshot gone;
its next child is refused with "start the session again" (`CapturedEgressFromReference`).

`export` is redacted by default because it is the shareable artifact (`--include-addresses` puts the remote
hostnames and IP addresses back); `inspect`/`check`/`blocked`/
`watch` show names, as the local operator view. An event that has aged out of a run's bounded
ring reports `event_not_retained` — which is not proof the id ever existed.

The human run projection lives in `internal/networkreport` (`WriteRun`), BELOW both `cli` and
`box`, because both render it: standalone `coop net inspect` on stdout with no prefix, and an
interactive box on stderr after cleanup sealed its receipt, under `Networking stats:`
(`View.Inline`; `box/network_summary.go`, `printRun`, fed from the record the supervisor already
holds through `networkstate.InspectExecution` — no reopen). The inline form puts each
destination's own totals on its row instead of listing its remote addresses; everything from the
exceptions down is the same body. It is destination-first and
exception-only: the `Allowed` aggregate, then every PROVEN workload destination — only
`NameSource == "sni"` rows, grouped by (name, port, transport) then by peer, bytes UNKNOWN when
any member is unmeasured; a row in state `failed` is "N attempts failed — <reason>" and
`connecting` is in flight, neither a connection — then, as their own blocks, `Other observed
endpoints` (`unattributed*` rows with their reason) and `Raw traffic` (per-rule kernel counters
named by the rule from the run's own policy, `Inspection.AddressRules`, with the keyed id only when
that copy is gone; no host), then only what went wrong (blocked, alerts, evidence gaps, unhealthy enforcement,
cleanup still owed), then `Full details: … --json`. Coop's own resolver socket
(`trusted-maintenance`) and an ownerless closing kernel block (`socket-inventory`) are explained
internals and cost no line. A terminal run's `stopped` gateway is normal teardown, not a warning;
a run whose supervisor is still alive (recovery reported `Live`) is `● Live`, not a lost record.
The old refusal-only end-of-box summary is gone; `box.NetworkReport` survives only as the bounded
DATA the loop (`OnNetworkReport`, between-iteration lines, closing summary pointing at `coop net
runs`) and the session daemon fold into their own output. The projection prints only for a box that
reached its main process: a launch that failed earlier has no traffic to report.

An interactive launch (`!Batch && !Quiet && !ForceNoTTY`, `box/launch_sections.go`) is narrated in
bold unprefixed sections before agent output — `Protecting secrets` (the exact shadow count),
`Connecting account`/`Connecting accounts` (`launchAccounts`: one row per provider in the credential
scope — display name, the account the box mounts, `API key protected` for a route of the run's
broker plan else `Signed in` — plus the one-line explanation, once, only when a key is protected;
no section without an account), `Configuring network access` (filtered: one row per selected provider's endpoints from
`policy.Dependencies` and each agent's `Vendor()`, `Applied N approved network rules` counted over
`project`/`operator` origins, the MCP servers over `mcp` origins, an unrecognized origin counted as "other", THEN
`✓ Everything else blocked`; open/offline: one `⚠` row under the same heading), `Starting <agent>`
— and `Checking the Coop box` first: in `box.Run` the remaining image nudges (age, a project
Dockerfile that drifted) as `⚠` rows, and in the cli (`cli/launch_box.go`, after admission and only
for a run that will use the repo's image — a filtered box runs the qualified client image) the
automatic `box.Build` when `BaseImageSkew` reports a definition mismatch, never for an age nudge
or an unstamped image. A failure before the main process is rendered once as
a nested `ui.Fail` under its
section, AFTER the deferred cleanup has joined its own error into the reason, and comes back as
`ui.Reported(err)`, which `cli.Main` does not print again; a cancellation the stop line named is
marked the same way when teardown added nothing. Teardown prints exactly one
`coop: stopping the box — …` line for a box that reached its main process (`filtered.started()`, the
daemon's StartedAt evidence; the open path's plain client exit): the recorded host signal
(`hostInterrupt`, which replaced the anonymous `signal.NotifyContext`) or the exit status as a
number — never Ctrl-C inferred from 130.

## Changelog
- 2026-11-06 — `blocked <host>` drafts TLS on the port of a direct connection to a resolved name (not
  port 80), now that the filter records those refusals (task
  2026-11-06-record-direct-and-unlisted-port-blocks-so-coop-n).
- 2026-11-06 — the fork worker checks its build files before each assignment (`box.CheckProjectBuild`),
  read-only, and every build miss names `COOP_REPO=<fork> coop build --egress filtered` when the
  person's coop build would build another checkout; only the detached start's refusal named it. ACP
  thread texts take the command from the miss. `preflightForkLoop` lost its own "Build it:" row (task
  2026-11-06-close-the-last-gaps-in-carrying-fork-decisions-a).
- 2026-11-06 — a detached fork loop's start check is a full admission, so build files no person
  approved stop it in the terminal, before a worker starts, with the fork's own `coop build`
  command (task 2026-11-05-build-a-fork-s-box-image-on-its-first-loop-inste). Approved files, such
  as those a new fork snapshots from its parent, rebuild there. The worker's per-assignment check
  stays the read-only preview.
- 2026-11-05 — detached fork loops preview approval against their actual workspace before start and
  before every canonical assignment; the ordinary loop still admits each assigned task's run.
- 2026-11-04 — removed `markSetupCheck`: a setup-check snapshot written before markers existed is
  left on disk (task 2026-11-04-drop-the-old-network-record-readers). A version 1 run record is no
  longer read (`Store.execution` names it); the project it names is refused by
  `networkstate.projectIdentity`.
- 2026-11-04 — removed `networkstate.Store.Resolve` (see the 2026-09-10 entry below) and
  `Store.CheckRequests`, which nothing called. `Admit` and `PreviewAdmission` are the ways in, and the
  tests of those rules now run through them.
- 2026-11-04 — `coop net approve` no longer exists, so the `approve` row names only the top-level command.
- 2026-11-04 — `coop approve` with nothing to approve also shows a services refusal a box would still get.
- 2026-11-03 — an editor session connects on a holding capture while its project's request waits.
- 2026-11-03 — `coop net blocked` drafts TLS/443 as the labelled likely fix for a lookup-only
  refusal and finds a direct connection by the name the run resolved for its address; `net check`
  exits 3 when blocked or pending; an unnamed `net recover` lists only what it cleaned or could not;
  raw traffic rows are named by rule (task 2026-11-03-make-coop-net-blocked-and-net-check-answer-the-c).
- 2026-11-03 — rechecked approval dispatch and file review: ordinary network approval does not
  calculate or grant mount exposure; explicit `--case-exposure` retains that reviewed decision.
- 2026-11-03 — the built-in default is filtered (task
  2026-11-03-choose-the-default-network-for-boxes-outside-a-c); the forget row now says what the
  barrier leaves running (offline launches only) instead of naming an open default.
- 2026-11-02 — bounded parallel execution-record reads preserve the complete census, deterministic
  summaries and unreadable reports; verified fresh recovery and locked retention reads.
- 2026-10-28 — distinguished unavailable/incomplete evidence from absence in certification and remote reads; checked real malformed retained records, session ownership, and disclosure fixtures.
- 2026-10-19 — an unreadable run record no longer stops the unscoped recovery pass: it is skipped
  and counted, and snapshot retention still waits for a listing read whole (task
  2026-10-15-simplify-network-record-keys-and-setup-records).
- 2026-10-17 — replaced the line-number citations with `Control.runBox`, `networkAdmissionSpec`,
  `networkLog.finishIteration`/`summary`, `box.SessionNetworkCaptureEnv`, `networkChildEnvironment`,
  `verifySessionNetworkRuns`, `sessionNetworkOutcome`, `sessionACPFilteredStopGrace`,
  `serveSessionPath` and `netCommands`, each re-verified at HEAD; the `approve` row now says it is
  the top-level `coop approve` (moved in 4ab701a2; `coop net approve` returns `approveMovedError`).
  The 2026-09-26 entry now names the functions it removed, which older entries still mention.
- 2026-10-17 — the read-only and bare launches (`restricted.go`) are gone, so the ordinary launch
  narrates every section.
- 2026-10-16 — policy snapshots nothing uses are removed by the same pass (paragraph under the
  `coop net` table): only admission-created snapshots carry the `admitted-` marker that makes them
  removable, the census needs every record read, and re-admission or a load of a marked snapshot
  refreshes the marker under `snapshot-lock`.
- 2026-10-15 — finished run records are kept 90 days (audit decision 7): the settle pass removes
  them, the listing reads the whole directory (no 10,000-entry cap), and a record removed between
  the listing and the read counts as gone, not unreadable. Replaced a line reference with the name.
- 2026-10-15 — a filtered `coop acp` session now re-admits after an approval change (the frozen
  startup spec, between turns or when a box starts anyway) and replaces its boxes; per-child
  references are unchanged. Fixed the "no consumer admits twice" claim and a stale line reference.
- 2026-09-26 — removed the retired policy-resolution path (`sessionsvc.ResolvePolicyNetwork`,
  `ResolvePolicyNetworkSnapshot`, `Service.PolicyNetworks`, `coop sessions policies`) and its unused
  box adapter (`box.ResolveSessionNetworkSnapshot`); reverified
  controller admission, job-scoped snapshots, child proofs and generic API network reads. Local
  project/ACP/loop admission is unchanged.
- 2026-09-25 — reverified local fork ACP after the live editor test: its new fixed-target supervisor
  admits once, hands each child a proved capture reference plus account bindings, and leaves the
  remote/restricted direct adapter path unchanged.
- 2026-09-19 — added the `Connecting account(s)` section; the network section lost its broker row.
  Restricted modes narrate the sections too (the old shadow-line claim was stale).
- 2026-09-19 — the ACP supervisor's child stop is graceful for filtered children (was SIGKILL, which
  stranded every gateway on editor close); the session daemon closes warm sessions per workspace
  concurrently
- 2026-09-18 — direct, ACP and fork ACP launches go through `runBox`, and fork and session review
  gates call `forkctl.Host.SettleFilteredRuns`, so interrupted filtered runs are settled before a
  filtered box starts; the stop-window row above no longer claims a killed child's receipt cannot
  be finalized.
- 2026-09-14 — interactive forks, local fork ACP and fork review/merge gates now use the shared
  admission path; the box boundary rechecks project policy before requiring a capture.
- 2026-09-11 — the CLI design landed: `explain` became `blocked`, `export --include-destinations`
  became `--include-addresses`, bare `coop net` lost its project header and keeps the YAML
  explanation only where YAML selected the mode, `runs` shows 25 with a header row, the box's inline
  summary is `Networking stats:` from the same renderer (`View.Inline`), the launch section is
  `Configuring network access` / `Applied N approved network rules`, and `forget` now leaves a
  withdrawal marker that fails closed. Every view is pinned in `internal/cli/testdata/approved`.
- 2026-09-11 — `box.ResolveSessionNetworkSnapshot` returns the COMPILED snapshot beside the mode,
  so a reader can be shown a session policy's actual grants (provider bundles, then the project's
  approved rules) instead of the policy YAML, which omits whatever the project contributed.
  `sessionsvc.ResolvePolicyNetworkSnapshot` carries it to `coop sessions policies`' human view
  (`internal/cli/session_policies_view.go`); `ResolvePolicyNetwork` and the `policy_networks` JSON
  are byte-identical to before. A policy this host cannot resolve becomes a visible issue in that
  view, never a fabricated permission summary.
- 2026-09-10 — `approve` reviews the exact project-file snapshot with no `--mode` and skips a no-op; the
  pending check it shares with `coop init`, bare `coop net` and every `AdmitNetwork` launch lives in
  `networkstate` (`pendingApproval`); a filtered launch qualifies the host itself. Rows above updated.
- 2026-09-10 — the run projection moved to `internal/networkreport` and the interactive box renders it inline after `coop: stopping the box — …`; launch sections, the failure/`ui.Reported` contract and the `hostInterrupt` recorder recorded. Re-verified against the sources above.
- 2026-09-10 — the `coop net` family regrouped into ACCESS/RUNS/REPAIR (`runs` replaces `ls`, `check` replaces `why`, `export` replaces `receipt`, `explain` takes a host); `inspect` renders the destination-first exception-only projection and makes one bounded recovery attempt before reporting cleanup. Re-verified the consumer facts above against their sources.
- 2026-09-10 — resolving moved from load-time-only to every request as well: what a daemon advertises is what a create would accept now (`service.go` PolicyNetworks).
- 2026-09-10 — a session policy's RESOLVED network fingerprint is published at load
  (`/v1/capabilities`, `coop sessions policies`) and pinned by a create through
  `expected_network_fingerprint`; `networkstate.Store.Resolve` compiles what `Admit` would without
  publishing, and a policy that cannot be resolved is refused instead of served unfenced.
- 2026-09-10 — S7c: `coop acp --egress filtered` admits in the supervisor and hands each child a
  reference; incomplete session evidence fails the turn; `/network/connections` and
  `/network/explanations/{event}` (plus their worker commands) close the remote parity gap;
  `coop net recover` settles interrupted runs.
- 2026-09-10 — created from the shipped consumers (S3–S6, 4739bb1…f3e1d96), replacing the WIP
  session/ACP/diagnostics cards. Verified against the sources above.
