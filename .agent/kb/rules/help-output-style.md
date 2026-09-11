---
name: help-output-style
description: "UPPERCASE help headings, aligned command rows, practical prose and numbered how-tos"
scope: cli-output
sources: [internal/cli/help.go, internal/cli/cli.go, internal/cli/fork_cmd.go, internal/cli/presetcmd.go, internal/cli/help_test.go, internal/cli/conformance_test.go]
check: "none"
updated: 2026-09-11
---

# Help output: consistent headings, useful commands, practical guidance

- Formal help reference sections use UPPERCASE headings, including OPTIONS and command groups.
  `Usage:` is a label. This does not turn runtime next-action prose into uppercase headings:
  a sentence explaining which file to edit should remain a sentence, not "EDIT ITS AGENTS".
- One command per row, with a short action-oriented description. Do not hide several commands
  behind a verb placeholder or join their explanations with middle dots.
- Pad columns on plain text before applying color; use a minimum two-space gap even for a long
  syntax example. Top-level command cells stay at most 32 runes; full focused syntax must not
  omit required arguments to satisfy that index limit. See [[no-color-in-width-fields]].
- Group by the user's job. A heading may include one short explanation or concrete file path.
  Options and detailed examples belong on the focused page, not in the top-level menu.
- Explain the thing briefly, then show how to use it. Lists of concepts are actual bullets;
  sequential how-tos use numbered steps with properly nested commands/config. Do not format a
  how-to as if it were a subcommand table.
- Show only accepted arguments in selectable choices. Name the concrete file a command acts
  on: for example, services in `.agent/compose.yml`, not unexplained "sibling services".
- Use a labeled next-help pointer. An orphan `coop help net` is not guidance; say
  `For more details see:`, then indent the command. Avoid repeating content owned by a linked
  topic or appending an unrelated all-commands footer to a self-contained guide.
- Keep indexes plain and scannable. Runtime output examples embedded in help retain their
  actual status formatting; do not rewrite an example merely because runtime stats use `·`.

**Why:** consistent structure lets new users scan without learning a different style for each
page. Help needs to explain what to do, not expose the implementation. Complete examples prevent
future agents from inventing flags or rewriting approved copy.

**How to apply:** update shared renderers, focused pages, completion and generated references
together. Table-driven tests should cover headings, row gaps, and full command-path routing;
the full manual must be assembled from the same pages, not a second command registry.
Existing TestHelpRowsAlign, TestHelpTextAligned, TestAllHelpAvoidsMiddleDots and
TestCLIConformance cover portions of the historical rules, not this entire contract. Expand
their relevant coverage during implementation; `check: none` remains honest until that lands.

## Changelog
- 2026-09-11 — the runtime/integration family's pages are byte-pinned against the approved manual
  (`internal/cli/testdata/approved/{14,15,67,69,73,74,75,76,77}-*.txt`, TestApprovedRuntimeHelpPages):
  run, shell, ACP, the sessions family and each of its five leaves, sign, completion, prompt and
  version. Leaf pages register in the SAME `commandHelp` map under their full path
  ("sessions serve"), so `coop help <family> <leaf>`, `coop <family> <leaf> --help` and the manual
  read one source; `helpForPath` resolves the leaf before falling back to the family. All of these
  pages end themselves, so they joined `selfContainedHelp` and dropped the all-commands footer.
  `TestAllHelpAvoidsMiddleDots` exempts the prompt page: its EXAMPLE quotes `coop prompt`'s own
  output, which this card's last bullet already allows.
- 2026-09-11 — refined uppercase scope after the user rejected uppercase runtime edit/run
  instructions. Swept preset creation, account actions, service hints and help examples; retained
  formal reference headings while turning runtime guidance into direct prose. Retired task flags
  has no replacement help page; the remaining full manual stays derived from its source pages.
- 2026-09-11 — superseded the topic-page Title-case exception after the user's full quick-review
  correction. Swept provider/model/init/preset pages, task shortcuts, network how-tos, and the
  complete manual in the CLI design task; normalized headings and labeled help pointers.
  Runtime implementation remains a separate task gate, not a claim this rule is enforced.

### Earlier history

These entries record earlier decisions; the current rule above supersedes conflicting guidance.

- 2026-09-11 — added the topic-page tier from the approved per-agent/preset transcripts: Title-case
  headings and a closing pointer instead of UPPERCASE headers and the all-commands footer. Swept
  every page rendered through `printCommandHelp`: only the agent pages, `presets`, and the preset
  projection are self-contained today; the command index and every group page keep the reference
  shape unchanged.
- 2026-09-03 — added `forkHelpText` and both test files to `sources`; replaced the false claim that
  `TestHelpRowsAlign` enforced the whole card with an exact test-to-claim map, and set `check: none`
  because the focused regressions do not enforce every page-level claim.
- 2026-08-25 — removed the Fleet-only long verb-list exception, shortened the direct-fork rows,
  and extended `TestHelpRowsAlign` from the top-level index to `forkHelpText`; both surfaces now
  enforce the 32-rune command-cell rule.
- 2026-08-10 — path-only: `forkHelp`/`forkHelpText` stayed in cli as
  `internal/cli/fork_cmd.go` while the `·`-bearing runtime paths (`forkBrief`, the merge prompt)
  left for `internal/forkctl`. The guard is unchanged; its second half now points at two files.
- 2026-06-17 — created
- 2026-07-11 — revised
- 2026-08-06 — card metadata added (format v1); body unchanged
