# Never style a string inside a width-padded format field

When printing aligned, columnar output, pad **plain** text to the column width, then apply
color/bold to the result — never the other way round. Concretely: don't pass
`ui.Bold(x)` / `ui.Dim(x)` / `ui.Green(x)` into a width verb like `%-16s`. Either use a
plain `%-16s` with the unstyled value, or render the whole row/line with plain values and
style the finished string.

```go
// WRONG — the ANSI bytes count toward the 16-char width; the header drifts left.
fmt.Printf("  %-16s %-8s\n", ui.Bold("NAME"), ui.Bold("STATE"))

// RIGHT — pad plain, then bold the rendered line.
fmt.Print(ui.Bold(fmt.Sprintf("  %-16s %-8s\n", "NAME", "STATE")))
```

**Why:** `%-16s` pads to 16 *bytes* of the string it's given. A styled string is
`"\x1b[1mNAME\x1b[0m"` — 4 visible chars wrapped in ~8 invisible escape bytes — so the
verb thinks the cell is already 12 wide and adds only 4 spaces instead of 12. Every styled
header column loses ~8 chars of padding and stops lining up with the plain data rows
beneath it. It bites only on a terminal (colors are off when stderr isn't a tty), so it
sails through piped tests and shows up exactly where a user sees it. Tables must be aligned
when output.

**How to apply:**
- Aligned/columnar output → pad plain values in the `%-Ns`, then wrap the whole rendered
  line (or an already-padded cell) in `ui.Bold`/color.
- Color in non-padded output (`%s`, standalone labels, the doctor report) is fine — there's
  no width to throw off.
- Guard (review, not a clean auto-lint — `%s` bold is legitimate): inspect each hit of
  `grep -rnE 'ui\.(Bold|Dim|Green|Red)\(' internal/ | grep -v _test` and confirm none lands
  inside a `%-N` width field. After a fix the only hits should be non-width `%s` uses or
  whole-line wrapping.
