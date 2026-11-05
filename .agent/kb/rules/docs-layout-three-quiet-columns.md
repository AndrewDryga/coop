---
name: docs-layout-three-quiet-columns
description: docs pages use three columns across the whole page; the menu and the page's contents never share one, side columns stay small and quiet, the article keeps its reading width and type
scope: docs
sources: [site/assets/css/site.css, tools/site/reference.tpl.html, site/docs.html, tools/gendocs/docsmap_test.go, brand/DESIGN.md]
check: go test ./tools/gendocs -run TestMenuArticleAndContentsKeepTheirOwnColumns
updated: 2026-11-04
---

# Docs pages have three quiet columns

The menu of every page, the article and this page's contents each get their own column, across the
whole page instead of the marketing pages' 1200px. The menu shares the logo's left edge, the
contents share Install's right edge, and the article sits centered at its reading width with air on
both sides. The side columns stay small and quiet, and look different from each other because they
do different jobs: plain text with a soft wash on the current page, against a thin line. All three
start on one top line.

Before showing a layout change, compare it with the current page side by side at the same window
sizes, and look at how the best docs sites solve the same problem.

**Why:** the owner, 2026-11-04, rejected two attempts. On three columns squeezed into 1200px with a
filled search box, bold labels and filled rows: "The design was better, cleaner, better spacing and
typographics, but nav structure itself I like". On a menu stacked over the contents in the old side
column: "Links and On this page should not be in same column they merge visually and unusable, think
as a top tier designer". Vercel, Next.js, Linear and Cloudflare all use this frame at 1440px.

**How to apply:**
- Never put the page's contents in the menu's column, nested or stacked.
- Keep one article width (44rem) on every page, so the title's edge doesn't jump as you move between
  pages, and keep the type scale when navigation changes.
- Below 1280px the contents fold above the article. Below 1024px the menu becomes a bar.

## Changelog
- 2026-11-04 — created with the rebuilt docs frame. Measured Vercel, Next.js, Linear, Cloudflare,
  Tailwind, Stripe and Bun at 1440px first.
