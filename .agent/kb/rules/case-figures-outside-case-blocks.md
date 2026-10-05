---
name: case-figures-outside-case-blocks
description: "a homepage figure sits above or below its case's block, never between the threat and how the box stops it"
scope: docs
sources: [tools/site/index.tpl.html, site/index.html, site/assets/css/site.css, site/assets/js/site.js, tools/test_site_content.py]
check: make tools-test
updated: 2026-10-05
---

# Homepage: a case's figure stays outside the case's block

A homepage case is an `li.attack`. Its block is the `.case` div: the threat and the lines saying how
the box stops it. A sourced figure about the case (`.evidence`) goes above or below that block,
never inside it, in every layout. Today it is the first child of the `.attack`, above the block. In
the pinned chapters, site.js shows it above the rail.

**Why:** 3a348a5e set each figure between the threat and its stopped lines. The owner, 2026-10-05:
"im not a fan how you moved metrics inside that block, they should be either above or below it".

**How to apply:**
- A new case keeps the shape `<li class="attack"><p class="evidence">…</p><div class="case"><p
  class="threat">…</p><p class="stopped">…</p></div></li>`, and a case with no fitting source has
  no figure.
- Don't move a figure into the block with styles either (negative margins, absolute positioning).
  Check the 1440 pinned view and the 820 and 390 lists.
- Background: the Figures paragraph in `.agent/kb/website.md`.

## Changelog
- 2026-10-05 — created from the owner's correction. Swept site/index.html: 7 cases, 6 figures,
  0 inside a block after 82b276f2 (before it, all 6 were). Gated by
  test_case_figures_sit_outside_their_case_block in tools/test_site_content.py, run through
  `make tools-test`; moving the ssh figure into its block fails it.
