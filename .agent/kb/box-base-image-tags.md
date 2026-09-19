---
name: box-base-image-tags
description: Coop's shared base is tagged by its box definition so two Coop versions on one host keep their own; why there is no :latest alias, how an upgrade is repaired, and what stays shared
subsystem: box
sources: [internal/box/image.go, internal/box/staleness.go, internal/box/locked_image.go, internal/cli/launch_box.go, internal/cli/commands.go, internal/cli/loop_cmd.go, internal/cli/cli.go, internal/cli/build_cmd.go, internal/cli/session_cmd.go, internal/forkctl/host.go, internal/forkctl/merge.go]
updated: 2026-09-19
---

`COOP_BASE_IMAGE` defaults to the bare repository `coop-box`, which `box.ResolveBaseImage` (run
right after `config.Load` in `cli.Main` and the session service) turns into
`coop-box:<first 32 hex of baseDefHash>`: the hash of the base definition this binary would build.
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

Where it runs: an ordinary interactive launch resolves with `resolveLaunchImage(true)`, which lets
the missing base through, and builds it in `checkCoopBox` after network admission, so a filtered box
(it runs `coop-clients:<definition>`, whose Dockerfile shares no expensive layer with the base: the
PATH lines differ inside the big RUN) never pays for it. Every other `resolveImage` caller (fork
create, fork ACP, the ACP inner child), `restrictedImage` and loop start-up build it eagerly, since
no posture-aware check follows. A merge or session review gate builds it through
`forkctl.Host.EnsureBaseImage` (the CLI's `ensureManagedBase`; the daemon's writes to its log).
`coop build` and `coop update` print `Image: coop-box:<definition>`. Until the next launch,
`coop update --check` and `coop doctor` still report the new tag as not built.

Still shared: a project image (`coop-<repo>`, from `.agent/Dockerfile`) keeps one name, so two
versions still take turns on it, and a project box built on an older base keeps those clients until
`coop build` — `checkCoopBox` never replaces a project's image. Superseded bases are never removed
automatically; they accumulate like `coop-clients:<definition>`.

## Changelog
- 2026-09-19 — created with the per-definition base tag.
