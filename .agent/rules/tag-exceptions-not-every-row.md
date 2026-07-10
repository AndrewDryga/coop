# Tag the exception row, not every row

In list output, a state tag goes only on rows in the *exceptional* state; the common case
stays untagged. One dim caption under the list explains the scheme. Corollary: when two
stacked blocks list the same entities (e.g. a `--refresh` log above the model menu), they
share ONE computed column width so the repeated names line up block-to-block.

**Why:** `coop models` tagged every non-live row "(examples)" — the user: "no need to say
(examples) after each row, and formatting can be much better." A tag repeated on most rows
is noise that buries the one row where a tag means something, and the refresh log padded
names to a hardcoded 8 while the menu computed 6, so the same agent names jagged between
blocks. Marking only the exception (like the credentials list's single "(default)") keeps
the eye on the signal.

**How to apply:**
- Pick the exceptional state (default, pinned, blocked) and tag only those rows, dim,
  after the content — like the credentials list's single "(default)".
- Say what untagged rows mean once, in a dim caption under the list — never per row.
- Compute the name-column width once (`colWidth`) and pass it to every block that renders
  that column; never hardcode a width next to a computed one.
- Multi-column how-to/legend blocks: pad every column (`colWidth` + `padRight`) so the dim
  notes align — no ad-hoc gaps.
- When the "tag" is really a fact with detail behind it (how fresh, since when), promote
  it to a labeled field in a per-entity block instead — see [[entity-blocks-with-labeled-fields]].
- Not mechanically lintable (a tag string is just text) — enforce in review.

See also [[no-color-in-width-fields]], [[command-output-tiers]], [[help-output-style]].
