---
name: init-preflight
description: init resolves stack prerequisites and renders the Dockerfile before any scaffold or hook mutation; its in-place edits (Compose, project.yaml, .gitignore) refuse links
subsystem: scaffold
sources: [internal/scaffold/scaffold.go, internal/scaffold/compose_add.go, internal/scaffold/projectfile.go, internal/cli/init_cmd.go, internal/cli/init_test.go]
updated: 2026-10-16
---

# Stack validation precedes scaffold writes

`scaffold.Init` resolves its stack, verifies the explicit asdf prerequisite and renders any
Dockerfile before creating directories, writing instructions/configuration, or configuring Git
hooks. Invalid stacks fail even when `.tool-versions` exists; explicit asdf still requires that
file on re-init, including when a customized Dockerfile already exists.

The existing create-only write path owns the Dockerfile and its auto-detection announcement.
Tool parsing and unknown-tool acceptance are unchanged. The CLI calls Compose and global MCP
scaffolding only after Init succeeds. First-run prompts remain CLI-owned and precede Init; this
preflight does not introduce a second CLI validation implementation.

Additive `init --services` edits, subproject registration in project.yaml and Coop's stanzas in the
root `.gitignore` share one reader and one staged replacement (temporary file, then rename). The
reader opens the file's directory once as an `os.Root` handle, and the read, the temporary file and
the rename all go through it: a parent a box swaps for a link mid-edit cannot carry content read in
one directory, say one `.coopignore` hides from the box, into another the box can read. os.Root
follows a link that stays inside it even with `O_NOFOLLOW`, so the reader also compares the opened
file with the named entry. Leaf symlinks (in-repo ones included) and nonregular files, FIFOs
included, are refused as "<file> must be a regular file"; that refusal matches `ErrNotRegular`, so
`coop init` says to fix the path, not its permissions. An outside parent or a lexical escape is
refused with its own error. Relative parent
aliases inside the repository work. Because the edit is renamed into place, a `.gitignore` a box
swaps for a link between the read and the write is replaced, never written through, so the box's
bytes cannot reach a host file such as `~/.zshrc`. Staged edits retain permissions independently of
umask and preserve the original on write failure. No edit re-checks for a concurrent edit before the
rename (a same-user host editor is outside the threat model, audit decision 3.5), so the last writer
wins. A Compose file or `.gitignore` that appears between the read and the exclusive create is
reported as existing, never overwritten.

## Changelog
- 2026-10-16 — `.gitignore` joined the in-place edits. updateGitignore had used os.ReadFile and
  os.WriteFile, which followed a link a box planted (out to a host file such as ~/.zshrc, or to an
  in-repo file `.coopignore` hides); a missing file is now created exclusively. The regular-file
  refusal now matches ErrNotRegular, so init's remedy no longer mentions permissions. Pinned by
  TestUpdateGitignoreRefusesLinks (fails on the old read and write) and
  TestUpdateGitignoreReplacesALinkSwappedInBeforeTheWrite (fails when the edit is written by name
  instead of renamed).
- 2026-10-15 — review: the read now refuses in-repo leaf links (opened file compared with the named
  entry; os.Root had followed them, so a link to a hidden file could be copied into a box-readable
  one), and the read, temporary file and rename share one directory handle. Pinned by
  TestInPlaceEditsRefuseLeafLinks and TestRegisterSubprojectsEditStaysWhereItRead, both of which
  fail with their guard disabled.
- 2026-10-15 — removed the concurrent-edit check: no identity/content re-check before the rename,
  no three-attempt registration retry and no "kept changing" refusal from coop init. Rooted reads,
  FIFO refusal, umask-independent permissions and write-failure preservation are unchanged (the
  identity check at the file itself came back with the review fix above); sources now name
  init_cmd.go, where cmdInit lives.
- 2026-09-13 — verified additive Compose edits against rooted helpers and synthetic
  outside-link/partial-write/intervening-edit/FIFO/umask regressions; no service startup.
- 2026-09-05 — created after reproducing fresh scaffold writes and re-init Git-config rewrites
  before stack rejection; direct and CLI regressions snapshot complete isolated trees and retry.
