---
name: entity-blocks-with-labeled-fields
description: "multi-fact listings get one labeled block per entity, not one dense row"
scope: cli-output
sources: [internal/cli/profiles.go, internal/cli/presetcmd.go, internal/ui/ui.go]
check: "none"
updated: 2026-09-11
---

# Multi-fact listings: one labeled block per entity, not one dense row

When each entity in a listing carries more than one fact, give each entity a block: a bold
header naming it, then indented `label  value` lines — quiet labels, plain values. State a
fact as a fact ("refreshed 7 hours ago", "default  yes"), not as a cryptic tag, and put the
fix for a problem right where the problem shows.

**Why:** `coop models` packed ids, a live-ness tag, and the env default into one long row
per agent. The user sketched the block form ("Models: …", "Last refreshed at: …") and
asked for color and real readability (2026-07-10). A dense row makes every fact compete
for the same line; a block gives each fact a labeled home, and the header gives the eye a
stable landmark to scan by.

**The other half of the rule: count the facts first.** A block is for an entity with
several facts *a person needs at once* — the single-credential view (`coop credentials
claude work`: refreshed / default / dir). When an entity really carries ONE fact, a block
is padding: `coop models` was rebuilt in 2026-09 as a header plus its ids, because
freshness turned out to be coop's job (it refreshes what it renders) rather than a field
the reader has to judge, and the env default is one sentence, not a column. A block per
task in `coop tasks` would be noise the same way (see [[tag-exceptions-not-every-row]]).

**How to apply:**
- Header: `p.Bold(displayAgentName(id))` at column 0 — the entity's own name, nothing else.
- Fields: two-space indent, the label then the plain value, one fact per line; optional
  fields appear only when set. Blank line between blocks.
- A problem replaces its fact in the same place and carries the exact command that fixes
  it, dim, on the line beneath ([[command-output-tiers]]).
- Commands the user should copy-paste render cyan; pad plain first, then style
  ([[no-color-in-width-fields]]).
- An operation's per-entity outcome (a failed refresh, a failed login) folds into that
  entity's block instead of printing a separate status log that repeats every name.
- One-fact entities stay rows or a wrapped list — don't manufacture a second field to
  justify a block.

See also [[command-output-tiers]], [[list-output-echoes-source]].

## Changelog
- 2026-09-11 — a preset's roles became blocks too (`presetDetail`, presetcmd.go): the role name
  leads, then `Mode:`/`Agent:`/`When:`/`Prompt:`. Two things the card had not said, learned from
  the approved transcript: the LABELS align on one gutter measured across every entity, not just
  within a block, and an optional field costs no line at all — an absent prompt prints nothing,
  never "Coop default" or an empty label.
- 2026-09-11 — `coop models` left this form: its menu is now a header plus width-wrapped ids
  (freshness became automatic upkeep, so `Models:`/`Last refreshed:` had nothing to label), and
  `sources` moved to the surface that still exemplifies the rule, `internal/cli/profiles.go`'s
  single-credential view. Added the "count the facts first" half so the card can't be read as
  "always use a block". Swept every listing in internal/cli: `coop credentials`' overview is a
  measured three-column row per account (one fact each — correct as rows), its single-credential
  view is the canonical block, and `coop tasks`/`coop presets` stay rows. 0 violations.
- 2026-07-10 — created
- 2026-08-06 — card metadata added (format v1); body unchanged
- 2026-08-09 — validate-on-write backfill: read internal/cli/models.go in full (`cmdModels`, its
  only source). 0 violations — exact match: bold-cyan header via
  `p.Bold(p.Cyan(titleName(agent)))`, dim `Label:` + plain-value lines (Models/Last
  refreshed/Default-when-set), a blank line between blocks, and a `--refresh` outcome folded into
  the block's own "Last refreshed" line rather than a separate status log.
