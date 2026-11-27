---
name: session-checkpoint-object-custody
description: a checkpoint transports and scans only Git objects the restoring worker cannot fetch itself from the job's authorized source; an upstream past the base is proven by a controller-granted refresh, never by workspace refs
subsystem: sessions
sources: [internal/sessionsvc/checkpoint_artifacts.go, internal/sessionsvc/fleet_workspace_init.go, internal/sessionsvc/fleet_workspace.go, internal/sessionsvc/checkpoint.go, internal/sessionsvc/checkpoint_repository.go, internal/sessionsvc/checkpoint_restore.go, internal/sessionsvc/source.go, internal/workerproto/checkpoint_manifest.go, internal/sessionsvc/http.go, internal/workerconnector/capabilities.go, internal/workerconnector/job_sources.go]
updated: 2026-11-27
---

`writeCheckpointRepository` streams raw typed objects from `rev-list --objects HEAD <tree>` minus
what restore obtains on its own. The base (`^base` and `^base^{tree}`) is always left out. Every
transported byte passes `copyCheckpointContent`'s credential scan, including objects only
intermediate history reaches.

A rebase onto a newer default commit brings upstream objects past the base. When the base-only
export is refused for a credential, `buildWorkspaceCheckpoint` asks once for the job's current
default through `pinCurrentSessionParent` (the controller-granted `RefreshDefault`, the same
authority review uses). `checkpointUpstream` then takes the merge-base of HEAD and that default
head and keeps it only if the worker's private source copy proves base, then upstream, then
default head in ancestry. Workspace refs, remotes and config never take part, because the agent
writes them (TestWorkspaceRefsDoNotAuthorizeACheckpointUpstream). The retry scans everything it
still transports again, so a task-added credential refuses even after an upstream is found.
The answer names where the match was, never what: Git content in neither base nor default
(`checkpointTaskCredentialDetail`), an LFS file, or an untracked file, and never a path: a Git match
is a nameless object (blob, commit message, or a tree whose matched text is a file name), and an
agent-chosen name can carry the token itself (TestCheckpointCredentialRefusalNamesNoPath); a refresh that could not
run answers 503 (retry with a new key), a refused grant 409 (`checkpointUpstreamUnverified`).
All checkpoint Git commands go through `forkspace.TrustedGitCommand`, which appends
`core.commitGraph=false`, so a commit-graph the agent writes cannot change the ancestry answers.

The chosen commit travels as the manifest's optional `upstream_revision`. `fetchCheckpointUpstream`
on restore refreshes the replacement session's source, requires that exact commit in the private
copy and in the refreshed default's history, and pins it into the quarantine repository before
objects are read. It refuses before any live file changes: `repository_unavailable` (503) when
the refresh could not run or Git could not read the copy; `invalid_session_state` (409)
for a refused grant, no job authority, or a commit the refreshed copy lacks or whose default
history no longer contains it. Only git's clean answers count as "gone": `cat-file -e` exit 1
with empty stderr under LC_ALL=C (`checkpointCommitMissing`), `merge-base --is-ancestor` exit 1;
any other failure stays 503. The refresh fetched the current default with full history, so
"missing after refresh" means gone from the default now; a 503 there made every controller retry
spend one more refresh. The 409 is final for its key only: the replacement session stays unused,
and a NEW key restores once the branch holds the commit again
(TestRestoreAfterTheDefaultRegainsTheUpstreamTakesANewKey). `pinCurrentSessionParent` answers 409
for a job document that does not decode (unreachable after `sessionExecution`, aligned anyway);
capture already answered 409 for a session without job authority via `checkpointUpstreamUnverified`. A resumed (running or uncertain) restore proved all
that before it touched files, so it only checks the private copy still holds the commit on the
base: a default force-pushed between crash and retry must not fence the session for good.
`validateCheckpointRepository` also checks that the upstream is an ancestor of the committed
revision.

The LFS walk (`visitCheckpointTrees`) still starts at the base, so upstream LFS payloads travel and
are scanned, and a scanner match there still refuses (`checkpointLFSCredentialDetail`). Deliberate
limit (2026-11-20): `RefreshDefault`'s `fetchLFS` hydrates only the selected commit and the default
head, so a restoring worker cannot fetch and verify LFS objects of the commits in
`upstream ^base`; leaving them out needs a restore-side LFS fetch through the source grant AND a
new bundle shape that workers already advertising `checkpoint-upstream:1` (built from e287a088)
would fail to restore, so a controller must be able to tell them apart (owner decision).
Restore's "unused LFS object" check also forbids simply narrowing the walk to `head ^upstream`
for bundles that carry those objects. A manifest without the field is the v15.0.0 shape. An older worker strict-decodes the
new field as malformed (internal error, nothing changed); workers that can restore it advertise
`checkpoint-upstream:1` (daemon `checkpoint_upstream_versions`).

Restore's `git clean -qfdx` drops empty folders and the bundle carries files only, so
`restoreTaskStateDirs` recreates the four task-state folders (a queue missing one fails `coop tasks
lint`, and a hand `mv` into it renames the task); a path already standing there is left alone.

Fleet artifact export streams through the checkpoint verifier and spools only the task
projection. The controller task draft binds the requested job. A private staging file
must have a single link before task metadata is read, even when both links are
controller-owned. Reports under the bound task's `artifacts/` can be returned without
materializing Git. Selection uses regular verified members and a fresh private destination.

## Changelog
- 2026-11-20 — restore recreates the empty task-state folders; the credential refusal names no
  path, reason recorded (task 2026-11-20-tidy-three-notes-from-the-15-2-live-pass-wide-re).
- 2026-11-20 — restore answers 409 for an upstream commit the refreshed default provably lacks; upstream LFS stays in the bundle, reason recorded (task 2026-11-20-say-why-a-checkpoint-was-refused-when-the-source).
- 2026-11-20 — refusal answers tell the causes apart; resumed restore checks locally; capability
  `checkpoint-upstream:1`; restore codes match the service (refused grant 409, copy failures 503).
- 2026-11-19 — created: upstream exclusion after a rebase onto an authorized default, with restore
  proof (TestCheckpointAfterRebaseOntoAuthorizedUpstreamRestoresOnAFreshWorker and the refusal
  tests in checkpoint_upstream_test.go); mutation-checked the refresh, the ancestry checks and the
  private-copy proof.


Operator input uses the same portable Git objects and a separate manifest. Its stable offer
reference identifies the operator request. The mTLS fetch uses the execution generation's job
reference. The worker compares the archive's repository and selected source revision against
the frozen job and installs the tree under the workspace-task mutation lock. A durable task
binding permits execution. An interrupted initialization can reapply the archive before that
binding. A bound session can replay the receipt without rewriting later edits.

- 2026-11-27 — Operator verification compares HEAD, index, tracked working tree and untracked file path/mode/content against the admitted source. Read the exact pending candidate under the runtime cleanup lock. Only the bound task's artifact subtree is excluded from source comparison; it is retained by the existing checkpoint projection.
- 2026-11-27 — Traced operator capture, generation-specific fetch, setup restrictions and
  recovery from an interrupted task binding through the worker and its Git closure checks.
