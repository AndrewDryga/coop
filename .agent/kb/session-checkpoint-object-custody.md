---
name: session-checkpoint-object-custody
description: a checkpoint transports and scans only Git objects the restoring worker cannot fetch itself from the job's authorized source; an upstream past the base is proven by a controller-granted refresh, never by workspace refs
subsystem: sessions
sources: [internal/sessionsvc/checkpoint.go, internal/sessionsvc/checkpoint_repository.go, internal/sessionsvc/checkpoint_restore.go, internal/sessionsvc/source.go, internal/workerproto/checkpoint_manifest.go, internal/sessionsvc/http.go, internal/workerconnector/capabilities.go]
updated: 2026-11-20
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
(`checkpointTaskCredentialDetail`), an LFS file, or an untracked file; a refresh that could not
run answers 503 (retry with a new key), a refused grant 409 (`checkpointUpstreamUnverified`).
All checkpoint Git commands go through `forkspace.TrustedGitCommand`, which appends
`core.commitGraph=false`, so a commit-graph the agent writes cannot change the ancestry answers.

The chosen commit travels as the manifest's optional `upstream_revision`. `fetchCheckpointUpstream`
on restore refreshes the replacement session's source, requires that exact commit in the private
copy and in the refreshed default's history, and pins it into the quarantine repository before
objects are read. It refuses with `repository_unavailable` before any live file changes, or
`invalid_session_state` for a refused grant. A resumed (running or uncertain) restore proved all
that before it touched files, so it only checks the private copy still holds the commit on the
base: a default force-pushed between crash and retry must not fence the session for good.
`validateCheckpointRepository` also checks that the upstream is an ancestor of the committed
revision.

The LFS walk (`visitCheckpointTrees`) still starts at the base, so upstream LFS payloads travel and
are scanned. A manifest without the field is the v15.0.0 shape. An older worker strict-decodes the
new field as malformed (internal error, nothing changed); workers that can restore it advertise
`checkpoint-upstream:1` (daemon `checkpoint_upstream_versions`).

## Changelog
- 2026-11-20 — refusal answers tell the causes apart; resumed restore checks locally; capability
  `checkpoint-upstream:1`; restore codes match the service (refused grant 409, copy failures 503).
- 2026-11-19 — created: upstream exclusion after a rebase onto an authorized default, with restore
  proof (TestCheckpointAfterRebaseOntoAuthorizedUpstreamRestoresOnAFreshWorker and the refusal
  tests in checkpoint_upstream_test.go); mutation-checked the refresh, the ancestry checks and the
  private-copy proof.
