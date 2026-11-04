---
name: docs-describe-the-current-product
description: README and guides describe the product as it is today; what only older releases need stays in code, MIGRATING.md or CHANGELOG.md
scope: docs
sources: [README.md, docs, MIGRATING.md, CHANGELOG.md, install.sh]
check: none
updated: 2026-11-04
---

# Describe today's product in public docs, not its past

Write the README and guides for someone using the current release. Leave out repository moves,
old signers, removed commands and which versions behaved differently. Examples use a placeholder
such as `VER=vX.Y.Z`, not a real old version.

When old releases still need different handling, keep that in the code that handles them, as
`install.sh` does with its exact list of old signers. Upgrade steps belong in MIGRATING.md,
changes in CHANGELOG.md and maintainer notes in `.agent/kb/`.

**Why:** on 2026-11-04 the owner read the README's manual verification steps, which carried a
`SIGNER_REPO` variable and a note about releases published before the repository transfer, and
asked: "how do we simplify this and stop mentioning legacy stuff".

**How to apply:**
- Before removing a note, check that the remaining steps fail safely for old versions. The manual
  verification rejects an old release with a clear identity error, so it can't pass the wrong one.
- Keep names that are still real: "legacy SSE" is how a reader identifies an MCP transport. Describe
  an API field that still exists by what it does, without calling it legacy.
- Keep limitations and upgrade steps a reader must act on; see
  [[public-release-notes-are-product-focused]].

## Changelog
- 2026-11-04 — created. Swept README.md and docs/*.md for "legacy", "previously", "no longer",
  "historical", "formerly" and "before/after the move". Removed the signer notes from the README
  here. Queued `docs/loop.md`'s "`coop tasks split` ... no longer supported" as
  2026-11-04-describe-the-queue-in-the-loop-guide-without-the. Left the legacy SSE transport,
  `docs/session-api.md`'s legacy `truncated` field and its historical sessions, which describe
  state that still exists.
- 2026-11-04 — the owner asked for the same across the Markdown files. The simple-English pass
  (task 2026-11-04-rewrite-the-public-markdown-docs-in-simple-engli) described `truncated` and older
  sessions without "legacy" or "historical", and removed history from the agents, box,
  configuration, forks, loop and brand docs and the eval example. MIGRATING.md and CHANGELOG.md keep
  old versions, because they are about change.
