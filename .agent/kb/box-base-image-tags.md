---
name: box-base-image-tags
description: Coop's shared base is tagged by its box definition so two Coop versions on one host keep their own; why there is no :latest alias, how an upgrade is repaired, and what stays shared
subsystem: box
sources: [internal/box/reclaim.go, internal/box/derived_image.go, internal/box/filtered.go, internal/box/image.go, internal/box/staleness.go, internal/box/locked_image.go, internal/cli/launch_box.go, internal/cli/commands.go, internal/cli/loop_cmd.go, internal/cli/cli.go, internal/cli/build_cmd.go, internal/cli/session_cmd.go, internal/forkctl/host.go, internal/forkctl/merge.go]
updated: 2026-09-30
---

`COOP_BASE_IMAGE` defaults to the bare repository `coop-box`, which `box.ResolveBaseImage` (run
right after `config.Load` in `cli.Main` and the session service) turns into
`coop-box:<first 32 hex of baseDefHash>`: the hash of the base definition this binary would build.
On native Linux, the host user's UID/GID are part of that definition and the locked-client image's
build arguments, so two users of one Docker daemon do not reuse images with mismatched `/home/node`
ownership. macOS Docker Desktop retains UID/GID 1000. Native Linux cache/asdf volume names also
include non-default UID/GID; the default names stay unchanged. An ordinary project's built image
gets the same per-user separation, while filtered project tags already hash the locked client image.
The filtered client image was already tagged that way (`coop-clients:<definition>`,
`locked_image.go`). Any other value — `coop-box:latest`, a registry image — is the operator's and
stands as written; `IsManagedBase` tells the two apart.

Why: with one shared `coop-box:latest`, two installed Coop versions (a checkout's and a deployment's)
each saw the other's definition in the build record and rebuilt the tag on their next launch, running
the other's clients in between (seen 2026-09-19: v9.0.0-372 and v9.0.0-226 on one Mac). There is
deliberately NO `:latest` alias from a tagged Coop: an older Coop reads its own untagged record, so it
would run a newer base under that name without noticing. Manual `docker build`s pass the tag instead.

An upgrade is a new tag, so nothing is "skewed" any more for a managed base (`BaseImageSkew` returns
false for one; the record-vs-definition check remains for an operator's base). Instead
`box.ManagedBaseRepair` says when Coop builds its own base itself: managed, missing, the daemon
answers (a stopped daemon reads as a missing image and must stay the caller's to report), and some
managed-base record exists on the host — an untagged `coop-box` one, another definition's, or this
tag's own (then the image was removed, and the section says "The box image is missing"). A host
with no record still gets "run 'coop build'": a first build stays the operator's.
`box.BuildManagedBase` builds the base alone, reading nothing and writing to the given writer,
because an ACP child and the session daemon reach it too.

Where it runs: an ordinary interactive launch resolves with `resolveLaunchImage()`, which lets
the missing base through, and builds it in `checkCoopBox` after network admission, so a filtered box
(it runs `coop-clients:<definition>`, whose Dockerfile shares no expensive layer with the base: the
PATH lines differ inside the big RUN) never pays for it. Fork and ACP launch callers also defer
ordinary-image availability until admission or capture reconstruction. `restrictedImage` still
prepares the shared base eagerly; it never executes project build instructions. An ordinary merge
or controller-job review gate builds it through `forkctl.Host.EnsureBaseImage` (the CLI's
`ensureManagedBase`; the daemon's build output goes to the controller-readable review log).
Filtered gates defer to their captured image instead. `coop build --egress open` and `coop update`
print `Image: coop-box:<definition>` when preparing the shared image. Until the next launch,
`coop update --check` and `coop doctor` still report the new tag as not built.

A controller job's turns and reviews never resolve a project image. A worker stages every job source in a
folder named `repository`, so `ImageForRepo` there names one `coop-repository` tag for every
repository, and nothing on a worker may build repository instructions. Under
`COOP_CONTROLLER_JOB`, `resolveLaunchImage` picks `box.JobImage` (the operator's `COOP_IMAGE`,
else the worker's base) without parsing the job's project file: `project.Parse` rejects unknown
keys, so a file written for a newer Coop would refuse every job on an older worker. A filtered
job runs the locked client image (`filteredProjectImage` returns nothing for a job), and a
readonly or bare one `restrictedImage`, which refuses `COOP_IMAGE`. The session review gate uses
`forkctl.ReviewControllerJob`: it selects the worker image, runs with `ControllerJob`, and reopens
the session's saved filtered capture instead of consulting repository box policy or Dockerfile.

Still shared across Coop versions for one user: a project image (`coop-<repo>`, from `.agent/Dockerfile`;
on native Linux with a non-default UID/GID it has a user suffix) keeps one name per user, so two
versions still take turns on it, and a project box built on an older base keeps those clients until
`coop build` — `checkCoopBox` never replaces a project's image. A superseded base IS reclaimed now
(`reclaim.go`), but only after 14 days with no recorded use and no container referencing it. A
project's own derived image is reclaimed by the same rule, with one extra proof: that repository is
the PROJECT's name, so only images carrying the `coop.derived=<project>` label the build applies are
candidates — an image an operator built into the same name is never one, and neither is one Coop
built before it started marking them (remove those by hand). Note WHAT supersedes one:
`filteredProjectTag` hashes the LOCKED CLIENT IMAGE, not the Dockerfile, so editing
`.agent/Dockerfile` rewrites the same tag (the old image goes dangling, which `docker image prune`
already took); a tagged predecessor appears when the client image changes — a Coop upgrade, or
`coop net setup` again.

A use record is per *user* (`~/.config/coop/image-use/`, `Config.BoxHome`), while the images are
per *daemon*. On the
usual one-user host that is the same thing; where two users share one Docker, each sees only its own
records, so A's build can reclaim an image B still runs weekly (B's next launch rebuilds the managed
base; a filtered project image requires an explicit rebuild). What
stops that being routine: nothing without a record is ever removed on sight (first sight SEEDS the
record, so the 14 days start then), and an image any container still references — running or
stopped, whoever owns it — is kept.

## Changelog
- 2026-09-30 — reverified the dedicated controller review path and its saved-mode tests. Replaced
  the stale ordinary `JobGate` claim; review build output now enters the review log. The open and
  offline gate cases share one config without mutation, and filtered review reopens the job snapshot.
- 2026-09-28 — Ryker's worker failed every emisar/ryker turn on the never-built
  `coop-repository` tag: controller jobs now run the worker's base (`box.JobImage`) for turns and
  the session review gate. Corrected the stale `resolveLaunchImage(true)` reference.
- 2026-09-25 — rechecked image tagging against UID-aware base and locked-image builds and
  per-identity cache volumes for native Linux users.
- 2026-09-22 — filtered launches no longer require the unused ordinary project tag; only an
  explicit host build constructs the filtered project image.
- 2026-09-20 — a project's own derived images join the reclaim, recognized by the `coop.derived`
  label their build now applies (never by the `-filtered` name alone).
- 2026-09-20 — a build reclaims its family's superseded images (`box/reclaim.go`): 14 days without a
  recorded use and no container referencing it. Every launch records its image (`MarkImageUsed`),
  which is what keeps a second installed Coop's base alive; `:latest`, an operator's own image and
  any non-definition tag are never candidates.
- 2026-09-19 — created with the per-definition base tag.
