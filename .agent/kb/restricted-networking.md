---
name: restricted-networking
description: the layers between an --egress filtered flag and docker run, where network authority lives, the precedence ladder, and what a filtered run refuses
subsystem: networking
sources: [internal/egress/snapshot.go, internal/networkgateway/controller.go, internal/networkstate/admission.go, internal/networkstate/authority.go, internal/networkstate/qualification.go, internal/box/network_admission.go, internal/box/network_approval.go, internal/box/network_setup.go, internal/box/filtered_mounts.go, internal/box/composecheck.go, internal/box/derived_image.go, internal/box/run.go, docs/networking.md]
updated: 2026-09-10
---

`coop <agent> --egress filtered` runs the box behind a per-run gateway. Five boring layers stand
between that flag and `docker run`:

1. **Policy** — `internal/egress`: rule grammar, normalization, exact/wildcard matching, provider
   bundles, `Compile` → an owner-keyed `Snapshot` with a fingerprint (`egress/snapshot.go:20`). It
   combines already-authorized inputs; it approves nothing.
2. **Authority** — `internal/networkstate`: an owner-private `Store` under
   `~/.local/state/coop/network` (`box/network_admission.go:19`). `Open` refuses when that root is
   reachable from any agent mount (`networkstate/authority.go:39`), so it is never a box mount.
3. **Admission** — `box.AdmitNetwork` (`box/network_admission.go:55`) resolves the posture and, for
   filtered mode only, captures the frozen policy the gateway will enforce.
4. **Launch** — `box.Run` branches once, on `spec.CapturedEgress` (`box/run.go:404`): non-nil takes
   the `filtered*.go` family, nil takes the open path byte for byte.
5. **Per-host preflight** — `coop net setup` (`box/network_setup.go:63`).

Authority never comes from the repository or the box. A repo's `box.egress_rules`, and a rules file
that lives inside an agent mount, are *requests*; a human turns one into a grant with
`coop net approve`, the only caller of `Store.Approve` (`box/network_approval.go:202`). An approval
binds three things beyond the rules: the project directory's dev+inode (a replacement at the same
path is refused, `networkstate/authority.go:459`), the reviewed Compose stanza of every `service:`
grant as a digest recomputed at launch (`box/composecheck.go:415`, `box/network_approval.go:276`),
and the same capability gate a launch applies — an unenforceable rule is refused at review, not
remembered (`box/network_approval.go:157`). Observed
traffic is evidence, never a grant. Admission marks the resolved posture explicit through
`cfg.SetEgress` (`box/network_admission.go:91`), so the project overlay cannot decide the mode a
second time. A run that neither asks for filtered nor has a remembered posture writes NO host
state — the preview creates no owner key (`networkstate/admission.go:45`).

The precedence ladder, one line: invocation `--egress` → remembered approval posture → explicit
`COOP_EGRESS` → project `box.egress` → any rule present ⇒ filtered → open
(`networkstate/admission.go:181`). There is no hard ceiling: nothing ever produced one, so the
field and its clamp were deleted. A named session policy replaces the whole branch and refuses
rather than reconcile with a disagreeing remembered posture (`networkstate/admission.go:170`).

Qualification has two halves. The release half is the `networkruntimee2e`-tagged suite —
enforcement, denial, guard/collector faults, transports, credentialed providers — and it is what the
published support matrix rests on. The per-host half is `coop net setup`: it builds the pinned
gateway and locked client images for the bound daemon, runs ONE smoke through the ordinary launch
engine (allowed TLS with no proxy variables; a denied name, a raw IP, metadata and the resolver all
refused — `box/network_setup.go:43`), and records the runtime binding, image digests, contract and
receipt. Admission then needs a record whose contract, transports and client builds cover this
policy (`networkstate/qualification.go:386`). Nothing else builds an image or installs tooling at
launch.

Traps:

- A filtered run's box is the LOCKED client image, or that image plus the project's own layers.
  A project `.agent/Dockerfile` is built at launch with `COOP_BASE_IMAGE` set to the locked image,
  through the ordinary project build path, under its own tag `coop-<repo>-filtered:<hash of the
  client image>` (`box/derived_image.go:67`) — so an ordinary and a filtered build never collide and
  a new client image forces a rebuild. TWO proofs, both read from the BUILT image and never from the
  Dockerfile text, gate it (`box/derived_image.go:118`): the locked image's `RootFS.Layers` must be a
  PREFIX of the built image's, and every pinned client entry point (launcher, each `Exec` element
  including `/usr/local/bin/node`, and every `RequiredExecutables` path) must be byte-identical in
  both images. The file reads happen in a container that is CREATED AND NEVER STARTED
  (`runtime.Docker.FileDigest`, `runtime/docker_lifecycle.go:333`): asking a tampered image to
  describe itself is how the check would be defeated. They cost ~775 MB of `docker cp` per image on
  the current closure, so the digests are memoized per image ID. `COOP_IMAGE` stays refused at
  admission (`box/network_admission.go:175`): nothing qualified it and no proof can.
- That build is run by the LAUNCH, not by a human `coop build`, and a Docker build has root and
  ordinary network. The proofs bind what the box RUNS, not what the build may do, so an
  agent-authored `.agent/Dockerfile` is a way out of the gateway at BUILD time (a `RUN` line can
  post the staged context anywhere). The staged context omits every shadowed secret and `.git`
  (`box/image.go:stageBuildContext`), and an untracked box definition is called out on the launch
  line (`box/derived_image.go:102`) — but the trade-off is deliberate and unfenced: binding the
  Dockerfile to `coop net approve` the way a `service:` grant is bound is the open design question.
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

[[network-gateway]] is the runtime that enforces the capture; [[network-consumers]] is how the loop,
direct runs and remote sessions consume one. [[box-egress-poc]] is the retired experiment, not this.

## Changelog
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
