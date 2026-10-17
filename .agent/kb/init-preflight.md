---
name: init-preflight
description: init resolves stack prerequisites and renders the Dockerfile before any scaffold or hook mutation; its in-place edits (Compose, project.yaml, .gitignore) refuse links and files hidden from the box, and new files and agent links are created, checked and removed through one parent handle
subsystem: scaffold
sources: [internal/scaffold/scaffold.go, internal/scaffold/compose_add.go, internal/scaffold/projectfile.go, internal/shadowpath/shadowpath.go, internal/cli/init_cmd.go, internal/cli/init_test.go]
updated: 2026-10-17
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
refused with its own error. Parent aliases that resolve inside the repository work. Because the
edit is renamed into place, a `.gitignore` a box swaps for a link between the read and the write is
replaced, never written through, so the box's bytes cannot reach a host file such as `~/.zshrc`.
Staged edits retain permissions independently of umask and preserve the original on write failure.
No edit re-checks for a concurrent edit before the rename (a same-user host editor is outside the
threat model, audit decision 3.5), so the last writer wins. A Compose file or `.gitignore` that
appears between the read and the exclusive create is reported as existing, never overwritten.

The reader also refuses a file the box cannot see, before reading or writing a byte: the temporary
copy and the renamed result land where the box may look, and a box can write `box.compose` or link
`.agent` into a hidden folder. It judges the path the box's mounts walk, with the box's rules
(`shadowpath`). Links in the file's directory are resolved first; the directory is then opened one
real directory at a time (`openRealDir`: Lstat must find a real directory and the directory opened
must be that one; os.Root follows a link that stays inside the repository, so a link swapped in
after the Lstat is refused only when it leads to another directory, and one leading back to the
same directory still reads the directory that was judged); every name judged is the one the volume
stores (`storedName`), since a case-insensitive Mac volume opens `config/prod.yml` as
`CONFIG/prod.yml` while `.coopignore` rules match case-sensitively. Each directory's `.coopignore`
is read through the handle the edit holds (`heldPolicy`: `shadowpath.OpenTree`, then
`Snapshot.Extend`, judged by `Snapshot.Shadowed`), never by path: a box that swaps a held directory
for one without rules would otherwise pair the real file's bytes with the newcomer's empty rules.
That reader accepts only a regular `.coopignore` of at most 1 MiB that coop can read. Any other (a
link, a folder, a FIFO, one without read permission or one over 1 MiB) stops the edit as
`UnreadableCoopignore`, which names that `.coopignore` repository-relative (`.coopignore` at the
root) and why; `coop init` reports it as "Could not read <that file>" with its own fix line. The
box's visibility rules (`NewDecider`) skip such a file, so for a link, a folder or a FIFO the reason
says it hides nothing from the box.

The refusal is `HiddenFile`; it names the file, plus the hidden path a link or another spelling
leads to. `coop init` says to stop hiding the file in `.coopignore`, or to fix the path when a link
leads there, and `coop init --services` to point `box.compose` at a file the box can see.
`coop init` checks `.gitignore` before it writes anything (`CheckGitignore`), so a hidden or linked
one leaves the repository unchanged; registration edits `.agent/project.yaml` last, so a refusal
there keeps the files already written. The refusal holds even when no edit is needed, so a
`.coopignore` that hides `.gitignore`, or a root `.coopignore` coop cannot read, stops every
`coop init`. A file whose directory does not exist yet is created unjudged: it holds only Coop's
own lines. Paths are judged as they are when init runs, while the box's decoys were placed at
launch.

New files take the same discipline in `writeNewRepoFile`: it opens the file's parent once as an
`os.Root` handle, creates the file there with `O_EXCL`, and after a failed write checks and removes
it by name through that handle. An `os.Root` call on a nested path walks every parent again, so a
check and a removal by that path are two walks: a box that fails the write (say by filling the
disk) and swaps a parent for a link between them could have the removal take a same-named file the
link reaches, such as one `.coopignore` hides. The parent is opened as `<dir>/.`: os.Root opens
the last component of a path without `O_DIRECTORY`, so opening the folder itself blocked init on a
FIFO a box swapped in after `MkdirAll`, while `<dir>/.` makes every folder on the way a directory
open and still follows in-repo parent links. The handle reports a bare name when a create fails
and spells its open files `<dir>/./<name>`, so `repoPath` puts the path `coop init` prints back
on those errors. The in-place reader above still opens its folder by name, so a FIFO swapped in
for that folder blocks it the same way.

Agent links use the same helper. `linkIfAbsent` (CLAUDE.md and GEMINI.md to AGENTS.md) and
`linkSkillsIfAbsent` (each selected agent's `skills` to the shared source) replace a link, never a
file or folder, and open the link's folder with `openRepoParent`, the helper `writeNewRepoFile`
uses, then check, remove and create the link by name through that one handle, so a folder swapped
for a link after the check cannot redirect the removal.
`linkSkillsIfAbsent` still follows an existing link by path to keep one that leads to a folder,
since that folder may be outside the repository; a swap there only changes whether the link stays.
The handle reports a failed link by its bare name, so the link is returned as a `create`
`*fs.PathError` on its path and init prints "Could not create .claude/skills: …" as for a file.

## Changelog
- 2026-10-17 — agent links went by absolute path, so a box that swapped `.claude` for a link
  between the check and the removal could have the host delete a `skills` entry wherever the link
  pointed; they now go through one parent handle.
- 2026-10-16 — agent links moved onto the parent handle: `linkIfAbsent` checks, removes and
  creates through `openRepoParent` (shared with writeNewRepoFile) instead of by absolute path, and
  a failed link names its path. Pinned by TestLinkIfAbsentIgnoresSwappedParent (a swap to an
  in-repo folder and to one outside; both fail on the old by-path calls and on a by-path removal
  alone) and TestLinkIfAbsentErrorsNameTheLink (fails on the handle's bare-name error).
- 2026-10-17 — second review (same task). Any `.coopignore` the snapshot reader refuses now stops
  the edit as `UnreadableCoopignore`, naming that `.coopignore` and why, instead of a bare read
  error blamed on the edited file ("Fix .agent/compose.yml"); `coop init` gives it its own headline
  and fix (`reportUnreadableCoopignore`). `shadowpath` exports `MaxCoopIgnoreBytes` and its read
  error names the `.coopignore` rather than `in ""`. Pinned by
  TestInPlaceEditsRefuseAnUnreadableCoopignore (link, folder, FIFO, over 1 MiB, no read permission)
  and TestInitNamesAnUnreadableCoopignore (the .gitignore check, `--services`, registration), both
  failing with the old pass-through. The `openRealDir` wording now says what holds: os.Root follows
  an in-repository link, and the identity check refuses it unless it leads back to the same folder.
- 2026-10-16 — review fixes (same task). The `.coopignore` rules are now read through the
  directories the edit holds (`heldPolicy`), not by path: a reviewer reproduced a box swapping a
  held `config/` for a rule-free one during a slow listing and getting the secret staged in the
  moved folder; TestHiddenCheckReadsRulesFromHeldDirectories fails with the path-based decider.
  `coop init` checks `.gitignore` before writing (`CheckGitignore`); the refusal type is the
  exported `HiddenFile` (no `ErrHidden`), the remedy says to stop hiding a `.coopignore`-hidden file
  (`initFix`, TestInitFixMatchesTheRefusal), and registration names `.agent/project.yaml`
  repo-relative. Added nested `.coopignore` cases; TestInPlaceEditsJudgeStoredNames now runs on
  case-sensitive volumes too, where another spelling makes a new file and the hidden one stays put.
- 2026-10-16 — the reader refuses a file hidden from the box (`ErrHidden`), judged by
  `shadowpath.NewDecider` on the stored names of its real path, before reading or writing (task
  2026-10-16-coop-init-never-writes-a-temp-file-beside-a-path; a box could point `box.compose` at
  `config/database.yml` and read the temporary copy). Parent aliases now resolve with EvalSymlinks
  and open link-free, so ones that resolve inside the repository work even when absolute. Pinned by
  TestInPlaceEditsRefuseHiddenFiles (fails with the check off; its two link cases fail when the
  given path is judged), TestInPlaceEditsJudgeStoredNames (fails when the given spelling is judged;
  skips where case matters), TestOpenRealDirRefusesALink, and in the CLI by
  TestInitServicesRefusesHiddenCompose and TestInitRefusesAHiddenGitignore.
- 2026-10-16 — the create-only path keeps one parent handle, opened as `<dir>/.` so a FIFO parent
  fails at once, and removes a partial file by name through it, no longer by checking and removing
  the repo path (two walks a swapped parent could split). Pinned by
  TestWriteNewRepoFileCleanupIgnoresSwappedParent (fails on the old cleanup and with a by-path
  removal), TestWriteNewRepoFileFailsFastOnAFIFOParent (hung 6 of 6 runs on a plain
  `OpenRoot(dir)`), TestWriteNewRepoFileFollowsAnInRepoParentLink,
  TestWriteNewRepoFileErrorsNameTheRepoPath and 13w-init-write-failed. Probed the in-place reader:
  `AddComposeServices` blocks in `readProjectFile` on a FIFO at `.agent`; `project.Load` refuses
  that first in the CLI, so there it takes a race.
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
