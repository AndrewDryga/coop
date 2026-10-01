---
name: public-release-notes-are-product-focused
description: public release notes describe product changes and user actions, not internal engineering decisions
scope: docs
sources: [CHANGELOG.md, README.md, .agent/skills/release/SKILL.md]
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

## Changelog
- 2026-10-01 — swept current release copy, nearby changelog entries and public docs/website.
  Removed internal approval, qualification-run and fixture narratives; kept product fixes,
  upgrade steps and functional limits. Recorded the same boundary in release-writing guidance.
  Editorial relevance requires judgment, so `check: none` is intentional.
