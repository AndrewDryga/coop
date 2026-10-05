---
name: case-figures-outside-case-blocks
description: "a homepage figure sits above or below its case's block, never inside it; a chapter gives every case a figure or none"
scope: docs
sources: [tools/site/index.tpl.html, site/index.html, site/assets/css/site.css, site/assets/js/site.js, tools/test_site_content.py]
check: make tools-test
updated: 2026-10-05
---

# Homepage: a case's figure stays outside the case's block

A homepage case is an `li.attack`. Its block is the `.case` div: the threat and the lines saying how
the box stops it. A sourced figure about the case (`.evidence`) goes above or below that block,
never inside it, in every layout. Today it is the first child of the `.attack`, above the block. In
the pinned chapters, site.js shows it above the rail. A chapter gives every case a figure or none:
pinned, a case without one leaves the space above the rail empty.

**Why:** 3a348a5e set each figure between the threat and its stopped lines. The owner, 2026-10-05:
"im not a fan how you moved metrics inside that block, they should be either above or below it".
The same day, about the push case, which had no figure: "we should either have a metrics for all
sstates (add it) or not show previous one but not leave that gap empty".

**How to apply:**
- A new case keeps the shape `<li class="attack"><p class="evidence">…</p><div class="case"><p
  class="threat">…</p><p class="stopped">…</p></div></li>`.
- A new case in a chapter with figures needs a sourced figure too. If none fits, find one, or take
  the figures out of the whole chapter; never keep the previous case's figure on screen.
- Don't move a figure into the block with styles either (negative margins, absolute positioning).
  Check the 1440 pinned view and the 820 and 390 lists.
- Background: the Figures paragraph in `.agent/kb/website.md`.

## Changelog
- 2026-10-05 — created from the owner's correction. Swept site/index.html: 7 cases, 6 figures,
  0 inside a block after 82b276f2 (before it, all 6 were). Gated by
  test_case_figures_sit_outside_their_case_block in tools/test_site_content.py, run through
  `make tools-test`; moving the ssh figure into its block fails it.
- 2026-10-05 — every case or none, after the owner rejected the empty gap above the push case.
  The push case got a figure (35%, arXiv 2605.07769), so both chapters give all their cases one
  (7 of 7). Gated by test_a_chapter_gives_every_case_a_figure_or_none; removing the push figure
  fails it. site.js now builds the slot only when every case has a figure.
