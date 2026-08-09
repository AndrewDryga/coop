---
name: entity-blocks-with-labeled-fields
description: "multi-fact listings get one labeled block per entity, not one dense row"
scope: cli-output
sources: [internal/cli/models.go, internal/ui/ui.go]
check: "none"
updated: 2026-08-09
---

# Multi-fact listings: one labeled block per entity, not one dense row

When each entity in a listing carries more than one fact (its models, its freshness, an
env default), give each entity a block: a bold-cyan header naming it, then indented
`Label: value` lines — dim labels, plain values. State freshness as an explicit fact
("Last refreshed: just now / 2h ago / never / 12d ago — stale") colored by state, instead
of a cryptic tag; put the how-to-refresh hint right where the staleness shows.

**Why:** `coop models` packed ids, a live-ness tag, and the env default into one long row
per agent. The user sketched the block form ("Models: …", "Last refreshed at: …") and
asked for color and real readability (2026-07-10). A dense row makes every fact compete
for the same line; a block gives each fact a labeled home, and the header column gives the
eye a stable landmark to scan by. One-fact-per-entity listings should stay rows — a block
per task in `coop tasks` would be noise (see [[tag-exceptions-not-every-row]]).

**How to apply:**
- Header: `p.Bold(p.Cyan(titleName(id)))` at column 0 — bold cyan is coop's accent voice.
- Fields: two-space indent, `p.Dim("Label:")` then the plain value; optional fields
  (Default:) appear only when set. Blank line between blocks.
- Freshness/state values: green when fresh, yellow for stale or a failed refresh, plain
  "never"; the fix goes next to the problem, dim, naming the exact command
  ("(coop models --refresh)").
- Commands the user should copy-paste (the how-to lines) render cyan; pad plain first,
  then style ([[no-color-in-width-fields]]).
- An operation's per-entity outcome (a --refresh result) folds into that entity's block
  instead of printing a separate status log that repeats every name.

See also [[command-output-tiers]], [[list-output-echoes-source]].

## Changelog
- 2026-07-10 — created
- 2026-08-06 — card metadata added (format v1); body unchanged
- 2026-08-09 — validate-on-write backfill: read internal/cli/models.go in full (`cmdModels`, its
  only source). 0 violations — exact match: bold-cyan header via
  `p.Bold(p.Cyan(titleName(agent)))`, dim `Label:` + plain-value lines (Models/Last
  refreshed/Default-when-set), a blank line between blocks, and a `--refresh` outcome folded into
  the block's own "Last refreshed" line rather than a separate status log.
