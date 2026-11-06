---
name: forks-snapshot-parent-approvals
description: new forks snapshot existing host approvals with fresh identities and unchanged reviewed inputs; later parent approvals never carry over
scope: security
sources: [internal/cli/fork_isolated.go, internal/cli/fork_cmd.go, internal/box/fork_approvals.go, internal/networkstate/fork_approvals.go, internal/box/project_build.go, internal/cli/fork_approval_snapshot_test.go, internal/box/fork_approvals_test.go, internal/networkstate/fork_approvals_test.go]
check: "make check"
updated: 2026-11-05
---

# Snapshot approved inputs when creating a fork

New ordinary and isolated forks inherit the parent's existing network, service/secret and
project-build approvals once, while creation owns the unpublished checkout. Re-key host records
to fresh fork identities, preserving the reviewed digests, secret paths, image pins and volume
bindings. Project files and copied public markers never grant authority. Preserve withdrawal
barriers and refuse invalid parent identities. Reopening a fork never reads newer parent grants.

**Why:** four detached forks refused the parent's unchanged network rules, then asked again for
the project build and service secrets. The owner requested creation-time snapshots, including
denial after fork edits and no inheritance of approvals made later in the parent.

**How to apply:** use the existing admission checks on the fork's own inputs. Copy the build's
reviewed-files digest; its image-output proof includes a path-derived tag and cannot be re-keyed.
Retire previous image and review grants when reusing a fork pathname. Capture the parent's network
record under its lock, release it, then publish the fork: different IDs can share one lock stripe.
Keep controller-job and disposable review-candidate policy ownership separate. A refused detached
worker must show stopped with its reason, retaining the generation checks and machine state.

## Changelog
- 2026-11-05 — swept CLI new/fresh/resume/worker paths, fork ACP, review candidates and controller
  workspace creation. Added creation-only snapshots for ordinary/isolated forks and aligned local
  fork ACP build readiness with its admission. Regressions cover unchanged/changed inputs, later
  parent grants, copied markers, withdrawal, failed publication, reused image grants and colliding
  lock stripes. Controller jobs retain their independently frozen authority.
