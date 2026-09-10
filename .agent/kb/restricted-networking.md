---
name: restricted-networking
description: the layers between an --egress filtered flag and docker run, where network authority lives, the precedence ladder, and what a filtered run refuses
subsystem: networking
sources: [internal/egress/snapshot.go, internal/networkgateway/controller.go, internal/networkstate/admission.go, internal/networkstate/authority.go, internal/networkstate/qualification.go, internal/box/network_admission.go, internal/box/network_approval.go, internal/box/network_setup.go, internal/box/filtered_mounts.go, internal/box/run.go, docs/networking.md]
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
`coop net approve`, the only caller of `Store.Approve` (`box/network_approval.go:185`). Observed
traffic is evidence, never a grant. Admission marks the resolved posture explicit through
`cfg.SetEgress` (`box/network_admission.go:91`), so the project overlay cannot decide the mode a
second time. A run that neither asks for filtered nor has a remembered posture writes NO host
state — the preview creates no owner key (`networkstate/admission.go:45`).

The precedence ladder, one line: invocation `--egress` → remembered approval posture → explicit
`COOP_EGRESS` → project `box.egress` → any rule present ⇒ filtered → open
(`networkstate/admission.go:165`), and a hard ceiling then clamps that result, refusing rather than
narrowing an explicit mode that exceeds it. A named session policy replaces the whole branch and
refuses rather than reconcile with a disagreeing remembered posture
(`networkstate/admission.go:155`).

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

- A filtered run uses the LOCKED client image: a project `Dockerfile` or a `COOP_IMAGE` override is
  refused by name at admission (`box/network_admission.go:173`), never downgraded to a warning.
- Extra runtime arguments (`COOP_RUN_ARGS`, `coop … -- …`) are reduced to bind mounts and
  `-e KEY=VALUE`; anything else is refused by name (`box/filtered_mounts.go:25`). The gateway, not
  the environment, is the boundary.
- 443 and 53 are the gateway's own capture: a raw `tcp`/`udp` grant on either
  (`egress/snapshot.go:502`) or a published `serve` port on either
  (`networkgateway/controller.go:133`) is refused.
- IPv6 is refused everywhere on this runtime — address, CIDR or `icmpv6`
  (`egress/snapshot.go:469`) — because the reference Docker bridge has none.

[[network-gateway]] is the runtime that enforces the capture; [[network-consumers]] is how the loop,
direct runs and remote sessions consume one. [[box-egress-poc]] is the retired experiment, not this.

## Changelog
- 2026-09-10 — created for the shipped feature (S0–S6, 54be097…f3e1d96), replacing eleven cards
  written for the abandoned custody design. Verified against the sources above.
