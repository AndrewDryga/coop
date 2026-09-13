---
name: trusted-git-view
description: host git runs under a coop-owned GIT_DIR view with an allowlisted config, so a repository's filter/textconv/merge drivers never execute on the host; what the view carries, what must run on the real git dir, and the recovery consequences
subsystem: forkspace
sources: [internal/forkspace/gitview.go, internal/forkspace/gitview_config.go, internal/forkspace/git.go, internal/cli/util.go, internal/forkctl/git.go, internal/forkctl/merge.go, internal/forkctl/land.go, internal/cli/sign.go, internal/sessionsvc/workspace.go, internal/sessionsvc/companion.go, internal/tasks/git.go]
updated: 2026-09-13
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

## Changelog
- 2026-09-13 — documented the trusted view as the source of session pinned clones.
- 2026-09-06 — created with the trusted view (task close-host-git-driver-execution-paths), after the release audit reproduced host driver execution through includes and the enumerate-then-execute race; design reviewed read-only against git 2.39.5 (HEAD copy, auto-gc, ref-store runner, linked worktrees).
