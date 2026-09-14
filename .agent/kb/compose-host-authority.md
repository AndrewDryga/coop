---
name: compose-host-authority
description: sibling Compose execution uses a validated private snapshot and explicit values rather than ambient host imports
subsystem: box/services
sources: [internal/box/composecheck.go, internal/box/services.go, internal/box/serviceports.go, internal/box/service_observation.go, internal/runtime/service_observation.go, internal/box/run.go, internal/box/serviceshadow.go, internal/box/serviceapproval.go, internal/box/sweep.go, internal/forkspace/execution.go]
updated: 2026-09-15
---

Sibling service definitions are agent-writable but execute on the host daemon. `readValidatedCompose`
reads one bounded regular file through the workspace root, rejects unknown directives and host
environment imports, and requires every published port to name loopback. `ports: [5432]` publishes
a randomly assigned host port; only `expose` leaves publication to Coop's loopback override.

YAML scalar decoding precedes the import check, including `!!binary`; mapping keys are literal, but
aliases used as values are checked. Escaped `$$` container variables and literal environment values
remain supported. Bind and port strings retain their stricter no-dollar rule.

`snapshotComposeArgs` writes the approved bytes into a private directory whose canonical path must
be outside the workspace, even when `TMPDIR` points inside it. Discovery and launch share that file,
the original `--project-directory`, and an empty `--env-file`; the generated port override has the
same placement requirement. Callers also exclude credential configuration and ACP transcript mount
roots, with inode-aware ancestry so case aliases cannot place an artifact back in the workspace.
`Run` reuses the startup port list for forwarding instead of reading
the mutable source again. If `compose up --wait` fails, or startup is deliberately skipped, Coop
advertises only ports backed by a bounded read-only observation of one exact project/workdir/service
container: running and unpaused, healthy when an effective healthcheck exists, and carrying the
expected `127.0.0.1` binding on the network the box will join. The skipped-start config query is
also bounded, cancellable and strict; malformed or unavailable config cannot become an empty-success
claim. Unknown, duplicate, unhealthy or mismatched observations stay absent; the original startup
error remains the diagnostic. Teardown and independent port inspection also validate a snapshot.

This freezes configuration bytes, not bind-source filesystem identity. `LockServiceLaunch` closes
that separate boundary: service startup owns the exclusive side from validation through Docker
opening the mounts, while a writable sandbox holds the shared side for the time it can replace
paths. This may delay a new start, but never serializes already-running independent stacks. Live
source contents remain live; do not describe the snapshot or barrier as freezing directory contents.

## Sidecars get the box's secret shadowing

A validated bind may still hand a sidecar the raw repo: Compose resolves `../:/repo` on the host,
so every `.env`, key, and `.coopignore`'d path the box only sees as a decoy arrives in the sidecar
verbatim. `serviceShadowOverride` (`internal/box/serviceshadow.go`) runs the primary decision
(`NewShadowDecider`) over each bind's resolved source and writes a second override beside the
frozen snapshot: a read-only decoy (empty file, or empty dir for a secret directory, pruned whole)
at `<target>/<rel>` for every shadowed descendant of a directory bind, and a decoy at the bind's
own target when the resolved source is itself a secret — symlink aliases (`./alias -> .env`) are
resolved first. Compose merges `volumes` by container target, so the override wins over the base
entry. `snapshotComposeArgs` appends it to EVERY compose invocation (start, port discovery,
teardown), so the project definition never differs between them. Sources that do not exist yet,
named volumes, tmpfs, and public files are untouched.

The shared decision checks ancestor prefixes as well as the leaf. A direct bind
of `private/notes.txt` or a directory below `private/` remains hidden when that
parent matches a rule, even though the leaf name is ordinary or allowed. This
matches primary mount/build pruning; resolved service symlink sources use the
same rule. `TestRuntimeComposeHiddenAncestors` verifies real direct-file,
directory and alias decoys plus public sibling writeback using synthetic files.

Sidecar binds get the box's secret shadowing (`serviceShadowPlan` → decoys over every hidden
source). The one exemption is a human approval of the compose file's exact CONTENT
(`serviceapproval.go`: sha256 of the validated bytes → `~/.local/state/coop/service-approvals`,
outside any box). `coop up` at a TTY asks; box auto-up never asks, it warns through `ui.Warn` and keeps
the decoys (NOT through the writer it hands compose — a box start points that at a buffer read
only on failure, so a notice written there is discarded). Decoy SOURCES live in `<state>/decoys` (`serviceDecoyPaths`), never in the per-start
snapshot dir: `up -d` outlives the command, and a bind whose source coop deleted resolves to
whatever the runtime invents on the next restart. The primary box's decoy is a temp file on
purpose — that container dies inside the same `box.Run`. Any edit to the file voids the approval — that is the whole security argument, so
never key it by path or workspace. The approval also lists the exact repo-relative PATHS it
covered (`keepDecoysOutside`): a secret appearing later under an approved directory bind keeps its
decoy, because the human never saw it. `project.Services.RequireRealFiles` is a REQUEST, never a
grant — it is committed and agent-writable, so it only labels the prompt (`ReviewFile.Requested`),
and unrequested/new files sort to the top where a human cannot miss them. `lastApprovalFor` scans
approvals by workspace+compose path so "new since your last yes" survives a compose edit, which
voids the content-keyed approval.

Networks: the development Compose project keeps its canonical-workspace identity. A logical loop
adds a hashed run owner to its project and port scope, so its network, volumes, and loopback ports
are private but stable across that run's iterations. `StopSessionServices` removes containers by
ownership label AND the project's unused networks (`RemoveProjectNetworks`); `ReapOrphanNetworks`
(hooked into the orphan box sweep, once per process) removes unused networks of any project named
like coop's (`^coop-…-<8hex>$`) — never a human's compose project. "Unused" counts stopped
containers as users (`ps -a --filter network=`), because they reconnect on the next start.

## Changelog
- 2026-09-15: added logical-run Compose/port ownership and the shared/exclusive mount-launch
  barrier; development keeps its historical project and live source mounts.
- 2026-09-13: partial and skipped starts now expose only exact-owned observed running service
  bindings on the selected network while retaining the original Compose failure; skipped discovery
  is bounded and strict.
- 2026-09-13: fixed direct descendant queries that missed hidden ancestor directories;
  native Compose fixtures reproduced the exposure before and mutation denial after.
- 2026-09-07: repo-side `services.require_real_files` request labels the approval prompt.
- 2026-09-06: session teardown removes unused project networks; orphan coop networks swept.
- 2026-09-07: hidden-file notice moved off the compose writer onto ui.Warn.
- 2026-09-07: sidecar decoy sources moved to `<state>/decoys` so they outlive `compose up -d`.
- 2026-09-06: content-keyed service secret approval (`coop up` prompt) added as the exemption to sidecar shadowing; scoped to the exact approved paths.
- 2026-09-06 — sidecars start only when no other box is running in the project (`LiveBoxes`, `internal/box/services.go`; `box.Run` skips the start, `coop up` refuses): closes the bind-source replacement race without changing live binds (human decision, task resolve-sidecar-bind-identity-without-losing-liv).
- 2026-09-06 — sidecar secret shadowing: the generated shadow override projects the primary decoys into repo binds (release-audit follow-up; real-Compose merge verified by `TestRuntimeComposeShadowsRepoSecretsIntoSidecars`).

- 2026-09-05 — release-audit regressions established interpolation, encoded-scalar, implicit-port,
  source-replacement and in-workspace temporary-directory failures; added the validated snapshot
  boundary and runtime Compose configuration proof.
