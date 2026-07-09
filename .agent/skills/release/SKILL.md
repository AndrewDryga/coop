---
name: release
description: Cut a versioned release of coop — derive the semver bump from the `## Unreleased` changelog, finalize + annotated-tag it, then (only on explicit confirmation) push the `v*` tag that triggers the public GoReleaser build. Refuses a docs-only no-op release. Use when asked to "cut / ship / tag a release", "publish a release", or "bump the version".
argument-hint: "[version, e.g. 2.8.0 — omit to derive from the changelog]"
allowed-tools: Read, Grep, Glob, Bash, Edit
---

# /release — cut one clean, signed release

Releases are tag-driven: the version comes from `git describe --tags`, and pushing a `v*`
tag fires `.github/workflows/release.yml` → GoReleaser builds every OS/arch and publishes a
**public, cosign-signed GitHub Release** (binaries + `checksums.txt` + build provenance) that
`install.sh` serves. **The tag push is the point of no return** — everything before it is
local and reversible; after it, the artifacts are public. Don't poke at the Makefile/workflow
each time — this is the contract; follow it.

## 1. Pre-flight — is there anything real to ship?
- On `main`, clean tree, not behind `origin/main` (the only ahead commits should be your own
  release prep). Gate green: `make check`.
- **Refuse a no-op.** `git diff --stat "$(git tag --sort=v:refname | tail -1)"..main` — if it
  touches only `CHANGELOG.md`/docs (no code), STOP: the binary would be byte-identical to the
  last release. The fix is almost always changelog *attribution*, not a new version — move the
  entry under the release where the code actually shipped (`git tag --contains <commit>` shows
  which one already has it). Cutting a version for unchanged code misdates the feature.
- The `## Unreleased` entries ARE the release. Empty ⇒ nothing to cut; say so and stop.
- Optional dry-run: `make snapshot` (GoReleaser `--snapshot --clean --skip=sign`, no publish;
  signing is CI-only keyless OIDC) to catch config breakage before any tag exists.

## 2. Pick the version (semver — or take the user's)
- Latest: `git tag --sort=v:refname | tail -1`.
- Bump by what's in Unreleased: new functionality → **minor** `x.Y.0`; fixes/hardening only →
  **patch** `x.y.Z`; a breaking change → **major**. A version the user named wins.
- Guard: the tag must not already exist and must sort after the latest.

## 3. Finalize + tag (all local, still reversible)
- Rename `## Unreleased` → `## <version>` (NO date — house format is a bare `## X.Y.Z`).
  Keep entries as `- **Bold headline.** explanation`.
- Commit CHANGELOG-only: `changelog: finalize <version> (<one-line summary>)`.
- Annotated tag on THAT finalize commit: `git tag -a "v<version>" -m "v<version>"`
  (tags are annotated and point at the finalize commit, never the re-open commit).
- **Re-true the site casts now** — with HEAD at the tag, `./coop version` is the exact `v<version>`,
  so `make casts` re-captures `site/casts/help.cast` with the release version, not a dev/dirty one
  (it rebuilds `./coop` and refuses an untagged/`+dirty` binary). Commit the refreshed casts:
  `casts: regenerate at v<version>`.
- Re-open: add a fresh `## Unreleased` + the placeholder comment back; commit
  `changelog: open an Unreleased section after v<version>`.

## 4. Push — public, irreversible: CONFIRM FIRST
- Show the user the version, the one-line summary, and that this publishes a public signed
  release. Get an explicit go-ahead — this is the one outward-facing, hard-to-undo step.
- Then: `git push origin main && git push origin "v<version>"`.
- Watch it land: `gh run watch` (or `gh run list --workflow=release.yml`), then
  `gh release view "v<version>"` to confirm the assets + `checksums.txt` uploaded.

## 5. If it breaks
- Failed before publishing? Delete the tag both sides (`git tag -d "v<version>"`,
  `git push origin ":v<version>"`), fix, retry.
- Already published? Never rewrite a public tag — roll forward with a follow-up patch.
