# Listing subcommands advertise `ls` (and accept `list` as an alias)

Every subcommand that lists things uses `ls` as its canonical verb and shows `ls` in its usage, help
row, and error suggestions — `coop fork ls`, `coop tasks ls`. `list` is accepted everywhere as a
forgiving alias, but is never the form advertised.

**Why:** the user noticed `coop fork ls` but `coop tasks list` — "some use list, others ls" — and
asked to normalize. `ls` is the unix/git/docker idiom and was already the verb for `coop fork`;
`coop tasks` was the lone outlier (it advertised `list` while quietly accepting both). This mirrors
[[destructive-verb-rm]] exactly: the short unix verb is canonical and advertised, the long form is a
forgiving alias.

**How to apply:**
- A new listing subcommand: name it `ls`, accept `list` as an alias, advertise only `ls` (help row,
  group-help line, usage string, `unknownErr` suggestion list).
- Keep both in the dispatch `case "ls", "list":` (canonical first) and in `isTasksSubcommand`.
- Prose descriptions may still say "list" as the English verb ("ls — list tasks by state"); that's
  the *description*, not the advertised subcommand name.

See also [[destructive-verb-rm]] (the sibling short-verb precedent) and [[help-output-style]].
