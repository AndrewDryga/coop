---
name: site-ryker-product-link
description: "website Ryker links point to https://ryker.emisar.dev"
scope: docs
sources: [tools/site/index.tpl.html, tools/site/reference.tpl.html, site/docs.html]
check: none
updated: 2026-11-02
---

# Link to Ryker's product website

Website links labeled Ryker point to `https://ryker.emisar.dev`.

**Why:** the owner, 2026-11-02: "ryker link should go to ryker.emisar.dev".

**How to apply:** update the footer in `tools/site/*.tpl.html` and the hand-written
`site/docs.html`, then run `make docs` to update every generated page. Preserve the
external-link treatment from [[site-external-links-nofollow]].

## Changelog
- 2026-11-02 — swept 9 templates and 20 published HTML pages; replaced 29 Ryker GitHub
  destinations with the product website. No old Ryker GitHub target remains in either tree.
