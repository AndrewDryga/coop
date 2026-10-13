---
name: site-stories-use-mobile-cards
description: mobile landing stories pair their pinned animation with readable sticky cards
scope: docs
sources: [site/assets/css/site.css, site/assets/css/orchestrator.css, site/assets/js/site.js, site/assets/js/orchestrator.js]
check: none
updated: 2026-10-13
---

# Use the same sticky-card pattern for mobile landing stories

On small screens with room for a pinned animation, each story step stays as a card
beneath it while the next card approaches. Synchronize the picture to the card at
its floor. Measure card height so a taller card can reveal its last line before
sticking; preserve access to its citations and the plain short-screen/no-script page.

**Why:** the owner found that the sandbox had "nice cards that animate and stick to
bottom" while the orchestrator's "text just scrolls under animation", and asked
to apply the sandbox pattern (2026-10-13).

**How to apply:** reuse the existing measured-height deck in `site.css`/`site.js`
and `orchestrator.css`/`orchestrator.js`. Verify actual rendered scroll behavior,
including tall cards and the final card's containing-block bounds. Mobile changes
leave the desktop story unchanged. See [[website]].

## Changelog
- 2026-10-13 — swept both landing stories across their two stylesheets and two
  scripts: sandbox already met the pattern; corrected orchestrator's scrolling
  steps. Browser checks cover card pinning, timeline state and citation exposure.
  `check: none` because rendered layout and link exposure need browser verification.
