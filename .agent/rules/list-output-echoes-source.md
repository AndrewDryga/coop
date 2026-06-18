# List output echoes the source's own format, and groups breathe

A command that prints items which exist in a canonical on-disk form should render them in
that form, and separate grouped sections with a blank line. `coop tasks list` first printed
a bare `[ ]` with no bullet and ran several files together; it should print `- [ ]` (the
real `.agent/TASKS.md` marker) with a blank line between files.

**Why:** list output is scanned and often copied. Echoing the source marker (`- [ ]`) means
what you see is what the file holds — paste-able, and instantly recognizable as the task
format rather than a coop-only rendering. Whitespace between groups (per-file, per-agent)
turns a wall of lines into scannable sections. The bar, in the user's words: "more spaces
between task files and some - or * before [ ]."

**How to apply:**
- Echoing on-disk items (tasks, diffs, config entries) → keep their native markers/leading
  syntax; don't strip them to a coop-only form.
- Multi-section output (per file, per agent) → clear vertical space *between* sections (two
  blank lines reads better than one for file groups), while the section header stays tight
  to its own items.
- Siblings to keep consistent: `coop tasks list` (done); `coop profiles` groups by agent —
  give it the same inter-section blank line if it's ever touched.
- Pairs with [[help-output-style]] (the help reference's scannability) and
  [[no-color-in-width-fields]] (column alignment).
