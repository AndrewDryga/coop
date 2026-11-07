---
name: coop-repo-box-go
description: coop's own boxes are filtered, so Go and the gate tools come from .agent/Dockerfile and four approved domains, the module proxy's storage.googleapis.com redirect included; module downloads and the host-wide gate slots share the coop-cache volume
subsystem: box
sources: [.agent/Dockerfile, .agent/project.yaml, internal/box/run.go, internal/box/gateslot.go, tools/staticcheck.mod, tools/gateslot/main.go, Makefile, .tool-versions, agent_config_test.go]
updated: 2026-11-06
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

Because `~/.cache` is one volume for every box, `make check` runs under `tools/gateslot`, which
holds one of two flock slots in `~/.cache/coop-gate/slots` for the whole gate. Flock on a shared
Docker volume is seen across containers (checked on OrbStack 2026-11-05); a host gate has its own
`~/.cache`, so host gates and box gates count separately. A per-VM runtime such as Apple container
was not checked. Every box also mounts `coop-gate-slot` (internal/box/gateslot.go), a shell helper
on the same `slot-0`/`slot-1` files, so another project's opted-in gate and the `coop fork merge`
gate share the limit; keep its slot count equal to `tools/gateslot`'s. Both set
`COOP_GATE_SLOT_HELD` for the gate they run, and a gate that finds it set runs at once: the merge
gate's `coop-gate-slot make check` takes one slot, not two. Without that, two merges at once held
both slots and each make check inside waited forever. `make check-local` runs its `go test` step
under the same slots (`GATE_SLOT` in the Makefile); its vet steps run without one.

On Docker, a box with that helper also mounts a tmpfs at `/tmp/coop-gate` (a quarter of `--memory`,
at least 512m, 2g with no limit), and a gate with no `TMPDIR` gets a folder of its own in it
(`run.XXXXXX`): `coop-gate-slot` points `TMPDIR` and `GOTMPDIR` there for the gate it runs and removes
the folder on exit. A box's `/tmp` is on OrbStack's VM disk, where an fsync takes several times what
it takes on the host, and Go's race tests sync many small files. A gate that already has a `TMPDIR`
keeps it, and Go's folder with it. The exit trap does not run when dash is killed by a signal, and a
trap on TERM would hold a helper-only SIGTERM until the gate ends, so a killed gate leaves its folder
and each gate starts by removing `run.*` folders more than four hours old. A filtered launch accepts
exactly that tmpfs: `validateMountsContext` recomputes it from the `--memory` in the same options.
`tools/gateslot` sets none of this, so coop's own `make check` keeps `/tmp` unless it runs under
`coop-gate-slot`.

`coop init` in this repo also installs its generic `.githooks/` gate and sets `core.hooksPath`; coop
doesn't use them, so they were removed after running it.

## Changelog
- 2026-11-06 — each gate gets its own `run.*` folder in the memory folder, removed on exit and swept
  when hours old; a TMPDIR the gate has is kept; a filtered launch takes only the planned tmpfs, once
  (follow-up to task 2026-11-05-find-why-go-race-tests-run-several-times-slower).
- 2026-11-06 — Docker boxes mount a tmpfs at `/tmp/coop-gate`, and `coop-gate-slot` points `TMPDIR`
  and `GOTMPDIR` at it (task 2026-11-05-find-why-go-race-tests-run-several-times-slower).
- 2026-11-06 — a gate inside a gate runs in the outer gate's slot, and check-local's tests take a
  slot (task 2026-11-06-make-gate-slots-re-entrant-and-cover-check-local).
- 2026-11-06 — every box mounts `coop-gate-slot` on the same slot files, and the fork merge gate runs
  under it (task 2026-11-05-limit-concurrent-full-gates-for-every-project-s).
- 2026-11-05 — `make check` takes a host-wide gate slot from the shared cache volume (task
  2026-11-05-run-only-a-few-full-gates-at-once-across-a-host).
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
