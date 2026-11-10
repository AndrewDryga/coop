---
name: supply-chain-checks
description: Lockfile auditing and final-image vulnerability coverage
subsystem: development
sources: [Makefile, .github/workflows/ci.yml, tools/imagevuln/main.go, tools/installtrivy/main.go, tools/installtrivy/checksums.txt, internal/box/base_candidate.go, internal/fsimage/Dockerfile, .agent/project.yaml]
updated: 2026-11-09
---

`npm-audit` uses the embedded-client source lock without installation or lifecycle scripts. Its
registry is explicit, user/global configuration is isolated, and HIGH/CRITICAL or operational
errors fail the static gate. Explicit online mode and all dependency classes override ambient
offline/omit settings: npm offline audit can otherwise return a false zero-vulnerability result. npm refuses using the same file for user and global config, so the
empty global config must have a distinct temporary path.

`image-vuln` builds all four release-owned final images and scans immutable IDs through the captured
Docker endpoint. The ordinary base candidate deliberately skips operator metadata publication and
image reclamation. Trivy is installed from a versioned archive whose hash is reviewed in-tree,
without adding its dependency graph to Coop's Go module. Every run retains JSON and build logs in a
fresh directory, including failed scans. The owner-selected policy blocks HIGH/CRITICAL findings
with a nonempty fixed version. Findings without published fixes remain in JSON and per-image
counts. Trivy exits zero for findings; the report evaluator applies the policy after validating
inventories. Scanner/build/database failures always fail, independently of finding counts.

Trivy's image mode disables lockfile analyzers: verify the installed `node-pkg` inventory and each
direct client's exact package path/version; the separate npm audit covers the full cross-platform
lock. The filesystem image is scratch, so it explicitly carries its four runtime libraries' dpkg
status and Debian identity. Trivy's Debian analyzer requires `/etc/debian_version`; os-release alone
is insufficient. Never copy the whole build-stage package database, which names absent compilers.

Scans identify known package advisories, not exploitability or coverage of opaque native client
internals. They do not replace locked-client request-shape and runtime qualification.

## Changelog
- 2026-11-09 — added Make-owned npm and image checks; verified npm's config-path restriction and
  Trivy's Debian/node package detection against real scans and v0.72.0 analyzer sources.
