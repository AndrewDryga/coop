---
name: usage-placeholder-style
description: "usage metavariables use angle brackets and stable names; optionality stays outside the placeholder"
scope: cli-grammar
sources: [internal/cli/help.go, internal/cli/fork_cmd.go, internal/cli/presetcmd.go, internal/cli/commands.go, internal/cli/acp_cmd.go, internal/cli/loop_cmd.go, internal/cli/models.go, internal/cli/profiles.go, internal/cli/completion.go, internal/cli/worker_cmd.go, internal/tasks/cmd.go, internal/tasks/backlog.go, internal/forkctl/rm.go, internal/forkctl/review.go, internal/forkctl/merge.go, internal/forkctl/supervise.go, internal/consult/wrapper.go, internal/preset/wrapper.go, internal/cli/conformance_test.go]
check: none
updated: 2026-09-11
---

# Put usage metavariables in angle brackets and name them consistently

In `Usage:` lines, synopsis rows, and usage errors, a replaceable value uses `<angle-brackets>`.
Optionality wraps the whole token (`[<agent>]`), alternatives stay inside one token
(`<target|preset>`), and repetition uses ASCII `...` (`[<path>...]`). Literal flags and verbs remain
literal.

Use these established names:

| Value | Placeholder |
| --- | --- |
| command and trailing arguments | `<command>`, `<args>...`, or the established raw-command `<cmd...>` |
| Coop/provider argument groups | `<coop-flags>`, `<agent-args>...` |
| coding agent or account | `<agent>`, `<account>` |
| stored credential | `<credential>` |
| model, target, preset | `<model>`, `<target>`, `<preset>`, `<target|preset>` |
| fork or other resource name | `<name>` |
| filesystem path | `<path>` or `<absolute-path>` when absoluteness is required |
| task/ref/count/shell | `<id>`, `<ref>`, `<n>`, `<bash|zsh>` |
| structured task fields | `<title>`, `<project>`, `<context>`, `<acceptance>`, `<approach>`, `<subtask>` |
| prompt text | `<prompt>` |

Provider literals may replace `<agent>` when listing the closed accepted set, for example
`<claude|codex|gemini|grok>`. A usage error that interpolates an actual chosen value may print that
value literally; it is no longer a metavariable.

Inside a page where every value is the same kind of thing, the resource-name form is the plain one:
`coop help presets` writes `coop presets <name>` / `coop presets init [<name>]` / `coop help <name>`
because the page is about presets and nothing else. `<preset>` stays the placeholder everywhere the
slot competes with another kind of value (`coop <target|preset>`, the top-level rows, every usage
error the presets command raises).

**Why:** bare forms such as `[agent [credential]]`, `[name]`, `[paths...]`, and single-letter
placeholders make values look like fixed syntax and force users to learn several spellings for the
same slot. The vocabulary is intentionally descriptive, not artificially frozen: current commands
legitimately need paths, refs, task fields, and argument groups that the old table omitted.

**How to apply:**
- Reuse the nearest semantic placeholder before adding a new one; never abbreviate a value to one
  letter.
- Keep `...` outside a closing angle bracket for repeated individual values. Preserve `<cmd...>`
  only for the established raw-command tail, where command and arguments are intentionally one slot.
- `TestCLIConformance/usage_metavariables` pins the public forms that previously drifted, and its
  target-placeholder subtest covers launch/peer grammar. The card remains `check: none` because
  provider-specific usage errors are not exhaustively parsed; review still owns the full rule.

Related: [[help-output-style]].

## Changelog
- 2026-09-11 — recorded the approved presets page's `<name>` slot (the page's only value is a
  preset) and pinned it in `TestCLIConformance/usage_metavariables`. Swept the rest of the presets
  surface: the top-level row, `coop <target|preset>`, and every `coop presets` usage error still
  say `<preset>`, so the two forms never appear in the same sentence.
- 2026-09-03 — corrected the audited Coop help/parser and shipped wrapper forms, expanded the
  honest vocabulary, and added a focused conformance regression. Kept `check: none` because that
  test deliberately does not claim exhaustive provider-argument parsing.
- 2026-08-26 — added focused target/peer placeholder coverage to `TestCLIConformance`.
- 2026-08-09 — normalized Unicode ellipses, task-field abbreviations, and task title usage.
- 2026-07-02 — created; revised 2026-07-11.
