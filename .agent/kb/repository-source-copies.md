---
name: repository-source-copies
description: skills and fallback copies retain rooted content authority and validate links only after relocation; a lead's own agents are copied without following any link
subsystem: box
sources: [internal/box/sourcecopies.go, internal/box/run.go, internal/box/services.go]
updated: 2026-09-19
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
transcript roots and companion mounts. Where they are made depends on the run: an ordinary run uses
a private system temp dir, and a filtered run uses its execution's artifact directory
(`composedCopyDir` with `compositionArtifactOps.parent`), because a filtered launch accepts only
mounts that are canonical descendants of that directory and exact-owned cleanup removes it. The
exposed-roots check runs on both branches. Inode ancestry handles case aliases; exact transcript roots
also cover supported external ACP links. Service callers pass the same configuration roots through
snapshot and override creation; daemon teardown also excludes its projected session state.
Cleanup retains a separate local list: returning nil named result slices
before deferred cleanup would otherwise forget copies prepared before a later artifact failed.

A preset box's generated native roles are mounted over the lead home's agents directory, so the
definitions the user keeps there are copied in beside them (`ownAgentFiles`). That source is the
box's own writable home, which the box sees only through its mounts — the auth file a credential
broker covers with `{}`, ACP's shared session directories — so a link that stays inside the home can
still reach a file the box is not shown: `agents -> .` would copy the real credential in. Each
directory below the home is opened with `openat(O_DIRECTORY|O_NOFOLLOW|O_NONBLOCK)`, each listed
regular file with `openat(O_NOFOLLOW|O_NONBLOCK)`, and fstat must say regular and at most 1 MiB
before a byte is read. `os.Root` offers no no-follow open: `Root.OpenFile` follows a final link that
stays in the root despite its own O_NOFOLLOW, and `Root.OpenRoot` opens the last component without
O_DIRECTORY or O_NONBLOCK, so a FIFO there blocks the launch. `os.NewFile` puts an O_NONBLOCK fd in
Go's poller on every platform, so a read from a FIFO a writer holds open parks on Linux and darwin
alike — and on darwin never wakes, even after the writer leaves (golang/go#24164); a writer-less FIFO
reads EOF, so a reproduction must hold the writer open. (`os.OpenFile` is different: on darwin it
keeps FIFOs out of the poller, so the same read fails with EAGAIN there and hangs only on Linux.) A
hard link to a covered file is no way around this: the box has no path to the covered inode.

Pinned roots are not immutable snapshots of in-repository contents. Project artifacts still win
independently, inactive providers do not read their fallbacks, and the legacy shared Claude source
still requires a real directory rather than a symlink.

## Changelog
- 2026-09-19 — a lead's own agent definitions are copied into the preset box's generated agents
  directory by fd-relative, no-follow opens; recorded the two os.Root traps and the FIFO read that
  parks a launch when a box swaps one in.
- 2026-09-18 — copies follow the composition parent: a filtered run makes them under its artifact
  directory. They had gone to the system temp dir, so every filtered launch in a repository with
  shared skills or a home fallback was refused as an unowned mount, for every agent.

- 2026-09-05 — regression-first source containment, safe/case-alias link relocation, FIFO/bounds,
  exposed-root and partial cleanup tests; verified Go 1.26 CopyFS rather than assuming no-follow behavior.
