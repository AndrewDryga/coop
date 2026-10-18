---
name: coop-repo-box-go
description: coop's own boxes are filtered, so Go and the gate tools come from .agent/Dockerfile and four approved domains, the module proxy's storage.googleapis.com redirect included; module downloads share the coop-cache volume
subsystem: box
sources: [.agent/Dockerfile, .agent/project.yaml, internal/box/run.go, tools/staticcheck.mod, Makefile, .tool-versions, agent_config_test.go]
updated: 2026-10-18
---

A filtered box runs the locked client image, which carries the asdf binary but installs nothing on
start and keeps its shims off `PATH`, so a repo's `.tool-versions` never reaches it: only the
project's `.agent/Dockerfile` can put a toolchain there (`boxEnvNote` in `internal/box/run.go`). Until 2026-10-07 coop had none, and a loop agent working on coop
blocked with `go: not found` before it could run `make check`.

`.agent/Dockerfile` (from `coop init --stack asdf`) installs `.tool-versions` (Go) at build time and
adds the gate's tools at the Makefile's pins (staticcheck, govulncheck), plus shellcheck, git-lfs and
make. Changing it, or anything it copies, makes launches refuse until `coop build` runs. It is the
only source of the Go analyzers in coop's box: since 2026-10-16 Coop's own box images carry none, so
an analyzer bump moves no user's box tag or network setup, only this project's image.

Staticcheck's alternate module (`tools/staticcheck.mod` and `.sum`) pins its importer as
well as the analyzer: Go1.27.2 emits V5 export data that the published Staticcheck0.8.1 dependency
cannot read. `make install-staticcheck` uses that graph, and this project's Dockerfile installs the
same files as `go.mod`/`go.sum`. Never install with `@version` here: that would bypass the importer
pin. The gate checks both versions, and `TestOwnDockerfileInstallsTheGatePins` holds the
Dockerfile's module path and govulncheck version to the Makefile's.

The build needs the network, so `.agent/project.yaml` asks for `proxy.golang.org`, `sum.golang.org`
and `vuln.go.dev` (TLS 443), approved by the owner with `coop approve`. `proxy.golang.org` hands
large module zips (modernc.org/sqlite) to `storage.googleapis.com`, and since 21e0fb08 (2026-10-14)
the project allows that host too, so a box downloads them itself. That rule grants the whole host:
any public bucket on it, not only Go's. `GOMODCACHE` sits in `~/.cache`, the `coop-cache` volume
every box shares; the host command in the Dockerfile's comment fills that cache when a module still
fails to download in a box. Editing the Dockerfile, even a comment, marks this repo's box image
stale, so filtered launches here wait for `coop build --egress filtered`, which only the owner runs.

`coop init` in this repo also installs its generic `.githooks/` gate and sets `core.hooksPath`; coop
doesn't use them, so they were removed after running it.

## Changelog
- 2026-10-18 — the Dockerfile comment now says storage.googleapis.com is allowed and the host fill is
  the fallback; the owner chose to fix it now and rebuild once.
- 2026-10-17 — storage.googleapis.com is allowed, not blocked: `.agent/project.yaml` has granted it
  since 21e0fb08, so a box fetches the large zips itself and the host fill is only the fallback.
  Rewrote that paragraph, the description and the index line against `.agent/project.yaml` and
  21e0fb08; the matching .agent/Dockerfile comment waits for the Dockerfile's next real change.
- 2026-10-16 — the analyzer module moved to `tools/staticcheck.mod` when Coop's box images dropped
  their Go tools stage; a root test now holds this Dockerfile's pins to the Makefile's. The
  storage.googleapis.com paragraph is left to the queued stale-cards task: `.agent/project.yaml`
  allows that host since 21e0fb08.
- 2026-10-15 — the locked client image does carry the asdf binary (only its shims are off `PATH`);
  the code reference names `boxEnvNote` instead of a line range.
- 2026-10-09 — documented the shared analyzer graph and V5 importer compatibility; Go1.27.2 keeps
  current security fixes without retaining an older compiler just for lint.
- 2026-10-07 — created with the Dockerfile and the Go egress rules (task 2026-10-07-give-this-repo-s-filtered-boxes-go-and-the-gate).
