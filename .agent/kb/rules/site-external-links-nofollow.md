---
name: site-external-links-nofollow
description: "every link off coop.dryga.com opens in a new tab with rel=\"nofollow noopener\"; tools/gen_site.py adds both, never the page by hand"
scope: docs
sources: [tools/gen_site.py, tools/site/index.tpl.html, site/index.html, site/docs.html, tools/test_site_content.py]
check: make tools-test
updated: 2026-10-06
---

# Website: a link off the site opens in a new tab, with nofollow

Every `<a>` on the website whose `href` is `http(s)` to a host other than `coop.dryga.com` carries
`target="_blank"` and a `rel` with `nofollow` and `noopener`. Relative links, in-page anchors and
`coop.dryga.com` links carry neither.

**Why:** the owner, 2026-10-06: "make all external links to open in new window and add nofollow so
we don't add ton of external links for seo". The homepage cites papers, reports and vendors, and the
docs link GitHub throughout; followed, those links pass the site's search ranking away. `noopener`
keeps the new tab from reaching back into the page that opened it.

**How to apply:** write links plainly in `tools/site/index.tpl.html`, the blocks in
`tools/gen_site.py` and `site/docs.html`; `external_links()` in `tools/gen_site.py` marks both pages
when they are generated, and keeps any `rel` words already there. A bare external link makes
`python3 tools/gen_site.py --check` fail until you regenerate. `noreferrer` is deliberately not
added: GitHub's traffic page should still see visits from the site.

## Changelog
- 2026-10-06 — created. Swept both pages (`site/` is all the HTML Pages publishes): 39 external
  links, none marked before; all marked by the generator in the same change, 0 violations left.
