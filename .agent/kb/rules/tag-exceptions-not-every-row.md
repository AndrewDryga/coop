---
name: tag-exceptions-not-every-row
description: "tag only the exceptional row; explain the scheme once in a dim caption"
scope: cli-output
sources: [internal/cli/models.go]
check: "none"
updated: 2026-09-11
---

# Tag the exception row, not every row

In list output, a state tag goes only on rows in the *exceptional* state; the common case
stays untagged. Say what the tag means where it appears — a dim caption under the list, or
a tag that is a whole short sentence — never a legend the reader has to hold. Corollary:
when two stacked blocks list the same entities, they share ONE computed column width so
the repeated names line up block-to-block.

**Why:** `coop models` tagged every non-live row "(examples)" — the user: "no need to say
(examples) after each row, and formatting can be much better." A tag repeated on most rows
is noise that buries the one row where a tag means something, and the refresh log padded
names to a hardcoded 8 while the menu computed 6, so the same agent names jagged between
blocks. Marking only the exception (like the credentials list's single `default`) keeps
the eye on the signal.

**How to apply:**
- Pick the exceptional state (default, pinned, blocked, could-not-refresh) and tag only
  those rows, after the content — like the credentials list's single `default`.
- Say what untagged rows mean once, in a dim caption under the list — or make the tag a
  short sentence that carries its own meaning — never a marker repeated per row.
- Compute the name-column width once (`colWidth`) and pass it to every block that renders
  that column; never hardcode a width next to a computed one.
- Multi-column how-to/legend blocks: pad every column (`colWidth` + `padRight`) so the dim
  notes align — no ad-hoc gaps.
- When the "tag" is really a fact with detail behind it (how fresh, since when), promote
  it to a labeled field in a per-entity block instead — see [[entity-blocks-with-labeled-fields]].
- Not mechanically lintable (a tag string is just text) — enforce in review.

See also [[no-color-in-width-fields]], [[command-output-tiers]], [[help-output-style]].

## Changelog
- 2026-09-11 — re-verified against the rebuilt `coop models`, which is now the cleanest example of
  the rule: a healthy agent block is its name and its ids, and ONLY an agent whose catalog could
  not be refreshed gets a line (`⚠ showing the list saved 2 days ago — could not refresh: Docker is
  not running`) — a tag that explains itself, so the old dim caption is gone. Generalized the
  "one dim caption" clause to cover that, and dropped the `--refresh` log from the corollary (that
  log no longer exists; upkeep is automatic and silent). Swept internal/cli's listings: credentials
  tags only the default account, tasks/presets tag only the exceptional state. 0 violations.
- 2026-07-10 — created
- 2026-08-06 — card metadata added (format v1); body unchanged
- 2026-08-09 — validate-on-write backfill: read internal/cli/models.go in full (its only source).
  0 violations — the old per-row "(examples)" tag this rule was written against no longer exists;
  `coop models` fully migrated to the entity-block form (see [[entity-blocks-with-labeled-fields]]),
  with one dim caption ("any model id the agent's CLI accepts works — an unrefreshed list shows
  examples") and a single shared `colWidth` computation for the how-to block's columns.
