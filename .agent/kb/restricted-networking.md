---
name: restricted-networking
description: the layers between an --egress filtered flag and docker run, where network authority lives, the precedence ladder, and what a filtered run refuses
subsystem: networking
sources: [internal/cli/fork_cmd.go, internal/cli/fork_cmd_test.go, internal/box/filtered_test.go, internal/runtime/docker_lifecycle.go, internal/runtime/docker_test.go, internal/egress/snapshot.go, internal/networkgateway/controller.go, internal/networkgateway/credential_broker.go, internal/networkgateway/events.go, internal/networkgateway/guard.go, internal/networkview/records.go, internal/networkreport/report.go, internal/networkstate/admission.go, internal/networkstate/authority.go, internal/networkstate/project_anchor.go, internal/networkstate/approval_forget.go, internal/networkstate/qualification.go, internal/networkstate/bundles.go, internal/box/network_admission.go, internal/box/network_bundles.go, internal/box/network_approval.go, internal/box/network_forget.go, internal/box/network_setup.go, internal/box/authority_mounts.go, internal/box/credential_broker.go, internal/box/filtered_mounts.go, internal/box/filtered_services.go, internal/box/composecheck.go, internal/box/derived_image.go, internal/box/project_build.go, internal/box/locked_image.go, internal/box/run.go, internal/networkstate/image_files.go, internal/networkstate/image_trees.go, internal/networkstate/project_builds.go, internal/agent/network_bundle.go, internal/agent/locked_clients.go, internal/agent/claude.go, internal/agent/codex.go, internal/agent/gemini.go, internal/agent/grok.go, internal/acpctl/network.go, internal/cli/acp_cmd.go, internal/cli/acp_network.go, internal/cli/net_cmd.go, internal/cli/modelscache.go, docs/networking.md, internal/sessionsvc/acp.go]
updated: 2026-10-09
---

`coop <agent> --egress filtered` runs the box behind a per-run gateway. Five boring layers stand
between that flag and `docker run`:

1. **Policy** — `internal/egress`: rule grammar, normalization, exact/wildcard matching, provider
   bundles, `Compile` → an owner-keyed `Snapshot` with a fingerprint (`egress/snapshot.go:20`). It
   combines already-authorized inputs; it approves nothing.
2. **Authority** — `internal/networkstate`: an owner-private `Store` under
   `~/.local/state/coop/network` (`box/network_admission.go:19`). `Open` refuses when that root is
   reachable from any agent mount (`networkstate/authority.go:39`), so it is never a box mount.
3. **Admission** — `box.AdmitNetwork` (`box/network_admission.go:58`) resolves the posture and, for
   filtered mode only, captures the frozen policy the gateway will enforce. Once the mode resolves
   to filtered, `checkFilteredRuntime` is the first gate: a runtime that cannot serve the gateway is
   refused before `networkstate.Open`, so asking for something this host cannot do leaves no
   authority state behind. `SetupNetwork` and `AdmitControllerJobNetwork` ask the same question first.
   The shared MCP file is handled in two halves: its SOURCE is proven on every launch (outside
   every mount, parseable — `networkMCPSnapshot`), but its destinations and the gateway's MCP
   qualification rules (literal only, HTTPS on 443, no `headersHelper`/`oauth`) are derived only
   once the posture is filtered (`networkMCPDependenciesOf`), because on an open box those rules
   refuse ordinary client features.
4. **Launch** — `box.Run` branches once, on `spec.CapturedEgress` (`box/run.go:399`): non-nil takes
   the `filtered*.go` family, nil takes the open path byte for byte.
5. **Per-host qualification** — `SetupNetwork` (`box/network_setup.go`), run by the first filtered
   launch that finds no current proof (`box/network_admission.go`, `ensureNetworkQualification`)
   and by `coop net setup` on demand.

Authority never comes from the repository or the box. A repo's `box.egress`, its
`box.egress_rules`, and a rules file that lives inside an agent mount, are *requests*; a human turns
them into a grant with `coop approve`, the only caller of `Store.Approve`
(`box/network_approval.go`). The approval is the EXACT snapshot of the file — mode, normalized
rules, and each named service's reviewed definition — and ONE read-only check decides whether the
file and the approval differ: `Admission.pendingApproval` (`networkstate/admission.go`), surfaced
as `AdmissionPreview.Pending` / `*PendingApproval`. `coop init`, bare `coop net`, `coop approve`
(which then has nothing to ask and writes nothing) and every launch through `AdmitNetwork` read the
same answer, so no two of them can disagree. Without an approval only a widening is pending —
`open`, or any rule; filtered/offline with no rules is what coop grants on its own, which is why a
fresh `coop init` project (explicitly `filtered`) launches without a review. The file's mode counts
only where it would decide anything: under `--egress` or `COOP_EGRESS` the file's
`open` is moot and is not pending. `approve` has no `--mode`: to change access you edit the file.
An approval binds three things beyond the rules: the project directory's private two-link anchor (a
replacement or copied marker is pending; allocator metadata alone is never trusted), the reviewed Compose
stanza of every `service:` grant as a digest recomputed at launch (`box/composecheck.go:415`,
`box/network_approval.go`, `requestedServiceDigests`), and the same capability gate a launch applies
— an unenforceable rule is refused at review, not remembered. `coop net forget` is the way back and the only caller of
`Store.Forget` (`networkstate/approval_forget.go:59`): it removes exactly one approval file under the
approval lock, proves the removal, and touches no evidence. It cannot sweep — a record is filed under
a keyed hash of the canonical path and stores no path, so the store can answer "is this project
approved?" and never "which projects are?" — and it derives that id LEXICALLY
(`networkstate/approval_forget.go:34`), which is the only way to name the record a deleted checkout
left behind. An approval made through a symlink is filed under its target, so once the link is gone
its path locates nothing, and forget says so instead of removing another project's. Observed
traffic is evidence, never a grant. Admission marks the resolved posture explicit through
`cfg.SetEgress` (`box/network_admission.go:91`), so the project overlay cannot decide the mode a
second time. A run that neither asks for filtered nor has a remembered posture writes NO host
state — the preview creates no owner key (`networkstate/admission.go:45`).

Approval review has a two-phase concurrency rule. An exact existing marker/private-anchor pair may
be reviewed read-only without pausing running boxes. Any missing, replaced, or moved binding is a
transition: `ReviewProjectNetwork` takes the project's exclusive service-launch lock, re-reads the
policy and pending state, then creates or re-enrolls the pair. `Store.Approve` never performs that
transition later at the prompt boundary; it reuses the reviewed pair exactly or returns
`ErrApprovalChanged`. This prevents marker deletion or checkout movement between an unlocked probe
and enrollment from crossing an active sandbox mount window.

ACP supervision carries this explicit mode through `spawnBox` to initial, replacement and warm
children, removing any conflicting ambient `COOP_EGRESS`. Each ACP-backed `coop models` probe
admits separately on a cloned configuration, for its exact selected provider/account only
(`cli/modelscache.go`, `admitModelProbe`). It follows the same project/remembered-policy ladder;
it never admits the editor toolbar's alternatives or exports a default `open` to bypass project
tightening. The child re-proves its capture and account binding normally. Host setup may run
before the 15-second metadata handshake; cancellation still owns setup cleanup. Host-command
catalogs do not launch a box. Exporting a mode alone grants no filtered authority.

A withdrawal marker (`networkstate/approval_withdrawal.go`, written before the grant is cleared)
outranks the ladder below for an ordinary launch: with no approval it makes the project pending
rather than letting the built-in default reopen it, and only `coop approve` removes it.

The precedence ladder, one line: invocation `--egress` → remembered approval posture → explicit
`COOP_EGRESS` → project `box.egress` → any rule present ⇒ filtered → open
(`networkstate/admission.go`, `resolveMode`). There is no hard ceiling: nothing ever produced one,
so the field and its clamp were deleted. Controller-authored jobs use `CaptureJob` instead of
this local approval ladder; they do not consult or clear local withdrawal markers, and ordinary
admission stays barred. The project file spells no-network access `offline` — `project.Load` is
the one place it becomes the internal `none`, with no alias (`internal/project/project.go`).

Qualification has two halves. The release half is the `networkruntimee2e`-tagged suite —
enforcement, denial, guard/collector faults, transports, credentialed providers — and it is what the
published support matrix rests on. The per-host half is `SetupNetwork`: it builds the pinned
gateway and locked client images for the bound daemon, runs ONE smoke through the ordinary launch
engine (allowed TLS with no proxy variables; a denied name, a raw IP, metadata and the resolver all
refused — `smokeScript`, `setupChecks`), and records the runtime binding, image digests, contract
and receipt. It is machinery, not a decision, so an ordinary filtered launch runs it itself when
`currentQualification` finds no record that covers the policy, was made on this daemon by this
coop's definitions AND still names an image pair that exists (`box/network_admission.go`); the
launch continues on the record setup left. Setups serialize on `Store.LockSetup` (`setup-lock`
under the root, 30 min bound), so two first launches never build the same images side by side.
The session daemon passes no qualifier (`network_session.go`): an API request never builds images,
so a host must be prepared with `coop net setup` before a filtered session. The transcript is the
whole result — two sentences, one `✓`/`✗` line per proved property, one verdict; a check failure is
`ErrNetworkSetupFailed`, which `cmdNetSetup` maps to exit 1 without repeating the verdict. Nothing
else builds an image or installs tooling at launch.

The locked client closure has two supply-chain arms with the same identity rule. npm clients come
from the embedded exact package lock and registry SHA-512 integrity; a native artifact carries one
versioned HTTPS object URL and a Coop-owned SHA-256 digest. The image downloads that object without
following redirects, verifies the digest before decompression, and only then installs it. One
physical executable may record both CLI and ACP coverage only when provider, package/artifact,
launcher argv and environment controls are byte-for-byte identical. Gemini 0.59.0 uses this shared
npm shape; Grok 1.0.25 uses the direct checksummed GCS object. A floating installer, downloaded
checksum, mismatched platform URL, or two conflicting declarations is not a locked client.

The ordinary base image installs this same closure: `lockedClientParts` renders the client layer
for both `baseImageDefinition` and `lockedImageDefinition`, so only PATH and the base's asdf
provisioning differ and every box runs the qualified set. The launchers live in
`agents.LauncherDir` (`/opt/coop/bin`), first on PATH in both images, so an asdf shim cannot shadow
a client. Each adapter's `UpdateControls` ride in the closure too (environment in `ENV`, files under
`/etc` via `COPY system/ /`), so the closure digest covers them: changing one re-qualifies every
host's filtered setup on its next launch. The base build asks the runtime for its platform
(`Runtime.BuildPlatform`) because the closure's native paths and Grok's artifact are per-arch.

Traps:

- Docker 27's `container stop` long flag is `--time`; Docker 29's is `--timeout`. Bound cleanup
  uses the stable `-t` spelling (`runtime/docker_lifecycle.go`) and still confirms the exact
  container's terminal state. A parser failure otherwise makes setup report unfinished cleanup,
  even after packet checks pass. A Debian DinD worker also needs the modern CLI and Buildx
  together, plus identical worker/daemon bind-source paths; installing Buildx beside CLI 20.10
  does not make `docker build --builder` work.
- A normal repository-read-only session has a writable, private `.coop-output` bind. Its trusted
  emitter uses `source:target`, Docker's default RW form, not redundant `:rw`: both complete-plan
  filtered parsers permit only that form or explicit `:ro`. The output's exact private-state
  allowlist and inspected RW equality remain mandatory; do not relax parsing or repository RO.
- A filtered run's box is the LOCKED client image, or that image plus the project's own layers.
  A project `.agent/Dockerfile` is explicitly built on the host with `COOP_BASE_IMAGE` set to the locked image,
  through the ordinary project build path, under its own tag `coop-<repo>-filtered:<hash of the
  client image>` (`box/derived_image.go:BuildFilteredProject`) — so an ordinary and a filtered build never collide and
  a new client image forces a rebuild. TWO proofs, both read from the BUILT image and never from the
  Dockerfile text, gate it (`box/derived_image.go:proveDerivedImage`): the locked image's `RootFS.Layers` must be a
  PREFIX of the built image's, and every pinned client entry point (launcher, each `Exec` element
  including `/usr/local/bin/node`, and every `RequiredExecutables` path) must be byte-identical in
  both images. The complete `/opt/coop/clients` directory archive is also hashed, so changing,
  adding or removing an imported JavaScript dependency is refused. The file and tree reads happen
  in a container that is CREATED AND NEVER STARTED (`runtime.Docker.FileDigest` and `TreeDigest`):
  asking a tampered image to describe itself is how the check would be defeated. Both reads are
  memoized per immutable image ID in the process and owner-private store
  (`networkstate/image_files.go`, `networkstate/image_trees.go`). Host setup records the locked
  image's pinned files while it has the daemon in hand; the first derived-image proof records the
  complete tree. A record that is missing, damaged or for another image/root is a MISS and the image
  is read; nothing there can make a changed image pass. `COOP_IMAGE` stays refused at
  admission (`box/network_admission.go:175`): nothing qualified it and no proof can.
- The explicit filtered-build CLI cancels its context on SIGINT/SIGTERM: the runtime build owns
  a separate process group, so terminal signals alone cannot clean it up. Cancellation stops the
  builder and its children, removes staged inputs, and never publishes an unfinished approval.
- `coop net setup` also needs a signal-bound context while it builds the locked client and gateway
  images. A bare `context.Background()` lets Coop exit on SIGTERM while Buildx continues in its
  separate process group; the build can fill a constrained host after the CLI is gone. Runtime
  process-group cleanup occurs only when the setup context is cancelled. A failed setup never
  publishes host qualification; leftover Docker cache may still need operator cleanup.
- Project builds are explicit under restricted networking: `coop build --egress filtered` prepares
  the filtered image; plain `coop build` follows the effective project posture. The build itself
  still has ordinary Docker networking, so review the Dockerfile and copied files first.
  `filteredProjectImage` consumes only a version-2 host approval, never building on a miss. Legacy
  version-1 automatic-build memos are not authority. An open editor connection may build a
  missing project image; ordinary launches do not rebuild changed project inputs. `BuildWith`
  refuses automatic project builds for filtered/offline modes.
- Approval binds the exact sanitized context (kind, path, mode/link target, bytes), daemon, locked
  image, client closure, tags, Dockerfile path and build environment. Only an explicit build writes
  it, after both image proofs, for the digest produced WHILE staging and the ID from `--iidfile`.
  Publication failure fails the command. A launch requires matching inputs, a present immutable ID
  and both proofs. The old cache-purity Dockerfile parser is gone: external inputs are fetched only
  during the explicit build. Linked Dockerfiles/ignore files are refused BEFORE building, not just
  excluded from cache reuse. Staging preserves source modes and excludes shadowed secrets/Git.
- Direct, fork, loop and ACP launch checks defer ordinary-image availability until admission:
  filtered runs need only the prepared filtered image. Offline still requires an existing ordinary
  image, preventing an implicit runtime pull. Filtered ACP warming keys the actual approved image
  and current build inputs, not an unrelated ordinary tag.
- The host qualification keeps naming the LOCKED image, and so does the execution record's
  `ClientImage` — the derived image is recorded beside it as `ProjectImage`
  (`networkstate/execution.go:75`), evidence of what ran, never authority. The preflight smoke may
  never carry one; it qualifies the client image itself.
- Extra runtime arguments (`COOP_RUN_ARGS`, `coop … -- …`) are reduced to bind mounts, `-e KEY=VALUE`
  and `--label KEY=VALUE` (metadata: fleet accounting, a live test's reaping key — it changes neither
  what the box knows nor what it reaches, and `validateMounts` already admits it); anything else is
  refused by name (`box/filtered_mounts.go:25`). Coop's ownership labels take precedence if an
  operator uses the same key. The gateway, not the environment, is the boundary — and a bind that
  IS or CONTAINS the runtime's control surface
  (`/var/run`, `/run`, `/proc`, `/sys`, `/dev`, `/`, the bound endpoint's socket) is refused too
  (`box/filtered_mounts.go:482`): one curl over a daemon socket starts a container no gateway sees.
- Mount validation distinguishes host authority from agent-writable roots. `BoxHome` is
  protected against exposure but is not mounted wholesale, so a normal selected profile
  beneath its default `agents/` tree is an independent bind. The selected profile itself
  is writable by the agent; a second bind beneath it is refused (`box/filtered.go`,
  `box/filtered_mounts.go`).
- The gateway captures every TLS port the policy grants (`Snapshot.TLSPorts`, `egress/snapshot.go:292`)
  plus DNS on 53; the upstream port comes from the kernel's redirect record (SO_ORIGINAL_DST),
  never from the client, and a dial straight at the guard listener is refused. A raw `tcp` grant
  on a captured TLS port, a raw grant on 53, `tls` on 53, and a published `serve` port on a
  captured port are refused (`SupportedRule`, `egress/snapshot.go:485`).
- IPv6 is refused everywhere on this runtime — address, CIDR or `icmpv6`
  (`egress/snapshot.go:469`) — because the reference Docker bridge has none.
- A provider bundle is FUNCTION, not everything the client asks for. Claude's core is
  `api.anthropic.com`, `platform.claude.com` (OAuth refresh) and `mcp-proxy.anthropic.com` (the
  claude.ai connectors a login has on by default); codex's is `chatgpt.com` and `auth.openai.com`
  (`agent/claude.go`, `agent/codex.go`). The client's own release feed, package registry, update
  check and telemetry intake (`raw.githubusercontent.com`, `registry.npmjs.org`, `api.github.com`,
  Datadog, `ab.chatgpt.com`) are switched off box-only instead — Claude through `BoxEnv`, codex
  through the always-on `config.toml` overlay, gemini through its settings overlay plus
  `GEMINI_TELEMETRY_ENABLED=false`, grok through `BoxEnv` (`GROK_TELEMETRY_ENABLED=false`, which
  also stops its `grok.com` and `api.x.ai` lookups, and `GROK_DISABLE_AUTOUPDATER=1`) — every Coop
  image also carries each client's update controls, homes mounted or not —
  so a hello-and-exit session retains no refusal, and a later deliberate request to one of those
  hosts is a real refusal nothing suppresses. Bundle content is
  pinned per `NetworkBundleVersion` by the owner store on first admission
  (`networkstate/bundles.go:18`): changing a bundle without bumping the version is refused as
  integrity drift, so the version moves with the content (`2026-09-10.1` added the proxy). The
  rule is [[provider-bundles-carry-function-not-chatter]].
- Online coding runs broker the selected accounts, API-key and subscription alike. The canonical
  host authority owns grants and renewal; repository-local native homes receive only public
  selectors, never access/refresh tokens (`box/native_broker.go`, `planNativeAccounts`). Each
  adapter's `NativeCredentials().Broker` declares exact routes, including Grok's model catalog.
  The filtered guard reaches those protected origins through helper authority; the workload
  receives no direct grant to them (`box/network_bundles.go`, `nativeNetworkBundle`). An open run
  uses the same broker in a private inspected Docker namespace (`box/native_open.go`), so open
  networking does not mean unprotected credentials. Online provider runs require Docker.
  Offline and sign-in runs do not start this coding broker. Sign-in is a separate host workflow;
  never import an old access-only projection to repair a revoked canonical account.
- Bearer MCP servers ride the same broker, as `mcp`-kind routes after the provider routes (exact
  URL path, POST/GET/DELETE, no header timeout; 8 provider + 64 MCP routes, 15580–15651): the box
  gets `COOP_MCP_TOKEN_<i>` stand-ins and a rewritten snapshot, the operator's token variables never
  enter any filtered box, and each bearer server's host is withheld from the agent's own grants. A
  direct grant of such a host needs no refusal — the box never holds the token. Details and the
  session handoff: [[mcp-authority-projection]].
- Host renewal happens before and throughout brokered runs. Private broker snapshots expire
  closed when publication stops; remote children point to the same canonical host authority
  (`sessionsvc/acp.go`), not a second refresh store. Uncertain renewal is a recovery result,
  never a blind retry or permission to expose refresh authority. See
  [[renew-before-access-only-projection]]. The pre-v11 box-owned renewal captures in older
  changelog entries are historical evidence, not instructions for current launches.
- A selected Compose service and its dependency closure use fixed prepared addresses on an internal
  network. The guard maps accepted proxy peers back to those exact service names. Approved and
  denied external TLS therefore carry `service` through the existing event, receipt, human view,
  watch and JSON paths. Internal peer traffic stays direct and never enters that external record.
  This is observation only: `coop approve` remains the sole approval writer.
- Those addresses take two Compose passes (`box/filtered_services.go`): a seed creates the
  `<project>_filtered` network so Docker picks a subnet, then the final override pins that subnet and
  each service's address (fixed addresses need a declared subnet). Once the network exists, the seed
  must declare that same subnet: a network declared differently makes Compose recreate it, which
  first disconnects the services, and that disconnect fails for a service `coop up` moved to the
  plain network ("container ... is not connected to the network ..._filtered"). Never remove the
  service containers to get past it: a recreated container keeps its anonymous volumes, a removed
  one orphans them and its replacement starts empty.

[[network-gateway]] is the runtime that enforces the capture; [[network-consumers]] is how the loop,
direct runs and remote sessions consume one. [[box-egress-poc]] is the retired experiment, not this.

## Changelog
- 2026-10-09 — repaired unadmitted ACP catalog probes; verified private per-provider config,
  exact selected-account scope, project tightening and pending-approval refusal in regression tests.
  Replaced stale pre-v11 credential guidance against native broker planning, open namespace,
  account renewal and remote authority handoff; historical captures remain historical.
- 2026-10-07 — traced Docker 27 DinD setup's unconfirmed stop to the newer long flag, added the
  portable cleanup spelling and strict success/rejected-stop regressions, and documented worker
  preparation separately from protocol support. No fallback widens a filtered job's authority.
- 2026-10-07 — the filtered services seed declares the existing network's subnet, so `coop up` then a
  filtered launch works and services keep their anonymous volumes; the container removal that
  7f80e02e used instead is gone (task 2026-10-07-filtered-launches-keep-services-anonymous-volume).
- 2026-09-28 — checked both open run and filtered create option assembly: Coop ownership labels
  now come after operator labels, so sweep and reap still see the box when keys collide.
- 2026-09-28 — narrowed the open automatic-build note to the missing-image editor path after
  checking `ensureACPImage`, `BuildWith` and ordinary launch behavior.
- 2026-09-27 — rechecked `Admission`, `CaptureJob` and the withdrawal marker: removed the
  retired named-policy branch while preserving the direct-launch ladder and separate job capture;
  corrected the retired `AdmitSessionNetwork` reference to `AdmitControllerJobNetwork`.
- 2026-09-24 — proved a real filtered Gemini API-key launch was refused because the
  host-only `BoxHome` was treated as writable; recorded the corrected mount distinction
  and the focused acceptance/denial regression.
- 2026-09-24 — Cloud Shell interruption exposed a missing SIGINT/SIGTERM context in `coop net setup`:
  Buildx continued after the command exited and consumed disk. Added the CLI cancellation boundary
  and process regression; swept sibling filtered-build and runtime cancellation paths, which already
  bind their contexts and reap process groups. Filtered Linux qualification remains unverified on
  this disk-limited host.
- 2026-09-23 — project identity moved from reusable inode metadata to a private hardlink anchor.
  Older approvals require explicit review; final box arguments now refuse writable project-parent
  mounts, every private-state exposure and opaque/custom volumes that could transplant the link.
- 2026-09-22 — ACP children carry their supervisor's explicit mode across re-exec; standalone
  catalog refresh remains host-side metadata, with existing project-overlay behavior preserved.
- 2026-09-22 — implemented the approved explicit restricted-project-build policy, preserving open
  automatic builds. Swept automatic callers, input/approval records, recovery and ACP warm identity.
- 2026-09-22 — corrected the API-key and OAuth statements for direct readonly composition against
  `restricted.go` and its composition tests; retained the bare and restricted remote-session limits.
- 2026-09-20 — COOP_RUN_ARGS now also admits `--label KEY=VALUE` under filtered (metadata only);
  a live-test supervisor reaps its filtered boxes by label the same as an open one.
- 2026-09-19 — bearer MCP servers ride the broker under filtered networking.
- 2026-09-19 — the broker serves every selected API-key account (one route and listener per
  provider, lead to ACP and remote sessions); the Codex hookup moved from lead-only `-c` argv to a
  per-run managed config; recorded the three live-found traps (the Claude query, the caller-owned
  filtered check, per-account classification).
- 2026-09-19 — the ordinary base installs the same locked closure (one shared client layer); the
  launchers moved to /opt/coop/bin, first on PATH; the closure carries every adapter's update
  controls; Grok's updater is switched off.
- 2026-09-19 — a filtered launch reuses the project image it built last when nothing it builds from
  changed (the digest, the owner-private record, the BuildKit-faithful Dockerfile scanner); staging
  keeps source modes. Re-pointed the derived_image.go line references.
- 2026-09-18 — the session gap above is closed: Grok renews on the host before projection, and a
  filtered session projects for the horizon.
- 2026-09-18 — grok's telemetry is switched off box-only (`GROK_TELEMETRY_ENABLED=false`): the
  pinned client's mixpanel, `grok.com` and `api.x.ai` lookups raised a burst alert on every run.
- 2026-09-18 — a filtered Grok login with a refresh token no longer has to outlive the one-hour
  horizon: the box renews it through `auth.x.ai`; access-only credentials still do.
- 2026-09-18 — approvals and a run's artifact directory compare the inode, not the device: a reboot
  renumbered every approved project into "replaced" and made post-reboot recovery refuse to clean
  up (`authority.go` `checkDirectory`, `artifact.go` `openPrivateDirectory`).
- 2026-09-18 — MCP admission split: source isolation and parse on every launch, destinations and the
  gateway's MCP rules only on the filtered path. Deriving them first had made every launch pay the
  filtered rules, so an open box refused a `${VARIABLE}` header.
- 2026-09-18 — the runtime question moved to the front of every filtered entry point
  (`checkFilteredRuntime`), so a non-Docker runtime is refused by name before the authority root or
  an owner key exists, instead of surfacing from `bindDocker` mid-qualification
- 2026-09-15 — extended the API-key boundary to the pinned Gemini and Codex clients, made every
  recognized but unqualified key and launch shape refuse before runtime, and retained Grok OAuth
  while refusing its unredirectable API key
- 2026-09-14 — external TLS from filtered Compose services now retains the exact prepared service
  name in the existing network evidence and views; internal service traffic remains direct and
  unreported as external traffic.
- 2026-09-13 — added a bounded, non-executing directory-archive digest for the complete locked
  JavaScript client installation; derived images may add tools elsewhere but may not change the
  npm dependency tree. Re-verified with real unchanged and transitive-mutation Docker builds.
- 2026-09-13 — added exact Gemini npm and Grok native-artifact closures, including shared CLI/ACP
  coverage, direct no-redirect download, embedded digest verification and platform mutation checks;
  added account/auth-family bundle binding for filtered ACP
- 2026-09-13 — added the locked Claude API-key credential-broker first slice: guard-only secret,
  helper-only route admission, Envoy-attributed upstream, session substitute, and fail-closed
  unsupported-mode boundary. Re-verified against the listed adapter, box, and gateway sources.
- 2026-09-10 — a filtered launch qualifies the host itself (`ensureNetworkQualification`, images
  must still exist, serialized on `LockSetup`); `coop net setup` keeps the same transcript: two
  sentences, one line per proved check, one verdict. Approval is an exact snapshot of the project
  file (mode + rules + service digests) decided by ONE `pendingApproval` check shared by init, bare
  `coop net`, `approve` (no `--mode`, no-op writes nothing) and every `AdmitNetwork` launch; a
  repository `open` is a request. New projects scaffold `box.egress: filtered`; the file spells
  `offline`. Re-verified against the sources above.
- 2026-09-10 — Claude's bundle gained `mcp-proxy.anthropic.com` under `2026-09-10.1`, and the
  managed clients' own update/telemetry traffic is switched off box-only (Claude env, codex and
  gemini overlays) instead of being granted or filtered out of the report. Re-verified live: a
  filtered `coop claude -p` hello reaches api + mcp-proxy only, zero refusals; codex likewise.
- 2026-09-10 — the pinned-client digests a Dockerfile launch compares are now recorded per image id
  in the owner store (and by `coop net setup` for the locked image), so the added cost of a
  Dockerfile project falls from ~6.4s to ~2.6s on the first launch after a build and to ~0.4s on a
  repeat. Measured on this host; every refusal message is unchanged and re-proved live.
- 2026-09-10 — `coop net forget` removes one project's remembered approval (lexical id derivation, so
  a deleted checkout's record can still be named; a gone symlink locates nothing rather than removing
  the wrong record), and `coop net` reports a replaced project directory as blocked instead of leaving
  the refusal to the next launch. Verified on a real terminal against scratch repos.
- 2026-09-10 — a project `.agent/Dockerfile` now runs under `--egress filtered`: built on the locked
  client image under its own tag, admitted only by a layer-prefix proof plus a byte-identical
  pinned-client proof, both read from the built image. Admission's Dockerfile refusal is gone;
  `COOP_IMAGE`'s remains. Verified end-to-end on a scratch repo (a package the base lacks, `coop
  claude`, a tampered launcher, a foreign base).
- 2026-09-10 — S7c: an approval now binds the project directory's inode and each `service:` grant's
  reviewed Compose digest, `approve` applies the launch capability gate, filtered mounts refuse the
  runtime's control surfaces, and the never-produced hard ceiling is gone. Re-verified.
- 2026-09-10 — created for the shipped feature (S0–S6, 54be097…f3e1d96), replacing eleven cards
  written for the abandoned custody design. Verified against the sources above.
- 2026-09-10 — TLS on any granted port: the capture set follows `TLSPorts`, ports come from SO_ORIGINAL_DST; refs refreshed (SupportedRule moved to snapshot.go:485).
