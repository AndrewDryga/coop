# Help output: UPPERCASE section headers, one command per line, no "·"

`coop help` and every `coop <cmd> --help` are a scannable command reference, not prose:

- Section headers are UPPERCASE (`AGENTS`, `FORKS`, `UNATTENDED`, `SETUP & MAINTENANCE`);
  sub-labels are capitalized too (`Usage:`, `FLAGS`, `REVIEW`).
- One command per line. Never collapse distinct commands into a `coop fork <verb>`
  placeholder, and never pile several commands' descriptions behind a `·`.
- No `·` (middle dot) anywhere in help text — split into labeled lines or list rows.
- Pad the command column on PLAIN text so a description never glues to a long command
  (`row()` keeps a minimum gap) — see [[no-color-in-width-fields]].
- Flags, examples, and sub-verbs live in the command's own `coop <cmd> --help`, not
  crammed into the top-level index.

**Why:** the top-level help is scanned, not read. Lowercase headers, collapsed verbs, and
`·`-separated descriptions read as clutter; people expect a man-page-like reference where
each command stands on its own line. "I never saw any docs collapsing it like that."

**How to apply:**
- New command → add a one-line row to `helpText` (under its group) AND a `commandHelp`
  entry (synopsis + `Usage:` + flags). A test ties `commandHelp` to `topLevelCommands`.
- Never put `·` in a help string. Guard (help text only — runtime status/stat lines may
  still use `·` as a separator): `grep -n '·' internal/cli/help.go` should be empty, and in
  `internal/cli/fork.go` only the non-help paths (`forkBrief`, the merge prompt) may have it.
