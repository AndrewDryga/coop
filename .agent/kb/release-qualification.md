---
name: release-qualification
description: "tagged releases reuse exact-commit CI, validate finalized notes and preserve tag identity"
subsystem: release
sources: [Makefile, .github/workflows/ci.yml, .github/workflows/release.yml, .goreleaser.yaml, tools/releasenotes/main.go, .github/release-notes.md, release_test.go]
updated: 2026-10-17
---

Release `ci` locally calls ci.yml from the event commit; its Make-owned broad checks, Docker
doctor and Docker review-write jobs must succeed before the privileged publisher starts.
Every checkout explicitly uses github.sha. CI remains contents-read only. It makes no live model
calls and needs no live-provider run or version-specific waiver.

The status-only `check` job runs even after a dependency fails and passes only when every free
owner and both runtime jobs succeeded. Read-only jobs may cache modules and compiled objects;
tests rerun. The publisher remains fresh, uncached and without imported CI artifacts. See
[[ci-free-checks]].

The preflight, `go run ./tools/releasenotes notes <tag>`, runs right after checkout and Go setup.
It compares the event tag's peeled commit to HEAD, requires CHANGELOG.md's first real ATX H2
section to be the tag's, and prints the same section of `.github/release-notes.md`, the short
summary the finalize commit carries, which becomes the whole release body. Each file's heading
must exactly match the SemVer tag without `v`; Unreleased, empty notes,
Setext release headings, raw HTML blocks and unclosed comments/fences refuse. Code examples stay
literal, comments outside code are removed, and failures emit no partial notes. This is a
release-format validator, not a general Markdown renderer.

GoReleaser 2.18.2 prefers GORELEASER_CURRENT_TAG over other tags at HEAD, so the workflow pins it
to the validated event tag. Its changelog pipe must remain enabled to consume --release-notes.
Its dirty-tree check precedes before hooks: `go mod tidy -diff` refuses graph drift without
rewriting the files after qualification; the completions hook remains generated from source.
The canonical gate starts with the same nonmutating `mod-check`, so module metadata drift
fails before long behavioral suites, not for the first time in the privileged publisher.

The remote raw/peeled tag recheck, `verify-remote <tag>`, runs just before GoReleaser starts, not
atomically with GitHub publication. It requires RELEASE_REMOTE_URL, supplied by the workflow
from github.server_url and github.repository; it never resolves origin from the checkout.
Local identity queries use a trusted observation view. Remote lookup uses a fresh private Git
directory and independent environment, so checkout sshCommand and URL rewrites cannot execute
a transport. Missing remote authority fails closed. Keep v* update/deletion rules enforced; trusted bypass actors
must not mutate the tag or protection during build/publication. Local fixture tests cannot prove
hosted CI, signing or publication succeeded. Test refs/remotes are confined to temporary
filesystem repositories.

## Changelog
- 2026-10-17 — isolated release Git queries and moved remote authority into workflow context;
  hostile origin, sshCommand and URL-rewrite regression preserves tag identity checks.
- 2026-10-17 — the published body is `.github/release-notes.md`'s section, not the CHANGELOG
  section (owner decision); CHANGELOG.md is still checked, and both files share the parser.
- 2026-10-16 — the preflight is Go (tools/releasenotes), so unit tests need no python3; it gives
  byte-identical notes for all 42 release tags. verify-remote accepts only LF-separated ls-remote
  lines.
- 2026-10-16 — the saved provider report and its targets are gone; nothing in CI or the release
  read it since 2026-10-14, so the release path is unchanged.
- 2026-10-15 — CI lost its CHANGELOG-only path, so a release call and a main push run the same
  jobs; the required status passes only when all of them succeed.
- 2026-10-15 — the reusable CI call runs every Make owner in CI_OWNERS, not four; owner caches
  are restored from main only, and the publisher stays uncached.
- 2026-10-14 — verified Make partition coverage, strict metadata routing and aggregate refusal
  controls; exact-tag broad checks and uncached publisher identity/signing remain unchanged.
- 2026-10-14 — renamed the reusable release job to ci and removed saved-provider-report release
  permission. Exact-tag free CI, Docker checks, publisher isolation and signing are unchanged.
- 2026-10-09 — v11 publication refused untidy direct-dependency classification and stale sums
  after qualification. Kept the publisher refusal and added the identical early canonical check;
  dependency versions remain unchanged. Focused release contract and four-platform snapshot
  validate module hygiene and packaging; hosted publication/signing remain separate proof.
- 2026-09-30 — updated GoReleaser pin; config check and unsigned four-platform local snapshot
  pass under Go 1.27.1. Hosted signing/publication still require exact-tag qualification.
- 2026-09-17 — Podman removed; the qualifying doctor job is Docker only.
- 2026-09-06 — created from M01; verified workflow dependencies, tag/notes helper fixtures and
  pinned GoReleaser pipeline ordering. No repository settings change or public tag probe.
