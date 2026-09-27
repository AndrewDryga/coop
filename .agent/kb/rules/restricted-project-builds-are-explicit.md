---
name: restricted-project-builds-are-explicit
description: restricted launches consume explicit host project builds; open mode permits automatic preparation only where implemented
scope: security
sources: [internal/box/derived_image.go, internal/box/project_build.go, internal/box/image.go, internal/networkstate/project_builds.go, internal/cli/build_cmd.go, internal/cli/acp_cmd.go, internal/cli/commands.go, internal/cli/fork_cmd.go, internal/loop/loop.go, internal/forkctl/merge.go]
check: "go test ./internal/box -run 'TestFilteredProjectImageRequiresExplicitBuild|TestFilteredProjectImageReusesAnUnchangedBuild|TestAutomaticProjectBuildRequiresOpenNetworking'"
updated: 2026-09-28
---

# Require explicit host builds when project networking is restricted

Filtered/offline launches must not execute repository build instructions with ordinary build
networking. Open-network editor connections may prepare a missing project image automatically;
ordinary runs only warn on stale project inputs and need an explicit build. Explicit host builds
remain available; say plainly that their networking is not restricted by the run's egress policy.

**Why:** the audit found that a boxed agent could edit a copied build script, then obtain ordinary
networking through the next filtered launch's automatic rebuild. The human chose explicit builds,
adding: "But we can have automatic builds when networking is not restricted."

**How to apply:** bind filtered reuse to the actual staged context, including copied files and
modes, the qualified client base and immutable output image. Keep approval outside agent mounts;
legacy automatic-build memos cannot become approval. Missing/changed inputs or missing images must
refuse with a usable host command. Preserve both image proofs and fail if approval cannot be saved.
Cover direct, fork, loop and editor callers; do not require an unused ordinary image for filtered
runs. Keep offline image-existence checks so Docker cannot implicitly pull. Embedded Coop image
preparation is separate from executing project instructions. This does not freeze runtime source
edits or change service permissions; see [[project-edits-request-access]]. A controller job's
turns consume no project image in any mode, and a worker never builds a job repository's
instructions.

## Changelog
- 2026-09-28 — swept the listed sources for controller-job paths: `resolveLaunchImage` and the
  session review gate still named the job repository's project tag; both now take
  `box.JobImage` (the worker's base). Filtered jobs already skipped the project Dockerfile; the
  review gate's filtered posture still consults it (the gate box is not a ControllerJob), and
  refuses without an approved build.
- 2026-09-23 — manually changed a project Dockerfile and reran in open mode: the existing image
  ran with a stale-input warning until `coop build --egress open` was invoked. Clarified the
  scope of open automatic preparation without changing the approved restricted boundary.
- 2026-09-22 — swept the ten listed source files and their callers. Removed filtered automatic
  construction, guarded BuildWith's restricted callers, and deferred ordinary-image checks until
  posture is known. Controlled regression reproduced a builder call before the fix and zero after;
  reuse, copied-input changes, missing images, legacy records and open compatibility are tested.
  Filtered merge/review gates also defer unused ordinary-image checks; recovery commands retain
  the selected image family, and doctor documentation names its ordinary-image scope.
