---
name: trusted-git-view
description: host git runs under a coop-owned GIT_DIR view with an allowlisted config, so a repository's filter/textconv/merge drivers never execute on the host; what the view carries, what must run on the real git dir, and the recovery consequences
subsystem: forkspace
sources: [internal/forkspace/gitview.go, internal/forkspace/gitview_sweep.go, internal/cli/tempreap.go, internal/forkspace/gitview_config.go, internal/forkspace/gitfsck.go, internal/forkspace/git.go, internal/cli/util.go, internal/forkctl/git.go, internal/forkctl/merge.go, internal/forkctl/land.go, internal/cli/sign.go, internal/sessionsvc/workspace.go, internal/sessionsvc/companion.go, internal/tasks/git.go, internal/tasks/pending_review_test.go, internal/loop/git.go, internal/loop/changes.go, internal/loop/review_packet.go, internal/loop/git_test.go, internal/loop/changes_submodule_test.go, internal/box/repo.go, internal/cli/net_cmd.go, internal/forkspace/exclude_unix.go, internal/forkspace/gitobserve.go, internal/forkspace/gitview_metadata_test.go, internal/shadowpath/shadowpath.go, internal/testutil/teststate/teststate.go]
updated: 2026-10-17
---

Every repository coop touches on the host is agent-writable (the box binds `.git` read-write), and
git executes whatever that repository's configuration names — filters, textconv, merge drivers —
as soon as an in-tree `.gitattributes` assigns one. `GitHardening` blanks the fixed-name knobs;
driver names are arbitrary, so no `-c` list can blank them, and enumerating them first
(`DriverNeutralizer`, retired 2026-09-06) missed includes/`config.worktree` and raced the agent.

**The mechanism.** `forkspace.GitCommand(ctx, dir, args...)` runs git with `GIT_DIR` pointing at a
coop-owned view directory: `config` projected from the repository's local config through an
allowlist of operational data (`gitview_config.go`: core layout flags, `extensions.*`, remotes,
branch tracking, identity — never `filter.*`, `diff.*`, `merge.*.driver`, includes, aliases,
credential helpers, submodule settings, hook/pager/editor paths); `objects`, `refs`, `logs/refs`,
`packed-refs`, `shallow`, `info/exclude` as symlinks into the real (common) git dir; `HEAD` as a
copy; `GIT_INDEX_FILE` naming the real index; no `hooks`, `info/attributes`, `config.worktree`,
`modules`. Git reads exactly the bytes coop wrote, so there is no enumerate-then-execute race.
`gc.auto`/`maintenance.auto`/`fetch.prune` are off under the view (a pack-refs rewrite would land
in the view). Views live under `~/.local/state/coop/gitviews/<hash of git dir>` (`COOP_GITVIEW_ROOT`
overrides, and a test binary that leaves it unset is refused; see [[test-state-isolation]]). The
daily temp sweep calls `SweepGitViews` (`gitview_sweep.go`), which removes a view only when its
`objects` link names a store that no longer exists: eval, session and temporary repositories left
theirs behind for good (on one Mac 3,589 were created in the first half of October 2026, and 13,849
of 13,990 were dangling on 2026-10-16). A view being laid out has no link yet and is kept; so is one
whose store cannot be checked, and one holding an unfinished rebase or merge (only the view holds
that state, and its repository may be on a disk that is not mounted). An entry that is not a plain
directory is never followed, a test binary sweeps only a root it was pointed at, and a process
whose cached view was swept lays it out again on its next command.

Separate host processes share that persistent view. Config parsing and atomic file publication
therefore use invocation-private temporary files, closed before reading or renaming and removed
only by their owner. Concurrent first population accepts an existing symlink only when its target
matches exactly. This supports ordinary concurrent reads of a stable repository; it does not make
concurrent rebases or branch mutations transactional. Pending-review branch reads retain actual
Git/view failures instead of reporting them as branch loss or drift.

**Reading box-writable metadata.** Every file a view reads by content (`.git` pointer files,
`commondir`, a linked worktree's `gitdir` backlink, `HEAD`, the config) goes through
`readGitMetadata` (`exclude_unix.go`; `readGitFile` is its 64 KiB form): it follows a link as git
does, opens with `O_NONBLOCK`, requires a regular file by `fstat`, and refuses one over its limit
before reading. Limits: 64 KiB for pointers and HEAD, 4 MiB for the config (read on every refresh),
1 GiB for the `packed-refs`, `shallow` and index that only the fsck and observation snapshots copy
(`gitFileTooLargeError` names the file and both sizes; an oversize index also gets
`git update-index --index-version 4`, which git documents as 30-50% smaller). A plain
`os.ReadFile` there waits forever on a FIFO with no writer and reads `/dev/zero` without end.
In-process, `gitViewsMu` guards only the view map and root; each view refreshes under its own `mu`,
so a stalled refresh holds up only that repository (`TestGitViewStalledRefreshNeverHoldsUpAnotherView`
stalls one view with a PATH shim). The environment (`GIT_WORK_TREE`) is built under that `mu`, and a
view whose first refresh fails is dropped from the cache, or a later command reaching its git dir by
another path would clear the operation state an earlier coop left in it.

**Git's own repository search.** Git searches for a repository from its working folder upward even
for `git config --file`, and opens any `HEAD` it finds without `O_NONBLOCK`. The config parse
(`listGitConfig`) therefore runs with `cmd.Dir` at its scratch folder (the view, or the snapshot)
and `GIT_CEILING_DIRECTORIES` at that folder's parent: without the first, a FIFO at `.git/HEAD` of
coop's working folder hung every refresh in the process; without the ceiling, a view root or
`TMPDIR` inside a box's repository did (`TestGitViewNeverSearchesTheFoldersAroundIt`). The checkout
settings query (`includeTrustedCheckoutConfig`) gets `GIT_DIR` = the resolved git dir instead of
`-C <worktree>`: `includeIf "gitdir:"` still matches the real repository, and git never climbs
into the folders above it when it refuses the worktree's `HEAD`. What git itself opens is not
covered: the commands under a view or snapshot open the real index, refs and objects with blocking
opens, and the checkout settings query opens the real `HEAD` and config. Such a stall hangs only
that command, which holds no Coop lock.

**Linked worktree backlinks.** A git dir with its own `commondir`, reached through a `.git` file or
link, is a linked worktree's: it must sit at `<common dir>/worktrees/<name>` and its `gitdir`
backlink must name this folder's `.git`. `checkLinkedWorktree` makes that check for views and for
`coop approve` (`safeExcludeTarget`) alike. It compares the backlink's folder with the link-free
worktree folder by identity (`os.SameFile`), so case-insensitive disks and `/var` →
`/private/var` spellings agree; it never compares the `.git` files, because a box can replace its
`.git` with a symbolic or hard link to another worktree's, which is that very file. Views read the
backlink following links as git does, approve refuses links. It suggests `git worktree repair`
only when the backlink is missing, empty or names a worktree that is gone. When the named worktree
still exists the pointer is a hijack, and repair run from the pointing folder would hand it the
other worktree's git dir. Not covered, because git keeps no backlink there: a `.git` file naming
another repository's own git dir (the separate-git-dir and submodule layout), and a `commondir`
planted in a real `.git` folder, which views follow as git does.

Tests that deliberately invalidate repository configuration must restore the owned fixture's
original bytes without repository discovery. Git 2.55 rejects a format-0 repository containing
`extensions.refStorage=reftable`, including `git config --unset`; using that cleanup hides the
later detached/malformed-HEAD checks. The pending-review regression restores its saved config
directly while keeping the unsupported-store refusal assertions unchanged.

**What must NOT run under the view** — anything that replaces a top-level git-dir entry by rename
or edits the worktree list: `branch -D`/`tag -d` of packed refs, `pack-refs`, `update-ref`, a
writing `symbolic-ref`, `worktree add/remove`, `git config` reads/writes of the repository's own
config, `init`, and path queries (`rev-parse --git-dir/--git-common-dir/--git-path` would name the
view). Those use `forkspace.GitRefCommand` on the real git dir; none passes file content through a
driver. A branch switch is `GitSwitchBranch` (real `symbolic-ref`, then a view `reset --hard`) and
a detach is `GitDetach`; `checkout -b`/`-B`/`switch` under the view would rewrite only the view's
HEAD. The view reconciles HEAD with the real file by latest write before each command (a finished
or aborted rebase re-attaching HEAD wins over the stale real file; a real `symbolic-ref` wins over
the view), except while a rebase/merge/cherry-pick is in flight in the view.

**Consequences.** `rev-parse --show-toplevel` answers `GIT_WORK_TREE`; `rev-parse --git-path
rebase-merge` answers the VIEW, which is where coop's own interrupted rebases live and where
`coop fork merge` recovers them (`recoverInterruptedRebase`); rebase state in the repository's own
git dir is refused, never aborted (an abort checks files out with that config). `info/grafts` is
invisible to host git (a history rewrite the view does not carry); a real `shallow` file is
honored. A view cached in one process follows a worktree path reused by a new `worktree add` (the
re-sign scratch) and drops the previous life's operation state. The regression is
`TestGitViewNeverExecutesRepositoryDrivers` (clean + textconv, local/included/worktree config,
status/diff/checkout/rebase, with a raw positive control).

Choosing the project never asks git: `box.FindRepo`, behind `ResolveRepo` and the `coop net` verbs'
scope, walks up from the kernel's name for the working directory to the nearest `.git` by
`forkspace.RepositoryRoot`, the rule `gitDirsOf` follows (a `.git` file is read only when it is a
regular file of at most 64 KiB, never waiting on a FIFO). `rev-parse --show-toplevel` follows
`core.worktree` from a box-writable `.git/config`, so a planted value moved the next launch's
workspace mount anywhere, the home folder included. The walk also refuses a repository another
user owns (the folder or its `.git`), and never climbs to one at the home folder or above it;
only `COOP_REPO` can name one of those. Outside any repository the working directory is still
the project. The nearest `.git` wins even when a box planted it inside the real project; the
`.coopignore` rules of the folders above still apply there ([[secret-shadow-walk]]), and the
home-folder check both walks share is `shadowpath.HomeOrAbove`. `COOP_REPO` is followed through links to its folder (`EvalSymlinks`), so a linked
override and a walk from inside the folder give one path, and the secret walk looks inside it (see
[[secret-shadow-walk]]); an override that cannot be resolved is kept as given. `EvalSymlinks` keeps
the case the override was typed in, so on a case-insensitive disk that spelling still differs.

Session workspace clones use the view itself as their local transport source. A non-local,
no-checkout clone plus an exact fetch of the validated commit avoids racing mutable loose-object
files without exposing the source repository's executable Git configuration. Companions and
session submodules need no refs or remote, so they copy one commit's history instead
(`GitCopyPinnedHistoryContext`): `pack-objects` runs in the new repository with its own fresh
config, reads the source's objects through `GIT_ALTERNATE_OBJECT_DIRECTORIES` for that one
command, and writes its temporary and final pack there, never into the source.

Session integrity checks use `RunGitFsck` instead: modern Git's reference verifier
refuses the operational view's symlinked refs root. A per-call private view copies
HEAD, refs, packed-refs and shallow through pinned no-symlink regular-file reads,
each bounded by `copyGitMetadataFile`'s limit (observation copies the index the same way);
config is safely copied then projected through the same allowlist. Objects remain
linked and the real index remains selected. These checks exclude reflogs. Temporary
metadata is removed on construction/command failures and success; cached mutable
views and real HEAD are never refreshed by an integrity check.

Immutable commit comparisons that must include changed gitlinks use both
`--ignore-submodules=dirty` and `--submodule=short`. The first overrides the view's conservative
`diff.ignoreSubmodules=all`; the second pins old/new commit IDs rather than honoring a host
`diff.submodule=diff` preference. Inline diffs start another Git inside the submodule, outside
the trusted parent view, and the parent's `--no-ext-diff`/`--no-textconv` flags do not protect
that child. The loop regression proves a submodule-local named diff driver runs in a raw
inline-diff positive control but never during production review. The loop's working-tree reads
keep their existing restrictions.

## Changelog
- 2026-10-17 — a planted nested `.git` keeps the `.coopignore` rules above it; the home-folder check
  moved to `shadowpath.HomeOrAbove`.
- 2026-10-17 — the config parse runs in its scratch folder under a ceiling; the checkout settings
  query names the git dir instead of searching; one backlink check for views and approve, by folder
  identity (comparing the .git files let a symlinked or hard-linked .git through), with repair
  advice only where repair cannot hijack; plain size refusals; a failed first refresh is not
  cached; the environment is built under the view lock. Corrected the earlier claim that every
  backlink refusal suggests repair.
- 2026-10-16 — view metadata reads are bounded, non-blocking and regular-file only; refreshes run
  under a per-view lock; linked worktree pointers need a matching backlink (task
  2026-10-16-read-trusted-git-view-metadata-without-waiting-o).
- 2026-10-16 — `ResolveRepo` follows a linked `COOP_REPO` to its folder (task
  2026-10-16-resolve-links-in-coop-repo-so-secret-decoys-stil).
- 2026-10-16 — choosing the project (`box.FindRepo`) walks to the nearest `.git` instead of asking git.
- 2026-10-16 — a test binary must name its view root; the per-process temporary root it used to
  get, and the removal of it in `CloseGitViews`, are gone.
- 2026-10-16 — the daily temp sweep removes views whose repository is gone (`SweepGitViews`).
- 2026-10-10 — reproduced the pending-review fixture cleanup failure on actual Git 2.55 and
  restored saved config bytes, retaining unsupported-store, detached and malformed-HEAD checks.
  Swept all three reftable fixtures: the view fixture ends with refusal, and the config-parser
  fixture uses a standalone file; neither has the same repository-discovery cleanup bug.
- 2026-10-10 — actual detached startup plus status polling exposed shared config.copy removal:
  seven of eight real concurrent readers failed config parsing. Added private parser/publisher
  scratch, exact-target link race handling and review error preservation; persistent operation
  recovery and config allowlist unchanged. Swept the two pending-review branch authority reads.
- 2026-09-30 — reproduced healthy fsck failure on Git 2.55 and added a scoped real-refs
  projection, retaining config isolation and full reference/object checks. Tested
  packed/loose refs, linked/shallow/SHA256 repositories and invalid/special metadata.
- 2026-09-22 — restored gitlink paths, routing, stats and patches in loop commit reviews while
  forcing short submodule output. A real initialized-submodule fixture plus a mutation control
  demonstrates host driver execution if only the short-format pin is removed. Swept the loop's
  immutable comparisons; all three now retain dependency updates without entering the child.
- 2026-09-21 — moved the loop's status, review snapshot and NUL-path reads onto the trusted view.
  Fixed flags alone still executed an agent-installed clean filter on the host; the loop regression
  checks all three paths and exact whitespace-bearing filenames against a working raw-Git control.
  Swept remaining raw production runners: metadata-only queries, fresh isolated eval setup and
  trusted fixtures do not expose another repository-content driver path.
- 2026-09-13 — documented the trusted view as the source of session pinned clones.
- 2026-10-17 — companions and session submodules copy the pinned history in the new repository;
  `pack-objects` run in the source would write its temporary pack into the source.
- 2026-09-06 — created with the trusted view (task close-host-git-driver-execution-paths), after the release audit reproduced host driver execution through includes and the enumerate-then-execute race; design reviewed read-only against git 2.39.5 (HEAD copy, auto-gc, ref-store runner, linked worktrees).
