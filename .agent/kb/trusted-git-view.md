---
name: trusted-git-view
description: host git runs under a coop-owned GIT_DIR view with an allowlisted config, so a repository's filter/textconv/merge drivers never execute on the host; what the view carries, what must run on the real git dir, and the recovery consequences
subsystem: forkspace
sources: [internal/forkspace/gitview.go, internal/forkspace/gitview_config.go, internal/forkspace/gitfsck.go, internal/forkspace/git.go, internal/cli/util.go, internal/forkctl/git.go, internal/forkctl/merge.go, internal/forkctl/land.go, internal/cli/sign.go, internal/sessionsvc/workspace.go, internal/sessionsvc/companion.go, internal/tasks/git.go, internal/loop/git.go, internal/loop/changes.go, internal/loop/review_packet.go, internal/loop/git_test.go, internal/loop/changes_submodule_test.go]
updated: 2026-10-10
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
overrides; a test binary uses a temp root removed by `CloseGitViews`).

Separate host processes share that persistent view. Config parsing and atomic file publication
therefore use invocation-private temporary files, closed before reading or renaming and removed
only by their owner. Concurrent first population accepts an existing symlink only when its target
matches exactly. This supports ordinary concurrent reads of a stable repository; it does not make
concurrent rebases or branch mutations transactional. Pending-review branch reads retain actual
Git/view failures instead of reporting them as branch loss or drift.

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

Session workspace clones use the view itself as their local transport source. A non-local,
no-checkout clone plus an exact fetch of the validated commit avoids racing mutable loose-object
files without exposing the source repository's executable Git configuration.

Session integrity checks use `RunGitFsck` instead: modern Git's reference verifier
refuses the operational view's symlinked refs root. A per-call private view copies
HEAD, refs, packed-refs and shallow through pinned no-symlink regular-file reads;
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
- 2026-09-06 — created with the trusted view (task close-host-git-driver-execution-paths), after the release audit reproduced host driver execution through includes and the enumerate-then-execute race; design reviewed read-only against git 2.39.5 (HEAD copy, auto-gc, ref-store runner, linked worktrees).
