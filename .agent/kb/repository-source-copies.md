---
name: repository-source-copies
description: skills and fallback copies retain rooted content authority and validate links only after relocation
subsystem: box
sources: [internal/box/sourcecopies.go, internal/box/run.go, internal/box/services.go]
updated: 2026-09-05
---

Skills and adapter fallback settings/hooks become writable provider-home copies. The source is
not trusted host authority: `repositorySources` pins an `os.Root`, resolves metadata to support
relative and absolute in-repository source links, then opens content only by root-relative name.
Metadata traversal may inspect outside paths; it never grants an unrooted content read.

`sourceCopyFS` deliberately does not embed `Root.FS()`: its optimized filesystem methods would
bypass nonblocking opens and let a regular-file-to-FIFO race hang `CopyFS`. Settings are regular,
bounded to 1 MiB, and copied with mode 0600. Trees preserve executable permissions and internal symlinks;
absolute internal links relocate to relative links. A completed private copy must resolve every
link inside itself before exposure, so mutable source-link validation cannot bless different bytes.

Temporary copies use canonical paths outside the repository, credential configuration, exact ACP
transcript roots and companion mounts. Inode ancestry handles case aliases; exact transcript roots
also cover supported external ACP links. Service callers pass the same configuration roots through
snapshot and override creation; daemon teardown also excludes its projected session state.
Cleanup retains a separate local list: returning nil named result slices
before deferred cleanup would otherwise forget copies prepared before a later artifact failed.

Pinned roots are not immutable snapshots of in-repository contents. Project artifacts still win
independently, inactive providers do not read their fallbacks, and the legacy shared Claude source
still requires a real directory rather than a symlink.

## Changelog

- 2026-09-05 — regression-first source containment, safe/case-alias link relocation, FIFO/bounds,
  exposed-root and partial cleanup tests; verified Go 1.26 CopyFS rather than assuming no-follow behavior.
