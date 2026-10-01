---
name: public-release-notes-are-product-focused
description: public release notes describe actual published releases and product changes, not internal engineering decisions
scope: docs
sources: [CHANGELOG.md, MIGRATING.md, README.md, .agent/skills/release/SKILL.md]
check: none
updated: 2026-10-01
---

# Keep public release notes focused on the product

Publish shipped behavior, useful fixes, compatibility changes and required user actions.
Keep operator approvals, waivers, test/review diaries, spend details and CI troubleshooting
in gitignored task records, not changelogs, GitHub release bodies or customer-facing docs.
Do not turn an internal acceptance decision into a public announcement.

Preserve real user-impacting limitations and migrations. Removing process narration must not
invent successful tests, broader compatibility or capabilities the product does not have.
Contributor instructions may explain how a check works without narrating a particular run.

**Why:** the user explicitly rejected internal-process disclosures in public release notes.

When correcting published copy, edit the existing release body and commit the documentation
fix. Do not rebuild binaries, rewrite an immutable tag or cut a release for wording alone.

Numbered changelog entries represent published releases, not local or failed release attempts.
Keep pending work under Unreleased; fold failed-attempt changes into the version that actually
ships. Preserve every change entry when moving it; do not use history correction as an excuse
to condense or drop changes. Correct descriptions superseded before publication to match the
version users actually receive. Derive the previous version and comparison links from published
GitHub Releases, not the highest Git tag. A failed tag is not evidence that users received a release.

When a release fails before publication, reconcile its changelog entry and remove only its
verified unpublished tag with the required authority. Never remove or rewrite a published tag
to make the history look tidier. Upgrade instructions describe the actual public upgrade, not
fictional intermediate releases.

## Changelog
- 2026-10-01 — swept all 36 published release bodies and the changelog version headings against
  GitHub release history. Folded unpublished v9/v10 attempts into v10.1.2 and the unshipped 2.3
  changes into v2.4.0; corrected both comparison baselines and the current migration guide.
  Preserved early pre-GitHub release history rather than inferring it never shipped.
  After the user's preservation correction, retained all original change-entry headlines and
  detailed descriptions, updating only superseded behavior instead of condensing the entries.
  Release existence is checked against live GitHub metadata during release work, not inferred
  by an offline prose lint. Updated release guidance to use the published baseline.
- 2026-10-01 — swept current release copy, nearby changelog entries and public docs/website.
  Removed internal approval, qualification-run and fixture narratives; kept product fixes,
  upgrade steps and functional limits. Recorded the same boundary in release-writing guidance.
  Editorial relevance requires judgment, so `check: none` is intentional.
