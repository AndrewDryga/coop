---
name: box-toolchain-on-login-path
description: "a box toolchain goes on the login PATH too, not just the `ENV` PATH"
scope: box
sources: [internal/box/image.go, internal/scaffold/templates/dockerfile/asdf]
check: "go test ./internal/box -run TestBaseDockerfileInstallsTheQualifiedClients"
updated: 2026-09-19
---

# A box toolchain must be on the login PATH, not just the ENV PATH

The box gets its `.tool-versions` toolchains (go, ruby, erlang, …) from asdf as
shims under `/home/node/.asdf/shims`. The image puts that dir on PATH via Docker
`ENV` (`internal/box/image.go`) — which only reaches the agent process and
non-login shells.

A **login shell** (`sh -lc`, `bash -l`, anything that sources `/etc/profile`)
hits Debian's `/etc/profile`, which *overwrites* PATH with a bare default that
omits the shims dir. So every asdf tool vanishes there, while node/python/git
(in `/usr/local/bin`, part of the default PATH) survive. Agents often shell out
through a profile-sourcing shell, so the gate reported `go: not found` even
though go was installed and asdf marked it current — for weeks, silently.

The base image now carries an `/etc/profile.d/asdf.sh` drop-in that re-prepends
the client launchers (`/opt/coop/bin`) and the shims for login shells, in the `ENV`
order. `image_test.go` locks it; the scaffolded asdf Dockerfile keeps the same order
with its own drop-in (`TestAsdfDockerfileKeepsToolchainsOnLoginPath`).

**Why:** `/etc/profile` resets PATH; an `ENV PATH` alone never reaches a login shell.

**How to apply:** any PATH a boxed tool relies on must be set in BOTH the image
`ENV` (non-login) AND an `/etc/profile.d/*.sh` drop-in (login). Never depend on
`ENV PATH` alone for something an agent might invoke via a login shell.

## Changelog
- 2026-06-20 — created
- 2026-08-06 — card metadata added (format v1); body unchanged
- 2026-09-19 — the qualified client launchers lead both PATHs; check renamed with its test. Swept
  the images and the asdf scaffold: each sets the order in ENV and a profile.d drop-in.
