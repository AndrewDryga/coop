# Listing subcommands use `ls` — the only spelling (no `list` alias in v3)

Every subcommand that lists things uses `ls` as its verb — in the dispatch, usage, help row, and
error suggestions (`coop fork ls`, `coop tasks ls`). It is the ONLY accepted spelling: v3 keeps no
`list` alias.

**Why:** the user first noticed `coop fork ls` but `coop tasks list` — "some use list, others ls" —
and asked to normalize on `ls`. Later, for v3: "no need to keep backwards compatible cli aliases, v3
can be clean from legacy." So the `list` alias was dropped — `ls` everywhere, one spelling, no
dual-accepting compat. This mirrors [[destructive-verb-rm]] exactly (rm is the only destructive verb).

**How to apply:**
- A new listing subcommand: name it `ls`; do NOT add a `list` alias. Advertise `ls` in the help row,
  group-help line, usage string, and `unknownErr` suggestion list.
- Dispatch is a single `case "ls":` (no `, "list"`), and `list` is NOT in `tasksVerbs`/`isTasksSubcommand`.
- Prose descriptions may still say "list" as the English verb ("ls — list tasks by state"); that's
  the *description*, not an accepted subcommand.
- `coop conformance` (TestCLIConformance) asserts `ls` lists and `list` is unknown.

See also [[destructive-verb-rm]] (the sibling: rm is the only destructive verb) and [[help-output-style]].
