---
name: compose-host-authority
description: sibling Compose execution uses a validated private snapshot and explicit values rather than ambient host imports
subsystem: box/services
sources: [internal/box/composecheck.go, internal/box/services.go, internal/box/serviceports.go, internal/box/run.go, internal/box/serviceshadow.go]
updated: 2026-09-06
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
the mutable source again. Teardown and independent port inspection also validate a snapshot.

This freezes configuration bytes, not bind-source filesystem identity: changing a bind path after
validation is a separate boundary. Do not describe snapshots as freezing mounted directory contents.

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

## Changelog
- 2026-09-06 — sidecars start only when no other box is running in the project (`LiveBoxes`, `internal/box/services.go`; `box.Run` skips the start, `coop up` refuses): closes the bind-source replacement race without changing live binds (human decision, task resolve-sidecar-bind-identity-without-losing-liv).
- 2026-09-06 — sidecar secret shadowing: the generated shadow override projects the primary decoys into repo binds (release-audit follow-up; real-Compose merge verified by `TestRuntimeComposeShadowsRepoSecretsIntoSidecars`).

- 2026-09-05 — release-audit regressions established interpolation, encoded-scalar, implicit-port,
  source-replacement and in-workspace temporary-directory failures; added the validated snapshot
  boundary and runtime Compose configuration proof.
