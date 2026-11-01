---
name: filesystem-qualification
description: actual-share and cgroup BPF probes are explicit diagnostics, with replacement and alias failures retained as evidence
subsystem: box
sources: [tools/fsprobe/main.go, tools/fsprobe/experiments.go, tools/fsprobe/assets/loader.c, tools/fsprobe/assets/probe.py, tools/fsprobe/RESULTS.md, internal/runtime/docker_lifecycle.go]
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
candidate; policy hardlink writes changed the host policy. `RESULTS.md` records the
scope. A finite fixture cannot authorize removing spelling mounts from arbitrary
shares or service binds. The command always leaves `Qualified` false.

## Changelog
- 2026-11-01 — added after actual-share and cgroup qualification on Docker 29.8.2;
  source review and live counterexamples preserve the separation from admission.
