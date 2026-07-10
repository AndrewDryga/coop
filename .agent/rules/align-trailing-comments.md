# Trailing `#` comments in an example line up in one column

In a code example, every trailing comment sits in ONE vertical column — one space past
the block's widest commented line. A single line whose comment pokes out past the rest is
the defect. This covers the `coop help` manual examples (`internal/cli/help.go`, the single
source for `docs/cli.md` + `docs/man/coop.1` + `site/llms.txt`), README code fences, and the
site's `<pre>` blocks.

**Why:** "I'm really annoyed by all the times you did not align comments in one vertical line
nicely!!" — the `coop presets` block and the roles YAML both had one long line (`coop loop
--preset frontier`, `agent: claude:claude-opus-4-8`) whose comment jutted past the column the
other rows shared. Same failure as a help row pushing past the description gap — see
[[help-output-style]].

**How to apply:**
- One column per example. The preset YAML's blank-separated `lead:`/`thinker:`/`critic:`/
  `fast:` stanzas still share ONE column (all lines are comparable width). But two disparate
  command lists in the same fence — long `coop fork … --tasks …` vs short `coop fleet watch`
  — align *separately*; don't drag the short list out to the long one's column.
- Exception, mirroring help-output-style's "completeness beats alignment" verb-row: a lone
  over-long line (a compound `-- -p "…"` invocation, ~28+ runes longer than the next) stands
  on its own — align the comparable lines and let its comment trail. Better still, if a value
  is that long, drop the trailing comment and document the field on a full `#` line above it,
  the way `.agent/presets/frontier/preset.yaml` does throughout.
- Don't churn an already-aligned block: if the comments already share a column, keep the
  author's gap — this rule fixes jaggedness, it doesn't impose a canonical gap width.
- `help.go` is the source of truth for the manual: edit it, run `make docs` (docs-check
  enforces the sync), and the alignment flows into cli.md/coop.1/llms.txt.

**Mechanically:** `make align` (or `tools/align-comments.py --check` / `--write`) over README,
the site pages, and cli.md. It aligns jagged blocks, skips aligned ones and lone outliers, and
counts width visually (HTML tags/entities and the `<pre>` `>` don't count). The stanza-vs-block
and outlier calls are heuristic, so it's an aid, not a gate — confirm the result in review.

See also [[help-output-style]], [[no-color-in-width-fields]], [[tag-exceptions-not-every-row]].
