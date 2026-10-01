---
name: box-home-nested-mounts
description: Avoid bind targets that make Docker create missing application-owned home parents as root
subsystem: box
sources: [internal/box/run.go, internal/box/gitenv.go, internal/cli/doctor.go, internal/box/config_publication.go, internal/box/config_publication.js, internal/agent/claude.go]
updated: 2026-10-01
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

The same mechanism rules the restricted modes' tmpfs home: no bind may target anything under it,
which is why their seed is bound outside the home and copied in ([[restricted-execution-modes]]).

A host atomic rename can briefly expose stale content and metadata through a VM bind mount.
After defaults, normal launches carry a digest of the selected Claude `.claude.json`; the current
Coop-managed entrypoint requires matching bounded bytes and stable descriptor metadata before
starting any command, including ACP. Mismatches reopen at most three times, then fail by filename.
The witness is removed before client startup; no bytes are restored or rewritten. Claude peers
are covered too. `settings.json` is excluded because project fallbacks may legitimately overlay it.
Rebuilt managed images are required: independent custom entrypoints and later concurrent native
writes are outside this startup check.

## Changelog
- 2026-10-01 — captured a stale857-byte guest read followed by899-byte descriptor metadata and an
  exact899-byte reopen, all before native startup; documented the bounded publication check and
  project-overlay/custom-entrypoint limits. Focused stale-first-read and refusal regressions pass.
- 2026-09-10 — re-verified against run.go; noted the tmpfs-home consequence the restricted modes hit
- 2026-07-16 — created after bisecting Chromium exit 133 to the nested Git bind targets
