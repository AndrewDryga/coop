---
name: forks-snapshot-parent-approvals
description: new forks snapshot existing host approvals with fresh identities and unchanged reviewed inputs; later parent approvals never carry over
scope: security
sources: [internal/cli/fork_isolated.go, internal/cli/fork_cmd.go, internal/box/fork_approvals.go, internal/box/caseapproval.go, internal/networkstate/fork_approvals.go, internal/box/project_build.go, internal/forkctl/rm.go, internal/forkctl/merge.go, internal/cli/fork_approval_snapshot_test.go, internal/box/fork_approvals_test.go, internal/networkstate/fork_approvals_test.go]
check: "go test ./internal/cli -run 'TestForkApprovalSnapshot|TestForkApprovalRecoveryFollowsTheRefusal|TestForkFreshTakesTheParentsApprovalsAgain'"
updated: 2026-11-06
---

# Snapshot approved inputs when creating a fork

New ordinary and isolated forks, and forks recreated with `--fresh`, inherit the parent's existing
network, service/secret, case-exposure and project-build approvals once, while creation owns the
unpublished checkout. Re-key host records to fresh fork identities, preserving the reviewed
digests, secret paths, accepted exposure paths, image pins and volume bindings. Project files and
copied public markers never grant authority. Preserve withdrawal barriers and refuse invalid parent
identities. Reopening a fork never reads newer parent grants.

**Why:** the owner decided, on 2026-11-05: "Forks should not wait for approval. When you create a
fork we should take whatever is already approved on project we fork from." Four detached forks had
refused the parent's unchanged network rules, then asked again for the project build and service
secrets. Inheriting grants only what the owner already allowed: a fork's later request still needs
`coop approve` in the fork, and approvals made later in the parent do not reach it.

**How to apply:** use the existing admission checks on the fork's own inputs. Copy the build's
reviewed-files digest; its image-output proof includes a path-derived tag and cannot be re-keyed.
Retire previous image and review grants when reusing a fork pathname. Capture the parent's network
record under its lock, release it, then publish the fork: different IDs can share one lock stripe.
Keep controller-job and disposable review-candidate policy ownership separate. A refused detached
worker must show stopped with its reason, retaining the generation checks and machine state.
Removing a fork retires what it holds through its own markers before the checkout goes
(`box.RetireForkApprovals`), keeping withdrawal barriers and the parent's grants.

## Changelog
- 2026-11-06 — the case-exposure approval (`coop approve --case-exposure`) was not copied, so a
  fresh fork of a parent that had accepted it still stopped for it. It now copies with the service
  approvals under the fork's own anchor, and a parent can hold it alone. Recorded the owner's
  decision verbatim, named the tests in `check:`, and added the `--fresh` re-snapshot test.
- 2026-11-05 — fork removal retires the fork's own service, case-exposure, network, build and
  image grants and anchors; swept rm, `--fresh` and merge cleanup (session discards create no
  snapshot and are unchanged).
- 2026-11-05 — swept CLI new/fresh/resume/worker paths, fork ACP, review candidates and controller
  workspace creation. Added creation-only snapshots for ordinary/isolated forks and aligned local
  fork ACP build readiness with its admission. Regressions cover unchanged/changed inputs, later
  parent grants, copied markers, withdrawal, failed publication, reused image grants and colliding
  lock stripes. Controller jobs retain their independently frozen authority.
