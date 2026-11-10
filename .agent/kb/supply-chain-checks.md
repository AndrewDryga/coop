---
name: supply-chain-checks
description: Lockfile auditing and final-image vulnerability coverage
subsystem: development
sources: [internal/box/image.go, internal/gatewayimage/Dockerfile, Makefile, .github/workflows/ci.yml, tools/imagevuln/main.go, tools/installtrivy/main.go, tools/installtrivy/checksums.txt, internal/box/base_candidate.go, internal/fsimage/Dockerfile, .agent/project.yaml]
updated: 2026-11-10
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

The Node base's npm is replaced by a complete, hash-checked npm distribution after client
installation. Remove its old directory before extraction so obsolete bundled dependencies cannot
survive; existing npm/npx links point into the replacement tree. The source client lock is
unchanged. A root guard keeps image npm, the Make audit pin and this repo's development image
on the same reviewed release.

asdf's unchanged release source is compiled in the existing pinned Go helper stage because
upstream's latest binary release still embeds an affected Go runtime. Verify the immutable source
archive before extraction, forbid implicit toolchain downloads and module-file updates, and copy
only the static executable into both final images. The source checksum is architecture-neutral;
the existing per-platform image build selects the native compiler target. No host Go is needed.

Image OS fixes retain their existing distro families. Both ordinary and fresh Node bases stay on
Node24/Bookworm; the gateway retains its Envoy digest and installs exact patched Jammy packages
from a dated snapshot. Updating one base image cannot patch the other image's OS packages.

## Changelog
- 2026-11-10 — rebuilt unchanged asdf0.20.2 from its verified source commit with the existing
  pinnedGo1.27.3 stage; removed vulnerable prebuilt downloads and architecture-specific asset map.
- 2026-11-10 — replaced vulnerable inherited npm with verified11.21.2, without lifecycle scripts
  or overlay leftovers; pinned gate/development tools to the same version.
- 2026-11-10 — patched Debian apt/PCRE2 through the same-family Node image and Jammy apt/OpenSSL
  through explicit snapshot package versions; checked vendor fixed versions and both architectures.
- 2026-11-09 — added Make-owned npm and image checks; verified npm's config-path restriction and
  Trivy's Debian/node package detection against real scans and v0.72.0 analyzer sources.
