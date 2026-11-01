---
name: filesystem-qualification
description: actual-share and cgroup BPF probes are explicit diagnostics, with replacement and alias failures retained as evidence
subsystem: box
sources: [tools/fsprobe/main.go, tools/fsprobe/experiments.go, tools/fsprobe/metadata.go, tools/fsprobe/assets/loader.c, tools/fsprobe/assets/probe.py, tools/fsprobe/RESULTS.md, internal/runtime/docker_lifecycle.go]
updated: 2026-11-01
---

`go run ./tools/fsprobe` creates inert fixtures on the explicitly selected share,
binds a local Docker endpoint/daemon and immutable image, and records raw controls,
mounts, host identities, and access observations. It is a developer qualification
tool for a quiescent share, never an admission grant or a startup probe. Existing
spelling mounts and case-exposure approvals are unchanged.

`--bpf` uses raw system calls in a separate privileged loader to attach six LSM
programs directly to two unprivileged workload cgroups. The cgroups retain the
programs after loader removal; the final observer checks their release. No BPF
libraries or packages are installed. Compilation uses the existing clients image's
development compiler, not a new Coop runtime requirement.

OrbStack denied original aliases and fresh ASCII accesses after host atomic saves.
New hardlinks, Unicode replacement aliases and replaced ancestors bypassed the
original candidate; policy hardlink writes changed the host policy.

The strengthened candidate adds basename/suffix rules and an experimental
multi-link restriction, and records `bpf_d_path` plus inode/link counts inside the
open hook. Suffix rules fix the tested Unicode `.pem` prefixes and ancestor saves;
secret-source mutation hooks work. Repeated runs expose stale `i_nlink` after a
host adds a link to a warmed guest path: host count 2, kernel count 1, read allowed.
Other runs see the update. The restriction also blocks legitimate public
hardlinks, while exact Unicode custom rules and existing descriptors/mappings
retain measured gaps. Do not infer freshness from a single passing run or omit
an earlier raw counterexample because a summary array failed to classify it.
The stale-link test reclassifies existing bytes. A supplementary variant that
starts public and writes new secret bytes after the host adds the alias denied
six of six fresh opens. Keep those outcomes separate from the measured new-byte
disclosure through already-held descriptors/mappings.

`RESULTS.md` records the scope and integration decision. A finite fixture cannot
authorize removing spelling mounts from arbitrary shares or service binds. The
command always leaves `Qualified` false; production protection is unchanged.

## Changelog
- 2026-11-01 — strengthened BPF qualification: actual helper support, open-time
  telemetry, guest mutations and handle lifetime; retained repeated stale-link
  failures and public-hardlink compatibility cost, with kernel map cleanup proof.
- 2026-11-01 — added after actual-share and cgroup qualification on Docker 29.8.2;
  source review and live counterexamples preserve the separation from admission.
