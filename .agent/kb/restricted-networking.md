---
name: restricted-networking
description: the layers between an --egress filtered flag and docker run, where network authority lives, the precedence ladder, and what a filtered run refuses
subsystem: networking
sources: [internal/egress/snapshot.go, internal/networkgateway/controller.go, internal/networkgateway/credential_broker.go, internal/networkgateway/events.go, internal/networkgateway/guard.go, internal/networkview/records.go, internal/networkreport/report.go, internal/networkstate/admission.go, internal/networkstate/authority.go, internal/networkstate/approval_forget.go, internal/networkstate/qualification.go, internal/networkstate/bundles.go, internal/box/network_admission.go, internal/box/network_bundles.go, internal/box/network_approval.go, internal/box/network_forget.go, internal/box/network_setup.go, internal/box/credential_broker.go, internal/box/filtered_mounts.go, internal/box/filtered_services.go, internal/box/composecheck.go, internal/box/derived_image.go, internal/box/locked_image.go, internal/box/run.go, internal/networkstate/image_files.go, internal/networkstate/image_trees.go, internal/agent/network_bundle.go, internal/agent/locked_clients.go, internal/agent/claude.go, internal/agent/codex.go, internal/agent/gemini.go, internal/agent/grok.go, internal/acpctl/network.go, internal/cli/acp_cmd.go, internal/cli/acp_network.go, docs/networking.md]
updated: 2026-09-18
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
   authority state behind. `SetupNetwork` and `AdmitSessionNetwork` ask the same question first.
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
only where it would decide anything: under `--egress`, `COOP_EGRESS` or a session policy the file's
`open` is moot and is not pending. `approve` has no `--mode`: to change access you edit the file.
An approval binds three things beyond the rules: the project directory's dev+inode (a replacement
at the same path is pending, `networkstate/authority.go`, `checkDirectory`), the reviewed Compose
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

A withdrawal marker (`networkstate/approval_withdrawal.go`, written before the grant is cleared)
outranks the ladder below for an ordinary launch: with no approval it makes the project pending
rather than letting the built-in default reopen it, and only `coop approve` removes it.

The precedence ladder, one line: invocation `--egress` → remembered approval posture → explicit
`COOP_EGRESS` → project `box.egress` → any rule present ⇒ filtered → open
(`networkstate/admission.go`, `resolveMode`). There is no hard ceiling: nothing ever produced one,
so the field and its clamp were deleted. A named session policy replaces the whole branch and
refuses rather than reconcile with a disagreeing remembered posture. The project file spells
no-network access `offline` — `project.Load` is the one place it becomes the internal `none`, with
no alias (`internal/project/project.go`).

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

Traps:

- A filtered run's box is the LOCKED client image, or that image plus the project's own layers.
  A project `.agent/Dockerfile` is built at launch with `COOP_BASE_IMAGE` set to the locked image,
  through the ordinary project build path, under its own tag `coop-<repo>-filtered:<hash of the
  client image>` (`box/derived_image.go:67`) — so an ordinary and a filtered build never collide and
  a new client image forces a rebuild. TWO proofs, both read from the BUILT image and never from the
  Dockerfile text, gate it (`box/derived_image.go:118`): the locked image's `RootFS.Layers` must be a
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
- That build is run by the LAUNCH, not by a human `coop build`, and a Docker build has root and
  ordinary network. The proofs bind what the box RUNS, not what the build may do, so an
  agent-authored `.agent/Dockerfile` is a way out of the gateway at BUILD time (a `RUN` line can
  post the staged context anywhere). The staged context omits every shadowed secret and `.git`
  (`box/image.go:stageBuildContext`), and an untracked box definition is called out on the launch
  line (`box/derived_image.go:102`) — but the trade-off is deliberate and unfenced: binding the
  Dockerfile to `coop approve` the way a `service:` grant is bound is the open design question.
- The host qualification keeps naming the LOCKED image, and so does the execution record's
  `ClientImage` — the derived image is recorded beside it as `ProjectImage`
  (`networkstate/execution.go:75`), evidence of what ran, never authority. The preflight smoke may
  never carry one; it qualifies the client image itself.
- Extra runtime arguments (`COOP_RUN_ARGS`, `coop … -- …`) are reduced to bind mounts and
  `-e KEY=VALUE`; anything else is refused by name (`box/filtered_mounts.go:25`). The gateway, not
  the environment, is the boundary — and a bind that IS or CONTAINS the runtime's control surface
  (`/var/run`, `/run`, `/proc`, `/sys`, `/dev`, `/`, the bound endpoint's socket) is refused too
  (`box/filtered_mounts.go:429`): one curl over a daemon socket starts a container no gateway sees.
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
  `GEMINI_TELEMETRY_ENABLED=false` — so a hello-and-exit session retains no refusal, and a later
  deliberate request to one of those hosts is a real refusal nothing suppresses. Bundle content is
  pinned per `NetworkBundleVersion` by the owner store on first admission
  (`networkstate/bundles.go:18`): changing a bundle without bumping the version is refused as
  integrity drift, so the version moves with the content (`2026-09-10.1` added the proxy). The
  rule is [[provider-bundles-carry-function-not-chatter]].
- A direct filtered CLI run (including one loop worker) brokers the one API-key route its pinned
  adapter declares: Claude `ANTHROPIC_API_KEY`, Gemini `GEMINI_API_KEY`, or Codex
  `OPENAI_API_KEY`. The key is removed from the agent environment and replaced with a per-execution
  substitute plus a loopback provider base URL; Codex also receives an adapter-owned custom
  provider selection because its built-in provider ignores `OPENAI_BASE_URL`. The capless guard
  alone receives an exact read-only secret file; the privileged controller and agent never do.
  Its helper-only resolver and typed controller lease keep the provider API out of agent policy,
  while Envoy retains exact socket/byte attribution. Grok's pinned client has no qualified API base
  override, and every alternate API-key variable or unsupported launch shape refuses before the
  runtime. Ordinary OAuth/access-token files keep their existing handling and are not called
  broker-protected; restricted and session projections retain their existing access-only copies.
- A selected Compose service and its dependency closure use fixed prepared addresses on an internal
  network. The guard maps accepted proxy peers back to those exact service names. Approved and
  denied external TLS therefore carry `service` through the existing event, receipt, human view,
  watch and JSON paths. Internal peer traffic stays direct and never enters that external record.
  This is observation only: `coop approve` remains the sole approval writer.

[[network-gateway]] is the runtime that enforces the capture; [[network-consumers]] is how the loop,
direct runs and remote sessions consume one. [[box-egress-poc]] is the retired experiment, not this.

## Changelog
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
