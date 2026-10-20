---
name: hermetic-git-tests
description: "a test that runs git pins GIT_CONFIG_GLOBAL *and* GIT_CONFIG_SYSTEM — identity envs alone still let the host's config in"
scope: architecture
sources: [internal/testutil/gitrepo/gitrepo.go, internal/testutil/gitrepo/committed.go, internal/testutil/procharness/harness.go, internal/hermeticgit_test.go]
check: "go test ./internal -run TestHermeticGitTests"
updated: 2026-10-20
---

# A test that runs git answers to nothing outside itself

Any test that runs `git` against a temp repo must close BOTH config doors before the first command:

- `internal/testutil/gitrepo.New(t)` — the default. It returns a fresh repo and a `git` runner with
  both `GIT_CONFIG_GLOBAL` and `GIT_CONFIG_SYSTEM` pinned at empty files.
- A helper that builds its own env — pin `GIT_CONFIG_GLOBAL` at an empty file AND either pin
  `GIT_CONFIG_SYSTEM` the same way or set `GIT_CONFIG_NOSYSTEM=1`
  (`procharness.Environment` in internal/testutil/procharness/harness.go does the latter).

`GIT_AUTHOR_NAME`/`GIT_COMMITTER_EMAIL` are NOT isolation. They pin who commits; every other setting
still comes from the developer's machine.

**Why:** ambient config silently changes what the test observes — `commit.gpgsign` makes a fixture
commit block on pinentry, `core.hooksPath` runs the host's hooks inside the fixture,
`commit.template` rewrites the message a test asserts on, and `core.excludesFile` changes which
files `git ls-files` reports. Pinning only `GIT_CONFIG_GLOBAL` is the near miss this rule exists
for: `7fd8150` (2026-07-14) had to add `GIT_CONFIG_SYSTEM` beside a `GIT_CONFIG_GLOBAL` pin that was
already there (`TestInitGitHooks` in internal/scaffold/scaffold_test.go), and the lesson was written
down as an inbox draft that then sat unread for a month. `internal/testutil/gitrepo` (created in
`9eff655`) exists to make the hermetic form the easy one, and its package doc names the same
hazards.

**How to apply:**
- New git-touching test? Call `gitrepo.New(t)`. Reach for a hand-rolled runner only when the test
  needs a git binary or env the helper can't express — then pin both doors explicitly.
- Does the CODE UNDER TEST shell out to git? Then the pins belong in the PROCESS environment
  (`t.Setenv`), because that is the env it inherits — `gitrepo.New` pins only its own runner, so the
  fixture would be hermetic while the thing being measured is not. `internal/box/image_test.go`
  (`ignoredBuildPaths` runs `git ls-files --exclude-standard`, which reads `core.excludesFile`),
  `internal/cli/checksecrets_test.go`, and the fork tests all pin process-wide for this reason. Same
  answer, second reason: a test that plants `git config --global` needs the write and the read to
  land in one file (`internal/forkspace/git_test.go` says so at its helper).
- A fixture that reads or copies its repository right after committing also turns off automatic
  maintenance (`gitrepo`'s `noMaintenance`): since git 2.47 a commit starts `git maintenance run
  --auto --detach`, which takes `.git/objects/maintenance.lock` in the background. CI's git 2.55 hit
  the committed seed's walk with it; Apple's git 2.39 runs it in the foreground, so a Mac never does.
- Proving an isolation fix works needs `-count=1` on the focused test (and a cleared test cache
  before the full gate). A cached green result is not evidence that isolation holds — the run that
  produced it may predate the fix.
- A test that deliberately exercises host config (`internal/box/gitenv_test.go`) still pins both;
  it points them at fixture files it wrote, which is the same rule, not an exception to it.

Related: [[internal-import-dag]] (`internal/testutil/gitrepo` is a leaf every test package may import).

## Changelog
- 2026-10-20 — fixtures also turn off automatic maintenance: a detached `git maintenance run` took
  `maintenance.lock` while `gitrepo`'s committed seed walked `.git` on CI (task
  2026-10-20-keep-git-maintenance-out-of-the-committed-reposi). `TestFixtureCommitsStartNoMaintenance`
  checks it through `GIT_TRACE`. Swept the tree for test code that reads `.git` after a commit: one
  sibling, the live provider loop suite's `providerLoopLiveAdminDigest`, walks a `.git` Coop's own
  commits wrote under procharness's environment, which leaves maintenance on (queued as task
  2026-10-20-keep-git-maintenance-out-of-the-process-test-chi); the `.git/hooks` reads and the seed
  copies' own walk see no commit first.
- 2026-10-17 — named `procharness.Environment`, `TestInitGitHooks` and the five 2026-08-10 violators
  by function instead of line numbers (re-verified both pins in the first two at HEAD).
- 2026-08-10 — created, promoting the `kb/inbox/environment-sensitive-test-evidence.md` draft that
  `7fd8150` left behind (the inbox itself was a retired protocol; see the same-commit sibling
  [[shell-guards-fail-closed]]). **Swept every Go file in `internal/` + `tools/` that runs a git
  `init`** — 26 matched `git` + `"init"`, 6 of them false positives for the `coop init` command:
  **14 files** route through `gitrepo.New`, **6** pin the envs themselves
  (`box/gitenv_test.go`, `tasks/audit_test.go`, `scaffold/scaffold_test.go`,
  `testutil/procharness/harness.go` — which covers the four `initProcessRepo` process-e2e callers —
  `cli/testdata/providerfixture/loop.go`, `testutil/liveprovider/contract.go`), and **5 violate**:
  `prepareLiveRepo` in acpproxy/e2e_test.go (bare `exec.Command`, full ambient env),
  `TestStageBuildContextOmitsIgnoredOutputButKeepsAuthoredInputs` in box/image_test.go, the `git`
  helpers in cli/fork_cmd_test.go and forkctl/testhelpers_test.go, and `newDelegateHarness` in
  preset/wrapper_test.go (the middle three pin identity only). All five are reported for the
  queue, not fixed here — the rule commit carries the rule ([[small-work-to-the-queue]]).
  `check:` stays `none` until they are converted: a scan test would land red, and a red check is a
  gate nobody can keep.
- 2026-08-10 — **converted all five and graduated `check:`** from `none` to
  `go test ./internal -run TestHermeticGitTests` (`internal/hermeticgit_test.go`): a source scan
  that parses every test-serving file under `internal/` and `tools/` — comment-blind and
  tag-agnostic, so `acpe2e` fixtures count too — and fails any FUNCTION that builds a repo without
  naming `gitrepo.New`, both `GIT_CONFIG_*` doors, or `GIT_CONFIG_NOSYSTEM`. A sibling test
  (`TestHermeticGitTestsScannerCatchesTheNearMisses`) pins the verdict against synthetic fixtures,
  including the global-only near miss and identity-only, so the check is known able to fail.
  The five: `acpproxy/e2e_test.go` pins the layout's gitconfig + `NOSYSTEM` on its `init` command;
  `box/image_test.go`, `cli/fork_cmd_test.go`, `forkctl/testhelpers_test.go`, and
  `preset/wrapper_test.go` pin the PROCESS env, because in each the code under test (or the
  wrapper script) shells out to git itself. **Per-function granularity then found four more the
  file-level sweep above had hidden behind a sibling's pins** — `tasks/audit_test.go`
  (`TestReconcileInterruptedCompletions`, `TestSemanticHistoryExactSupportsSHA256Root`),
  `box/image_test.go` (`TestBoxDockerfileUntracked`), `cli/checksecrets_test.go`
  (`TestScanVisibleTreeSkipsGitignored`) — all converted here. `checksecrets_test.go`'s three
  `gitrepo.New` tests also gained a process pin (`pinGitConfig`): `candidateFiles` reads
  `core.excludesFile` from the ambient env, which the helper's child pins never reach. One
  exception is recorded, `cli/scripted_process_e2e_test.go:initProcessRepo` (its env is a parameter
  built by `procharness.Environment`), and the now-redundant per-test pins in `forkctl`
  (`lifecycle_test.go` ×2, `review_gate_test.go` ×1) were dropped — `initRepo` pins for them.
