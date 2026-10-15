---
name: preset-scaffold-publication
description: preset initialization creates every file exclusively inside the repository, refuses any existing destination and removes its own files on failure
subsystem: presets
sources: [internal/preset/scaffold.go, internal/preset/preset.go, internal/preset/template.go]
updated: 2026-10-15
---

# A preset scaffold creates every file exclusively

`Scaffold` renders `preset.yaml` (the preset name lands in its header comments) and the role
prompts, then validates them in memory with the parser `Load` uses, before anything touches disk.

`.agent` and `.agent/presets` are created when missing and refused when either is a symlink: `Load`
never follows a link in a preset's path, so a preset written through an in-repo link could not
run (os.Root alone refuses only links that leave the repository). After that check, every create
and removal goes through `os.Root` handles: one on `.agent/presets`, opened once, and one on the
new folder, opened right after its exclusive `Mkdir`. os.Root follows links that stay inside it,
so a path walked again could be redirected by a box racing the host command into a folder
`.coopignore` hides (those are read-only decoys inside the box but real files on the host). A
handle, once open, keeps pointing at the folder it opened; opening it still follows an in-repo link,
so a racing box can at most move the template files into another folder of the repository.

Any existing entry at the destination, even an empty or incomplete folder or a dangling link,
belongs to the user and comes back as `ExistsError` (`Complete` when a `preset.yaml` is reachable
through it). Every file is `O_CREATE|O_EXCL`: nothing is overwritten and no existing permission
changes. `scaffoldFiles` orders the writes with `preset.yaml` last, so an interrupted scaffold never
lists as a preset; the retry reports the leftover folder as incomplete. An ordinary failure removes
what the call created, newest first, through the handles with a non-recursive `Remove`, so the
retry is a normal create.

There is no staging, fsync or identity re-check: a same-user host process racing the scaffold is
outside the threat model (maintenance audit 2026-10-15, decision 3.5).

## Changelog
- 2026-10-15 — review: creates and cleanup moved onto handles for `.agent/presets` and the new
  folder (path-based removal could be redirected through a swapped `roles` link to unlink a file
  in a hidden folder); corrected the earlier claim that a box can already write every path.
  Write order pinned by TestScaffoldWritesPresetFileLast.
- 2026-10-15 — replaced the staged create-only bundle (private stage wrapper, pinned directory
  identities, directory fsyncs, no-replace rename) with exclusive creates inside os.Root;
  validation now runs in memory before any write, failure cleanup is a plain newest-first Remove.
  Verified against TestScaffoldPreservesEveryExistingDestination (unchanged),
  TestScaffoldFailuresLeaveNothingBehind and TestScaffoldRefusesSymlinkParents (in-repo link
  case added; both new cases fail when their guard is disabled).
- 2026-09-05 — created after reproducing prompt truncation and symlink-parent writes; mapped
  rooted staged validation, atomic collision refusal, interrupted retry and publication boundaries.
