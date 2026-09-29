---
name: session-source-selection
description: controller jobs freeze exact source bindings; the worker verifies private staging and refreshes the default separately without changing the admitted checkout
subsystem: sessions
sources: [internal/workerproto/job.go, internal/box/image.go, internal/box/network_session.go, internal/workerconnector/job_sources.go, internal/forkspace/lfs.go, internal/forkctl/merge.go, internal/sessionsvc/job.go, internal/sessionsvc/job_tree.go, internal/sessionsvc/empty_source.go, internal/sessionsvc/source.go, internal/sessionsvc/service.go, internal/sessionsvc/review.go, internal/sessionsvc/workspace.go, internal/box/authority_mounts.go, internal/session/records.go]
updated: 2026-09-29
---

A create carries one canonical controller job and its digest, not a local policy name or a
separate source selector. The job freezes the repository identity and complete SourceBinding:
default/selected/base commits, admitted tree, selector and resolved timestamp. The connector
stages the source privately before forwarding create. The service derives its path from the
source staging key and checks the exact source receipt, selected HEAD and tree; the controller
cannot supply a worker filesystem path.

The worker fetches the complete Git object closure for the pinned refs, not a partial/promisor
clone. Every recursive gitlink must exactly match the controller's repository-ID/path/commit/tree
manifest and receives its own repository-scoped read grant. Git never follows .gitmodules URLs.
Git LFS uses the fixed granted GitHub endpoint and basic transfers; .lfsconfig cannot redirect it.
Only required LFS trees are downloaded, then SHA/size-verified and copied offline into independently
owned primary, companion and review workspaces. Reuse re-verifies completeness. Noncanonical or
unsupported LFS pointer forms fail explicitly instead of becoming incomplete model inputs.

GitHub credentials live only in transient owner-only host config files outside checkouts and
exports, removed after each subprocess completes. They cannot use GIT_CONFIG_VALUE_*: Git LFS
persists those environment values in diagnostic logs. Normal completion and failure cleanup are
tested; worker-host crash cleanup of transient files remains an operational lifecycle concern.

A normal job with source:null uses a deterministic empty Git baseline in private worker state
and an ordinary per-session fork. This retains tools and semantic repair without inventing a
repository identity or weakening bare mode. Its local freshness receipt uses the durable create
timestamp; review needs no GitHub refresh. Independently authorized companions still stage and
produce receipts. The shared seed is never mounted in the sandbox, and unexpected seed content
or remotes are refused. No source means no GitHub publication authority.

The session persists the job document and digest before execution. Restart, turn, prepare and
review derive settings from that document and compare the saved session projections against it,
including each companion's name, repository, workspace and selected commit. The initial turn
budget is extendable; source and execution authority are not. Historical rows without a job
remain inspectable and eligible for exact-owned cleanup, never a fallback execution path.

Job workspaces and companion snapshots live below Coop's otherwise protected session state.
The box mount guard admits only the reserved fork and that session's companion checkouts beneath
owner-private service directories;
companions remain read-only. The source mirror, control socket, parent directories, symlinked
roots and another session's files are never sandbox mounts. A host alias above the service state
(such as `/var` on macOS) is canonicalized; an alias within its job-source tree is not trusted.
The box image never comes from the job repository: an open or offline job runs the worker's
base, a filtered one Coop's locked client image ([[box-base-image-tags]]).

Review revalidates the persisted job and carries its saved network mode and capture reference to
the host-owned gate. An explicit operator `COOP_GATE` is selected before parsing the repository's
project file; without one, the trusted parent supplies the gate. The candidate cannot choose its
own checker. The gate runs as a controller job with no project box settings or ambient runtime
arguments, homes, env file or MCP. Filtered review reopens the exact owner-keyed job snapshot and
qualification rather than capturing fresh rules. Only the disposable checkout for this review
operation is mounted from private session state; the source mirror, other candidates, and parent
state remain fenced. Ordinary local fork gates retain their separate project policy.

Review and changes need a fresh upstream default, not the private staged HEAD. SourceRefresher
is the trusted-host boundary for that fetch. It must use the saved job and source, import the
current default's objects without updating a ref, and leave staged HEAD at the original selected
commit. The trusted Git view does not carry FETCH_HEAD. Missing refresh authority refuses
review/change checks; discard can conservatively compare the original local parent so an
expired credential cannot orphan a workspace or silently waive unmerged-loss consent.

Session forks still clone a trusted Git view with --no-local --no-checkout and explicitly
fetch the selected commit. Neither a branch advance nor an idempotent create retry reselects
code. The API exposes source metadata through explicit DTO projection, never JobDocument.

## Changelog
- 2026-09-29 — reverified review against saved job execution and network authority. The prior
  gate discarded that authority and used ordinary project admission; the dedicated review path
  now reopens the saved capture and exact-allows only its operation-specific scratch checkout.
- 2026-09-28 — a job repository's Dockerfile resolved a shared, never-built `coop-repository`
  tag on the worker; jobs now run the worker's base image (see [[box-base-image-tags]]).
- 2026-09-27 — live v2 turn exposed the mount guard rejecting its own staged fork; admitted only
  generation-bound controller-job workspace and session-owned read-only companion snapshots.
- 2026-09-27 — implemented full Git/recursive source custody, scoped LFS transfer and offline
  materialization; verified cold/warm, nested, hostile-URL, zero-size and model-edited binary paths.
  Reproduced LFS diagnostic credential retention and eliminated secret-valued environment entries.
- 2026-09-27 — added empty private workspaces for repository-free normal jobs; checked concurrent
  creation, restart/replay, independent forks, tamper refusal, tools and semantic admission,
  companion staging, changes/review and discard. Bare restrictions remain unchanged.
- 2026-09-26 — replaced the retired local selector/policy machinery with the persisted job
  contract; verified exact source projection and replay tests. Production refresh wiring,
  complete LFS/submodule staging and source cache remain part of the active cutover task.
- 2026-09-13 — pinned non-local clones avoid concurrent loose-object hardlink races.
- 2026-09-11 — recorded source custody traps: fetching a cached OID may short-circuit without
  contacting the remote; a local Git transport is not proof of hosted-server OID refusal.
