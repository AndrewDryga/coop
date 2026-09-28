---
name: shell-completion-current-word
description: Shell completion replaces the current word, and Bash 3.2 needs IFS kept away from the COMP_WORDS slice
subsystem: cli
sources: [internal/cli/completion.go, internal/cli/completion_test.go, internal/cli/approved_runtime_output_test.go, internal/cli/testdata/approved/75-completion-bash.txt]
updated: 2026-09-28
---

`coop __complete` receives every word after `coop` through the word being edited, including an
empty final word after a space. Its output replaces that current word. `coop loop<Tab>` therefore
returns `loop`; only `coop loop <Tab>` returns targets and flags. Returning next-slot candidates
for an exact current command makes Zsh reject them instead of inserting a space.

The Bash script must not set function-wide IFS before expanding the `COMP_WORDS` slice. Bash 3.2
(the macOS system shell) merges those arguments under newline-only IFS, so the backend receives
`loop ` as one word. IFS belongs on `read` while collecting each output line into a quoted array
element. That also preserves spaces and literal glob characters in a candidate. Completion is
best-effort, so a lookup failure produces no candidates rather than blocking the shell.

Bash also treats `:` as a Readline word break by default. Bash 3.2 keeps `codex:model` together
in COMP_WORDS; Bash 5 splits it into three entries. Rejoin those entries for the backend, then
return only the suffix Readline is replacing. Otherwise model completion either yields nothing
or duplicates the provider prefix (`codex:codex:model`). Do not change global COMP_WORDBREAKS:
that would alter other commands' completion, and users may have deliberately customized it.

`TestBashCompletionPreservesWordsAndCandidates` executes the generated script against a strict
backend, not a handwritten replica. Backend tests pin exact-word versus next-slot semantics;
the existing Zsh PTY regression separately covers its command-local correction suppression.

The complete emitted script is also pinned by `TestApprovedCompletionScripts`, including its
executable body, in `testdata/approved/75-completion-bash.txt`. A reviewed behavior change must
update that transcript as well; a behavioral test pass alone does not prove the CLI gate passes.
Review the full mismatch, not only its reported first differing line, and retain the byte-exact
assertion. Update the approved transcript in the same commit as a script-output change.

## Changelog
- 2026-09-28 — replaced process jargon with the actionable transcript-update rule; rechecked the
  script and approved fixture paths. The earlier fix changed the expected executable body, not only
  its header, and did not relax an output assertion.
- 2026-09-22 — repaired the omitted approved transcript; its exact-output check expected the old
  executable body, not just an older header.
- 2026-09-21 — reproduced empty and partial completion failures on Bash 3.2 and removed backend
  exact-command auto-advancement. The same script is also exercised on Linux Bash during audit.
  Actual typed model completion exposed both colon shapes on Bash 3.2 and 5.2; covered local
  rejoining and prefix trimming without global shell changes.
