---
name: list-verb-ls
description: "entity-listing subcommands use `ls`; recorded-run views use `runs`, never `list`"
scope: cli-grammar
sources: [internal/cli/help.go, internal/cli/conformance_test.go]
check: "go test ./internal/cli -run TestCLIConformance"
updated: 2026-09-29
---

# Entity listings use `ls`; recorded-run views use `runs`

Entity-listing subcommands use `ls` in dispatch, usage, help, and error suggestions (`coop fork ls`,
`coop tasks ls`). The recorded-run views are `coop net runs` and `coop eval runs`. None accepts a
`list` alias.

**Why:** the user first noticed `coop fork ls` but `coop tasks list` — "some use list, others ls" —
and asked to normalize on `ls`. Later, for v3: "no need to keep backwards compatible cli aliases, v3
can be clean from legacy." So the `list` alias was dropped — one spelling per command, no
dual-accepting compat. This mirrors [[destructive-verb-rm]] (rm is the only destructive verb).

**How to apply:**
- A new listing subcommand: name it `ls`; do NOT add a `list` alias. Advertise `ls` in the help row,
  group-help line, usage string, and unknown-subcommand suggestions.
- Dispatch is a single `case "ls":` (no `, "list"`), and `list` is NOT in `tasksVerbs`/`isTasksSubcommand`.
- Prose descriptions may still say "list" as the English verb ("ls — list tasks by state"); that's
  the *description*, not an accepted subcommand.
- `coop conformance` (TestCLIConformance) asserts `ls` lists and `list` is unknown. The
  intentionally named `coop net runs` and `coop eval runs` list their families' run records;
  neither has a `list` alias or replaces another family's `ls`.

See also [[destructive-verb-rm]] (the sibling: rm is the only destructive verb) and [[help-output-style]].

## Changelog
- 2026-09-29 — swept `eval` and `net` dispatch/help: both use `runs` for recorded runs, while
  `eval ls` lists starter suites. Clarified those two deliberate nouns without adding aliases.
- 2026-09-27 — removed a reference to the obsolete test-only `unknownErr` helper; the
  `TestCLIConformance` command still checks the accepted listing verb.
- 2026-06-29 — created
- 2026-07-02 — revised
- 2026-08-06 — card metadata added (format v1); body unchanged
- 2026-09-11 — `coop net runs` is unchanged by the CLI design; `explain` became `blocked`, which is
  not a listing verb either. No `net ls` alias exists.
- 2026-09-10 — one deliberate non-`ls`: `coop net runs` (task 2026-09-10-make-network-inspection-destination-first). The `net` family is grouped by job (ACCESS/RUNS/REPAIR) and `runs` is a bounded five-row view of recorded runs, not a listing of `net` objects; there is no `net ls` alias. Every other listing verb is still `ls`.
