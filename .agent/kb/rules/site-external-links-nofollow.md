---
name: site-external-links-nofollow
description: "every link off coop.dryga.com opens in a new tab with rel=\"nofollow noopener\"; the site generator (tools/gendocs) adds both, never the page by hand"
scope: docs
sources: [tools/gendocs/site.go, tools/site/index.tpl.html, site/index.html, site/docs.html, tools/gendocs/site_test.go]
check: go test ./tools/gendocs -run 'TestLinksOffTheSiteOpenInANewTabWithNofollow|TestExternalLinkMarkingKeepsRelAndSettles'
updated: 2026-10-16
---

# Website: a link off the site opens in a new tab, with nofollow

Every `<a>` on the website whose `href` is `http(s)` to a host other than `coop.dryga.com` carries
`target="_blank"` and a `rel` with `nofollow` and `noopener`. Relative links, in-page anchors and
`coop.dryga.com` links carry neither.

**Why:** the owner, 2026-10-06: "make all external links to open in new window and add nofollow so
we don't add ton of external links for seo". The homepage cites papers, reports and vendors, and the
docs link GitHub throughout; followed, those links pass the site's search ranking away. `noopener`
keeps the new tab from reaching back into the page that opened it.

**How to apply:** write links plainly in `tools/site/*.tpl.html`, the scenes in
`tools/gendocs/scenes.go` and `site/docs.html`; `externalLinks()` in `tools/gendocs/site.go` marks
every page when `make docs` generates it, and keeps any `rel` words already there. A bare external
link makes `make docs-check` fail until you regenerate. `noreferrer` is deliberately not added:
GitHub's traffic page should still see visits from the site.

## Changelog
- 2026-10-16 — the generator and its tests are Go (`tools/gendocs`). Swept the three pages: 0 bare
  external links; a bare emisar.dev link in docs.html fails both `make docs-check` and
  TestLinksOffTheSiteOpenInANewTabWithNofollow.
- 2026-10-06 — created. Swept both pages (`site/` is all the HTML Pages publishes): 39 external
  links, none marked before; all marked by the generator in the same change, 0 violations left.
