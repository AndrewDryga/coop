---
name: box-home-nested-mounts
description: Avoid bind targets that make Docker create missing application-owned home parents as root
subsystem: box
sources: [internal/box/run.go, internal/box/gitenv.go, internal/cli/doctor.go]
updated: 2026-07-16
---

Docker prepares bind targets before the image's non-root user starts. If a generated mount targets
a nested path under a missing home directory, Docker creates the parent directories as root. Coop's
old Git mounts under `~/.config` therefore made that shared config directory non-writable and caused
Chromium's crashpad handler to SIGTRAP before browser startup.

Generated Git artifacts now mount at the direct home children `~/.coop-git-hooks` and
`~/.coop-gitignore`; the curated `~/.gitconfig` points Git at both. Keep generated mounts out of
nested, application-owned home parents unless their ownership is guaranteed for arbitrary project
images. `coop doctor` guards the underlying contract by writing a throwaway directory below
`~/.config` from a normally composed non-root box.

## Changelog
- 2026-07-16 — created after bisecting Chromium exit 133 to the nested Git bind targets
