---
name: host-signing-starts-at-first-unsigned
description: every host signing pass (loop cycle, end-of-run sweep, run-box exit, coop sign) rewrites only from the first unsigned commit and refuses to rewrite a commit the branch's upstream already has; -f stays, and "signed" means a signature header
subsystem: loop
sources: [internal/cli/sign.go, internal/loop/loop.go, internal/loop/host.go, internal/cli/commands.go]
updated: 2026-10-15
---

Box commits are unsigned (no key enters a box), so the host signs them through `signUnpushed` in
`internal/cli/sign.go`. Four passes call it, each with its own requested base: the loop after a
successful iteration (`iterHead`), the loop's end-of-run sweep (`loopStartHead`), the exit of an
interactive or `coop run` box (`signOnBoxExit`, HEAD before the session), and `coop sign`
(`@{upstream}`, or `--from`).

The requested base only bounds what may change. `signingStart` walks the range oldest first
(`rev-list --reverse --header`) and moves the base past the signed commits the range starts with,
so the rebase begins at the first unsigned commit. Until 2026-10-15 every pass ran `rebase -f` from
the requested base and re-signed everything: the end-of-run sweep rewrote every commit since the run
started, including commits pushed mid-run and outside agents' commits in the same checkout, and
orphaned every SHA anyone had recorded. Its comment called it "normally a no-op"; now it is one.

Traps:
- **`-f` must stay.** The start is the exact parent of the first commit to sign; without `-f`, git
  reports the branch up to date and signs nothing.
- **Only the leading signed run is skipped.** A signed commit above an unsigned one is replayed with
  it, because its parent changes. Per-cycle signing (the owner's choice) keeps that window small.
- **Pushed means reachable from `<branch>@{upstream}`**, the remote-tracking ref. When the first
  unsigned commit is pushed, the pass refuses (`pushedCommitRefusal`) instead of signing around
  it; plain `coop sign` starts at the upstream, so it is the fix the refusal names. The check runs
  twice, before the rebase and again under the ref-authority lock just before the compare-and-swap,
  because a push can land while the rebase runs (seconds, or minutes at a pinentry prompt). No
  upstream, or a gone tracking ref, leaves no local record of what was pushed, so the requested
  range stands. Use the short branch name: `refs/heads/main@{upstream}` does not resolve.
- **Signed means a `gpgsig` or `gpgsig-sha256` header** (`commitSigned`), read from the header only
  and never verified: `%G?` reads a signed SSH commit as unsigned without an allowedSignersFile,
  and a commit anyone signed is not Coop's to re-sign. So a box can keep its own commit from being
  re-signed by signing it first, with a key of its own (the commit then verifies as that key, not
  yours) or with a forged header (it verifies as nothing). Either also silences the unsigned-commit
  marker in `coop prompt`. Neither runs anything on the host or rewrites pushed history. A box
  cannot split a `--header` record to choose the rebase base: git stops printing a message at an
  embedded NUL (probed with a `hash-object --literally` commit), so every record starts with the ID
  rev-list printed.
- **Merges are still refused over the whole requested range**, before trimming, so a signed merge
  below the first unsigned commit still stops the pass, as it did before.
- The tree-equality check and the old-SHA compare-and-swap guard the rewritten tail unchanged, and
  the signing journal (`RecordPendingReviewSigning`) maps only those commits: review evidence bound
  to an untouched signed commit keeps its SHA. See [[signoff-scope-is-run-anchored]], and
  [[loop-range-rejects-outside-commits]] for the ref-authority lock the compare-and-swap takes.

Regressions: `TestSignUnpushedStartsAtTheFirstUnsignedCommit`,
`TestSignUnpushedNeverRewritesPushedCommits` (including a push made while the pass holds the lock),
`TestSignUnpushed` (no-op second pass, key-failure and compare-and-swap paths) and
`TestCommitSigned`, all in `internal/cli`.

## Changelog
- 2026-10-15 — created with the fix for task 2026-10-15-stop-signing-passes-from-rewriting-signed-and-pu.
  Checked every pass against `signUnpushed`: per-cycle, end-of-run and box exit all route through it,
  so the one fix covers them; fork landing (`internal/forkctl/merge.go`) is separate and replays
  only the fork's own commits onto the parent, which it must rewrite anyway.
- 2026-10-15 — review fixes: the pushed check runs again under the ref-authority lock (a push made
  during the rebase was still rewritten); corrected the forged-signature note, since a box can also
  sign with its own key and either way silences the prompt's unsigned-commit marker.
