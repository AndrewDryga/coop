---
name: site-emisar-production-pitch
description: Emisar promotion connects unattended local sandbox work to controlled production access
scope: docs
sources: [tools/site/index.tpl.html, site/index.html]
check: none
updated: 2026-11-02
---

# Connect Emisar to the work beyond the sandbox

When promoting Emisar to co:op readers, explain the next need: a sandbox lets agents
work unattended locally, while real projects also need agents to inspect production.
Name the useful work: investigate incidents, check real environments, and test hypotheses.
Describe the controls in plain English: what runs automatically, what needs approval,
and what stays off-limits. Preserve the owner-approved passage when reusing it.

**Why:** the owner, 2026-11-02: "now with a sandbox your agents can run unattended
locally but most real projects also need let agents access your production".

**How to apply:** put this connection in Emisar's product card on the sandbox landing page.
Link to `https://emisar.dev`, follow [[site-external-links-nofollow]], and regenerate
through `make docs`. Explain the access controls without promising risk-free operation
or implying an automatic integration.

## Changelog
- 2026-11-02 — swept both landing templates and their generated pages. The existing
  footer links and homepage task-count mention did not explain Emisar's purpose; added
  the approved production-access pitch. No other Emisar product
  pitches in `tools/site/` or `site/` need the same correction. Editorial judgment keeps
  the positioning rule under review rather than an exact-copy test.
- 2026-11-02 — owner clarified the page pairing: Emisar is featured only on the sandbox
  landing page. Swept both landing templates and generated pages; kept the approved
  Emisar pitch on sandbox and removed its feature card from orchestrator.
