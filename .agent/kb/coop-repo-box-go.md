---
name: coop-repo-box-go
description: coop's own boxes are filtered, so Go and the gate tools come from .agent/Dockerfile and three approved Go domains; big modules redirect to a blocked host, so the shared cache is filled from the host
subsystem: box
sources: [.agent/Dockerfile, .agent/project.yaml, internal/box/run.go, internal/box/staticcheck.mod, Makefile, .tool-versions]
updated: 2026-10-09
---

A filtered box runs the locked client image, which has no asdf, so a repo's `.tool-versions` never
reaches it: only the project's `.agent/Dockerfile` can put a toolchain there
(`internal/box/run.go:2317-2329`). Until 2026-10-07 coop had none, and a loop agent working on coop
blocked with `go: not found` before it could run `make check`.

`.agent/Dockerfile` (from `coop init --stack asdf`) installs `.tool-versions` (Go) at build time and
adds the gate's tools at the Makefile's pins (staticcheck, govulncheck), plus shellcheck, git-lfs and
make. Changing it, or anything it copies, makes launches refuse until `coop build` runs.

Staticcheck's alternate module (`internal/box/staticcheck.mod` and `.sum`) pins its importer as
well as the analyzer: Go1.27.2 emits V5 export data that the published Staticcheck0.8.1 dependency
cannot read. `make install-staticcheck` uses that graph; both embedded images and this project's
Dockerfile install the same files as `go.mod`/`go.sum`. Never install with `@version` here: that
would bypass the importer pin. The gate checks both versions.

The build needs the network, so `.agent/project.yaml` asks for `proxy.golang.org`, `sum.golang.org`
and `vuln.go.dev` (TLS 443), approved by the owner with `coop approve`. `proxy.golang.org` hands a
large module zip (modernc.org/sqlite) to `storage.googleapis.com`, which stays blocked because it
would reach any public bucket. `GOMODCACHE` sits in `~/.cache`, the `coop-cache` volume every box
shares, and that cache was filled from the host with the command in the Dockerfile's comment. Run it
again when a new large dependency fails to download in a box.

`coop init` in this repo also installs its generic `.githooks/` gate and sets `core.hooksPath`; coop
doesn't use them, so they were removed after running it.

## Changelog
- 2026-10-09 — documented the shared analyzer graph and V5 importer compatibility; Go1.27.2 keeps
  current security fixes without retaining an older compiler just for lint.
- 2026-10-07 — created with the Dockerfile and the Go egress rules (task 2026-10-07-give-this-repo-s-filtered-boxes-go-and-the-gate).
