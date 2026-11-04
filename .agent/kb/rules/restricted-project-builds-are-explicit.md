---
name: restricted-project-builds-are-explicit
description: only a person approves new project build files; restricted launches rebuild by themselves only the exact files already approved
scope: security
sources: [internal/box/build_context_ignore.go, internal/box/derived_image.go, internal/box/project_build.go, internal/box/project_preparation.go, internal/box/image.go, internal/networkstate/project_builds.go, internal/cli/project_build.go, internal/cli/build_cmd.go, internal/cli/acp_cmd.go, internal/cli/commands.go, internal/cli/fork_cmd.go, internal/loop/loop.go, internal/forkctl/merge.go, internal/cli/acp_box_wait.go, internal/runtime/project_build.go]
check: "go test ./internal/box -run 'TestFilteredProjectImageRequiresExplicitBuild|TestFilteredProjectImageReusesAnUnchangedBuild|TestAutomaticProjectBuildRequiresOpenNetworking|TestReviewedProjectBuildExecutesOnlyItsPrivateSnapshot|TestDockerignoredEditsDoNotChangeBuildInputs|TestDockerignoreCannotRestoreSanitizedInputs|TestApprovedProjectFilesRebuildByThemselvesWhenOnlyCoopChanged|TestOnlyTheApprovedFilesRebuildByThemselves|TestAgentEditAfterTheCheckBuildsNothing|TestALaterApprovalReplacesTheEarlierOne|TestAdmissionRebuildsOnlyApprovedFilesAndOnlyWhenAllowed|TestStagingStaysInsideTheSnapshot|TestSameNamedProjectsGetTheirOwnTags|TestAutomaticRebuildGetsAFiniteEnvironment|TestSecondAutomaticBuildReusesTheFirst|TestALaunchWaitingForTheBuildLockHoldsNoSnapshot|TestANetworkRuleEditAsksForNoProjectBuild|TestABuildThatCopiesTheProjectSettingsIsRefused'"
updated: 2026-11-04
---

# Only a person approves new project build files when networking is restricted

Filtered launches must not execute repository build instructions a person has not approved: an
agent can edit them, and a project build has ordinary networking. Where a person has approved these
exact files, Coop must rebuild them by itself instead of stopping: a new client image or a lost image
needs no person. Approval comes from an explicit host build, or from one host-terminal interactive
provider launch offering default-No Build and continue for an inspectable owner-private snapshot
captured before consent. A decision covers only those bytes, that Dockerfile path, the selected daemon
and those build settings, never later edits, and a later approval replaces it. Headless, editor,
loop, controller and in-box callers cannot acquire this permission by having a tty, and an editor
approval button does not count: an editor can answer its own prompts. `coop update` approves nothing:
it rebuilds approved files and leaves others for `coop build`. An editor session connects anyway and
waits, running no turn (see [[acp-box-wait]]). Offline launches build nothing.
Open-network editor connections may prepare a missing project image automatically; ordinary runs only
warn on stale project inputs and need an explicit build. Explicit host builds remain available; say
plainly that their networking is not restricted by the run's egress policy.

**Why:** the audit found that a boxed agent could edit a copied build script, then obtain ordinary
networking through the next filtered launch's automatic rebuild. The human chose explicit builds,
adding: "But we can have automatic builds when networking is not restricted." On 2026-11-03, after
ACP died on a missing build: "changed build should wait, no escaping sandbox, but where we can do it
safely it should be automatic."

**How to apply:** decide an automatic rebuild on the digest of the staged snapshot, outside every
mount, under the project's build lock, never on an earlier read of the checkout; only a person's build
may change what counts as approved. Stage it under the lock too, so a launch waiting on another build
holds no copy; a read may only say the image is already built, which runs nothing and stages nothing.
A rebuild fetches again what the approved Dockerfile fetches, which
an explicit rebuild would too. Callers that only want an answer, like a model catalog refresh, refuse
instead of building. Bind filtered reuse to the actual staged context, including copied files and
modes, the qualified client base and immutable output image. Keep approval outside agent mounts;
apply Dockerfile-specific/root Docker ignore rules to both staging and readiness, so excluded
source edits do not revoke the build. Always cover the Dockerfile and active ignore control file;
Docker exceptions cannot restore secrets, Git or generated output omitted by the sanitizer.
Leave `.agent/project.yaml` and the approval markers out of the context itself, never only out of
the digest: then a rule edit revokes no build and `COPY . .` cannot bake it in, for every project.
The review binds `box.dockerfile` through the Dockerfile's path. A Dockerfile that copies that file
by name hears why before anything runs, and that check explains and guards nothing.
Legacy automatic-build memos cannot become approval. Missing/changed inputs or missing images must
refuse with a usable host command. Preserve both image proofs and fail if approval cannot be saved.
Cover direct, fork, loop and editor callers; do not require an unused ordinary image for filtered
runs. Keep offline image-existence checks so Docker cannot implicitly pull. Embedded Coop image
preparation is separate from executing project instructions. This does not freeze runtime source
edits or change service permissions; see [[project-edits-request-access]]. A controller job's
turns and review gates consume no project image in any mode, and a worker never builds a job repository's
instructions.

## Changelog
- 2026-11-04 — a live session on 2026-11-03 asked for another build right after a network rule edit,
  and a rule's approval marker did the same in a project with no Git. Chose the shared selection over
  a `coop init` ignore file, which misses existing projects and can be deleted. Swept the six
  non-test callers of `buildContextSelection`: all share it, and only `PlanBuild` and
  `snapshotProjectBuild` reach a builder first, so those two carry the refusal.
- 2026-11-03 — review found automatic rebuilds staged before taking the lock, so N waiting windows held
  N copies of the checkout. They now take the lock first; both callers (`RebuildApprovedProject`,
  `UpdateApprovedProject`) share `rebuildApprovedLocked`, the only place a build is marked automatic.
- 2026-11-03 — user hit `✗ this project's .agent/Dockerfile is not ready` in Zed and asked why Coop
  cannot just build; filtered became the default the same day. Approved: changed files wait, approved
  files rebuild. Swept every `AdmitNetwork` caller: direct, fork, fork ACP, loop and merge gates rebuild
  approved files; model probes (`RefuseProjectBuild`), evals (docs/evals.md: they never build a profile)
  and ACP re-admission (`ExistingQualification`) do not. Session-service admission reaches no project build. Offline never
  reaches readiness. Regressions cover every other kind of miss and a check-to-build edit. Review found `coop update` had
  been recording approval by building whatever was on disk: it now rebuilds only approved files. Two
  folders with one name shared a tag and would rebuild over each other: tags now carry a path hash.
  Staging wrote through an agent's link on a case-folding volume: writes now go through an `os.Root`.
  A rebuild gets a finite environment: Buildx follows file-backed controls beyond DOCKER_*.
- 2026-10-16 — user rebuilt successfully but source edits 13 seconds later blocked ACP again.
  Swept project staging, explicit build, readiness and editor image identity: they share one
  selection; it now respects Docker exclusions while keeping control files and selected copied
  inputs exact. Added excluded-source reuse and sanitization regressions to the check.
- 2026-10-11 — owner approved explicit host Build and continue after a late launch refusal.
  Swept 13 listed source files: unattended/ACP/fork/loop/controller paths retain refusal;
  only the direct host-terminal path confirms a private snapshot. Added the snapshot check;
  the canonical gate also covers native headless denials and daemon-binding regressions.
- 2026-09-30 — rechecked controller turns and review gates in `commands.go`, `merge.go` and
  `box/run.go`: both use the worker image and bypass the job repository's Dockerfile. The earlier
  filtered-review limitation below was superseded by `ReviewControllerJob`; focused open/offline
  and saved-snapshot review tests pass. The existing project-build rule check also passed.
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
