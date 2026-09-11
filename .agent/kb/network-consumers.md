---
name: network-consumers
description: how the loop, direct/ACP runs and remote sessions consume one frozen network capture, and which surface reads which evidence
subsystem: networking
sources: [internal/networkreport/report.go, internal/box/launch_sections.go, internal/box/network_summary.go, internal/cli/launch_box.go, internal/loop/network.go, internal/loop/host.go, internal/cli/commands.go, internal/cli/acp_cmd.go, internal/cli/net_cmd.go, internal/cli/net_diagnostic.go, internal/box/network_session.go, internal/box/network_recover.go, internal/cli/session_policies_view.go, internal/sessionsvc/network.go, internal/sessionsvc/service.go, internal/sessionsvc/acp.go, internal/sessionsvc/http.go, internal/networkstate/admission.go, internal/session/schema.go, internal/workerproto/protocol.go]
updated: 2026-09-11
---

Admission happens ONCE per unit of work and the resulting `*box.CapturedEgress` is passed down; no
consumer admits twice. See [[restricted-networking]] for what admission decides.

**Direct runs** admit in the CLI and hand the capture to `box.Run` on the same host process
(`cli/commands.go:203`). A fork loop re-renders the operator's flags in their exact spelling so the
detached worker admits what its foreground twin would have (`cli/network_flags.go:84`).

**An ACP session** admits ONCE, in the outer supervisor, before the first child
(`cli/acp_cmd.go:204`): its scope unions every provider the toolbar could switch to, so a provider
switch reuses that capture instead of meeting a mid-session denial. Each spawned child receives a
per-child REFERENCE through `COOP_NETWORK_CAPTURE` (`cli/acp_cmd.go:493`) — the same envelope a
remote session uses — and an inherited one is stripped from the child environment. A filtered ACP
child gets no `--cidfile` (the gateway engine owns its container ids) and a SIGTERM-cancelable
context, so an editor closing the session cleans up instead of stranding a gateway.

**The loop** admits once, right after the review ladders are built, and every box it launches —
pre-flight, work, review, verify, debug shell — attaches that same capture through one `runBox`
seam (`loop/host.go:116`). The admission spec unions every rung of every stage's ladder plus the
named peers (`loop/network.go:29`): a provider that rotates in mid-drain must already be in the
frozen policy, or it would meet a denial instead of a refusal at launch. Refusals accumulate during
an iteration and print between iterations, never over the live bar, plus one ranked closing summary
(`loop/network.go:91`, `:158`).

A **session policy** is the one consumer that publishes its reach BEFORE anyone asks for a launch.
The daemon resolves each policy's effective fingerprint when it loads them AND again whenever it is
asked (`/v1/capabilities`, `coop sessions policies`, every fenced create), so an approval edited on
the host is reflected without a restart — the same inputs
`Admit` compiles, through the same assembly (`box/network_session.go:159` builds the plan;
`networkstate/admission.go:152` builds the authority), but through `Store.Resolve` instead of
`Store.Admit`, so nothing is published: no approval, no snapshot, not even an owner key
(`OpenExisting`, never `Open`, and a first-seen provider bundle is matched, not pinned). A policy it
cannot resolve refuses the whole load by name, exactly as an unparsable one does
(`sessionsvc/network.go:131`) — serving it would advertise a reach nobody could pin. The value
reaches callers through `GET /v1/capabilities` and `coop sessions policies`, and a create pins it
as `expected_network_fingerprint`: the fence resolves FRESH and refuses
`network_fingerprint_mismatch` (409) beside the existing digest fence, before any intent is
journaled (`sessionsvc/service.go:987`). The published value is the daemon's LOAD-time resolution,
so after an approval edit it is stale until a restart; the refusal names the current one, and
`coop sessions policies` (a fresh process) always resolves fresh.

**Remote sessions** freeze the posture at create. The session row stores mode, owner-keyed
fingerprint and qualification id (schema 21, `session/schema.go:245`); every later run — cold turn,
warm child, resume, replay — loads that snapshot instead of admitting again, so an approval or
config edit landing mid-session can only produce a visible denial. Then:

- The daemon is the child's HOST parent and scrubs every `COOP_*` from the environment it builds, so
  `COOP_NETWORK_CAPTURE` (`box/network_session.go:22`) can only be set there
  (`sessionsvc/network.go:129`). It carries a *reference* — project, fingerprint, qualification,
  session, attempt — not authority.
- The child proves it: `OpenExisting` (never `Open`, so a child cannot create an owner key) plus
  `LoadSnapshot` of that exact project+fingerprint pair (`box/network_session.go:195`). A forged or
  cross-project reference produces no snapshot and no launch.
- After the child exits the daemon checks every run registered against the session against the
  immutable row's fingerprint and FAILS the turn on a mismatch (`sessionsvc/network.go:210`). An
  inventory it could not read WHOLE fails the turn too (`sessionsvc/network.go:311`): the record it
  skipped is exactly where a mismatching run would hide.
- A filtered child owns a gateway, its volumes and its receipt, so it gets a 30 s stop window
  instead of the ordinary 250 ms (`sessionsvc/acp.go:48`). Killing it fast leaves two running
  containers and a receipt nothing can finalize.

The API adds four reads under `GET /v1/sessions/{id}/network`: the live summary, `/connections`
(the newest run's bounded rows), `/explanations/{event}` (one retained refusal) and `/receipt`
(aggregate, final once closed) (`sessionsvc/http.go:483`), a `network` event on the session stream
whose payload is bounded by construction (refusals grouped and capped, alerts capped),
`SessionDTO.network`, and the four mirroring worker commands `get_network`,
`get_network_connections`, `get_network_explanation`, `get_network_receipt`
(`workerproto/protocol.go:29`). None of them is a path to authority: the API has no approval verb
at all, and the daemon owns the destination projection every one of them applies.

`coop net` is the host operator's read surface, grouped by the job a person arrives with
(`cli/net_cmd.go:49`): ACCESS (what a new run may reach), RUNS (what recorded runs did), REPAIR
(what coop normally does itself). Its verbs differ in what they read:

| verb | reads |
| --- | --- |
| bare `coop net` | this project's mode and its cause, the approved project rules, and — only when the file and the approval differ — the same access change `approve` would show; creates no owner key. Setup health and history cost no line: a launch sets the host up itself |
| `approve` | TTY only, no flags; the exact snapshot of `.agent/project.yaml` (mode, rules, service digests) diffed against the remembered approval, digest-fenced. Nothing pending → `No approval needed — …`, nothing written |
| `check <url-or-host>` | with no `--run`, the approved project rules plus each agent's provider bundle (`cli/net_diagnostic.go`, `netCurrentCheck`); with `--run`, that run's captured policy evaluated hypothetically. Never sends a packet |
| `forget` | one of three mutating verbs: writes a withdrawal marker, THEN removes one project's approval (`networkstate/approval_withdrawal.go`). While the marker stands every ordinary launch of that project is pending — the open default cannot be regained — and only `approve` clears it |
| `runs` · `inspect` · `watch` · `export` | the keyless `Evidence` handle over retained execution records — no runtime, no DNS, no key. Any unique prefix of a run id resolves (`netResolveRun`); an ambiguous one is refused with the prefixes that settle it |
| `blocked <host>` | the newest RETAINED denial of that host in this project's runs (or in `--run`), grouped by boundary/reason/port; the exact event id still works with `--run`. A DNS-only refusal drafts no rule, because it cannot prove TLS/443 |
| `setup` | mutating: builds the image pair and records the host qualification — the same work a filtered launch performs itself when this host has no current proof, so it is a prepare-ahead/recheck verb, not a prerequisite |
| `recover [<run>]` | settles a run whose supervisor died — exact-owned removal, then a final `supervisor_lost` receipt (`box/network_recover.go:50`). The same pass runs from the orphan sweep at loop/fork start AND from `coop net inspect` before it reports a cleanup as incomplete (`cli/net_cmd.go`, `netSettleCleanup`) |

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
endpoints` (`unattributed*` rows with their reason) and `Raw traffic` (per-rule kernel counters,
no host), then only what went wrong (blocked, alerts, evidence gaps, unhealthy enforcement,
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
`Configuring network access` (filtered: one row per selected provider's endpoints from
`policy.Dependencies` and each agent's `Vendor()`, `Applied N approved network rules` counted over
`project`/`operator` origins, the MCP servers over `mcp` origins, an unrecognized origin counted as "other", THEN
`✓ Everything else blocked`; open/offline: one `⚠` row under the same heading), `Starting <agent>`
— and `Checking the Coop box` first: in `box.Run` the remaining image nudges (age, a project
Dockerfile that drifted) as `⚠` rows, and in the cli (`cli/launch_box.go`, after admission and only
for a run that will use the repo's image — a filtered box runs the qualified client image) the
automatic `box.Build` when `BaseImageSkew` reports a definition mismatch, never for an age nudge
or an unstamped image. `runInBoxMode` only: the interactive fork path (`fork_cmd.go`) and the
restricted modes (`restricted.go`) still print the old `coop: shadowed …` line and get no box
check. A failure before the main process is rendered once as a nested `ui.Fail` under its
section, AFTER the deferred cleanup has joined its own error into the reason, and comes back as
`ui.Reported(err)`, which `cli.Main` does not print again; a cancellation the stop line named is
marked the same way when teardown added nothing. Teardown prints exactly one
`coop: stopping the box — …` line for a box that reached its main process (`filtered.started()`, the
daemon's StartedAt evidence; the open path's plain client exit): the recorded host signal
(`hostInterrupt`, which replaced the anonymous `signal.NotifyContext`) or the exit status as a
number — never Ctrl-C inferred from 130.

## Changelog
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
