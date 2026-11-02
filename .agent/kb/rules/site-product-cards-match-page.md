---
name: site-product-cards-match-page
description: Feature one relevant sibling per landing page with original artwork and a useful example
scope: docs
sources: [tools/site/index.tpl.html, tools/site/orchestrator.tpl.html, site/assets/css/site.css, site/index.html, site/orchestrator.html]
check: none
updated: 2026-11-02
---

# Match each product card to its landing page

Feature Emisar on the sandbox landing page and Ryker on the orchestrator page.
Use one generous, branded feature card after installation on each page. Reuse the
products' original artwork, give the pitch room to read, and show a useful, clearly
labeled example. Stack the copy and example on phones. Footer links can still include
both products.

Use a realistic policy preview for Emisar's illustration: show allowed, approval and
denied totals, short plain-English action examples, and the actions grouped by risk.
Label sample data as an example. Leave editor-only notes and inactive controls out of
the marketing card, and keep every count readable on phones.
Show a few real action IDs beside each risk tier. Check their declared risk in the
current pack files, preserve the exact IDs, and let examples wrap below the badge
on smaller screens.
Present each tier's examples as one comma-separated list ending in `, ..`, such as
`docker.ps, debugging.disk_free, ..`; do not put each example on its own line.

Make Ryker's illustration look like a real Slack thread: a channel header, compact
messages, timestamps and app badges. Show a specific finding
and a natural follow-up, using a supported product scenario. Describe the illustration
for assistive technology without a visible "Example Slack conversation" caption.

Pitch Ryker as a teammate working alongside the team in Slack and GitHub around the
clock: proactively investigate incoming alerts and root causes, propose fixes, and
take on assigned coding work. Preserve the owner-approved wording in the card.
Make the conversation demonstrate that pitch: an alert, an unprompted investigation,
a specific cause, a proactively prepared fix, and a reviewable result. Do not insert
a human request before Ryker prepares the fix. Show a GitHub pull request preview
under its result, including the repository, title, draft state and checks. Prefer a
relatable application failure over an infrastructure-specific setup problem. Treat
sample times and results as illustration, without fabricated PR links or deployment
claims.

**Why:** the owner, 2026-11-02: "I want nicer cards and emisar on sandbox page and
ryker on orchestrator page".

**How to apply:** edit the two landing templates and the scoped product styles in
`site.css`, then regenerate through `make docs`. Preserve [[site-emisar-production-pitch]]
and [[site-ryker-product-link]]. Review the rendered composition on desktop and mobile;
visual quality and contextual relevance need editorial review.

## Changelog
- 2026-11-02 — swept both landing templates and their generated pages. Replaced two
  generic text cards on each with one branded feature: Emisar's example production
  access rules on sandbox, and Ryker's example team conversation on orchestrator.
  Original sibling SVGs are reused unchanged. Existing footer links remain.
- 2026-11-02 — owner supplied the real policy preview as the visual reference and
  asked for something "realistic and simple to understand". Swept both product cards
  and shared styles; replaced Emisar's generic three-rule illustration with the
  supplied outcome/risk totals and plain-English examples. Ryker's conversation is
  unchanged. The 319-action outcome and risk groups each add up to 319.
- 2026-11-02 — owner requested real pack names after the risk labels. Checked eight
  action YAMLs across shell, EC2, Docker, linux-core and debugging packs: two matching
  examples per tier, zero risk mismatches. Added them to the sandbox preview and
  reviewed the two landing pages and scoped styles; all counts and Ryker content stay
  unchanged. Task artifacts retain exact source paths and risk declarations.
- 2026-11-02 — owner specified `docker.ps, debugging.disk_free, ..` as the display
  format. Swept all four risk rows; made each a comma-separated inline list with the
  requested trailing dots and removed the block style that forced separate lines.
- 2026-11-02 — owner asked for a conversation that looks more like real Slack.
  Swept both product visuals and shared thread styles. Replaced the loose two-message
  exchange and marketing footer with a channel/thread frame, four timestamped messages,
  a concrete registry-credential finding and an explicit fix request. Scenario checked
  against Ryker's current public-site hero example; Emisar's card is unchanged.
- 2026-11-02 — owner asked to drop "Example Slack conversation". Removed the visible
  caption and its spacing rule, retained an accessible figure label, and swept both
  product illustrations: Emisar's policy-preview label remains as approved.
- 2026-11-02 — owner clarified Ryker's pitch as a teammate in Slack and GitHub that
  works 24/7, investigates alerts proactively, proposes fixes and handles assigned
  coding tasks. Applied the approved paragraph to its orchestrator card; swept both
  landing templates and retained Emisar's distinct production-access pitch.
- 2026-11-02 — owner requested a better conversation after approving the teammate
  pitch. Swept the Ryker card and both product templates. Replaced the user-led mirror
  setup exchange with an overnight checkout alert, proactive pool-limit investigation,
  explicit fix assignment and an illustrative draft-PR result. No actual PR or deployed
  fix is claimed; the user-approved pitch and Emisar card remain unchanged.
- 2026-11-02 — owner explicitly rejected the inserted human assignment step and
  requested proactive fixing plus a GitHub PR card. Removed the human message and
  contradictory request requirement, changed Ryker's wording to work already underway,
  and added a static draft-PR preview under its result. Swept all three thread messages,
  reply count, preview markup and styles; no merge or deployment is implied.
