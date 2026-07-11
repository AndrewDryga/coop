# One frozen lexicon for usage placeholders

Every `Usage:`/`usage:` string and error hint spells a value the SAME way, in `<angle>` brackets,
using this fixed lexicon — so a value never reads `m` here and `<m>` there:

| Value | Placeholder |
| --- | --- |
| a coding agent | `<agent>` (or the literal `claude\|codex\|gemini\|grok`) |
| a credential name | `<name>` (a bare arg) or `<credential>` (the slot in `coop credentials <agent> <credential> …`) |
| a model | `<model>` |
| a filesystem path (dir or file) | `<path>` |
| a task id | `<id>` |
| a count | `<n>` |
| "one or more" / continuation | ASCII `...` — never the Unicode `…` |

**Why:** the v3 audit found the same value spelled `p` / `<name>`, `m` / `<m>` / `<model>`,
`<dir>` / `<path>`, and Unicode vs ASCII ellipses across help and error strings — noise that makes
the CLI read as several tools. A user shouldn't have to learn that `m` and `<model>` mean the same.

**How to apply:**
- A new usage/error string → use the placeholders above; wrap in `<…>`; ASCII `...` for repetition.
- Never abbreviate to a single letter (`p`, `m`) and never use the Unicode ellipsis in a usage string.
- The `coop-conformance` test ([[2026-07-02-cli-conformance-test-graduate-agent-rules-into-t]]) enforces this once it lands.

See also [[help-output-style]].
