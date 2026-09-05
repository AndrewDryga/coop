---
name: compose-host-authority
description: sibling Compose execution uses a validated private snapshot and explicit values rather than ambient host imports
subsystem: box/services
sources: [internal/box/composecheck.go, internal/box/services.go, internal/box/serviceports.go, internal/box/run.go]
updated: 2026-09-05
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
same placement requirement. `Run` reuses the startup port list for forwarding instead of reading
the mutable source again. Teardown and independent port inspection also validate a snapshot.

This freezes configuration bytes, not bind-source filesystem identity: changing a bind path after
validation is a separate boundary. Do not describe snapshots as freezing mounted directory contents.

## Changelog

- 2026-09-05 — release-audit regressions established interpolation, encoded-scalar, implicit-port,
  source-replacement and in-workspace temporary-directory failures; added the validated snapshot
  boundary and runtime Compose configuration proof.
