# List output echoes the source's own format, and groups breathe

A command that prints items which exist in a canonical on-disk form should render them in
that form, and separate grouped sections with a blank line. The original `coop tasks list`
(legacy single-file queue) first printed a bare `[ ]` with no bullet and ran files
together; it was fixed to echo `- [ ]` with a blank line between files. The folder-mode
`coop tasks ls` groups by state directory (`todo`/`in_progress`/`blocked`/`done`) and
breathes the same way — a blank line between state groups.

**Why:** list output is scanned and often copied. Echoing the on-disk shape (the marker,
or the state-folder grouping) means what you see maps straight to what's on disk —
recognizable as the task format rather than a coop-only rendering. Whitespace between groups
turns a wall of lines into scannable sections. The bar, in the user's words: "more spaces
between task files and some - or * before [ ]."

**How to apply:**
- Echoing on-disk items (tasks, diffs, config entries) → keep their native markers/leading
  syntax; don't strip them to a coop-only form.
- Multi-section output (per file, per agent) → clear vertical space *between* sections (two
  blank lines reads better than one for file groups), while the section header stays tight
  to its own items.
- Siblings to keep consistent: the `coop tasks` listing (done); `coop credentials` groups by
  agent — give it the same inter-section blank line if it's ever touched.
- Pairs with [[help-output-style]] (the help reference's scannability) and
  [[no-color-in-width-fields]] (column alignment).
