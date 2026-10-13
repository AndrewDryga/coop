---
name: cli-text-is-plain-english
description: CLI docs and messages use direct English while preserving commands, facts and safety details
scope: cli-output
sources: [internal/cli/help.go, internal/cli/models.go, internal/cli/usage.go, internal/box/launch_sections.go, internal/tasks/cmd.go, docs, install.sh]
check: none
updated: 2026-10-13
---

# Write CLI docs and messages in plain English

Say what happened and what the user can do next. Prefer "could not check this sign-in" to
"authentication authority unavailable", and "saved private commits" to "committed custody".
Keep technical names when users need them to find a file, understand a restriction or use an API.

Use ordinary sentence punctuation. Cut staged introductions, unsupported guesses, inflated
claims and sentences that repeat the previous point. Keep useful headings, status symbols and
tables. A warning about missing evidence, billing or deletion carries information; do not remove
it merely because it contains a contrast or qualification.

Describe the observed state without adding a cause, time or stage the code does not establish.
Do not replace every dash mechanically; a separator can be clearer than repeated colons.
Keep edited Markdown paragraphs wrapped without changing their examples or technical meaning.

Preserve command syntax, flags, paths, configuration/API values and link targets. Never rewrite
provider output or protocol values. Trace callers before changing a string used for comparisons,
stored templates or both human and machine output. Generated docs and transcript fixtures must
follow their source. A changed expected sentence is not permission to weaken its behavior test.

**Why:** the user asked to apply [Humanizer](https://github.com/blader/humanizer) to all CLI docs
and outputs and to use simple English where it helps. This pass used version 3.1.0 at commit
225a6f39ac85f76ee48dbad772ea4abe4ed6c9d8. Its factual-preservation and technical-voice rules
control the edit; editorial judgment is still needed, so there is no keyword-ban check.

## Changelog
- 2026-10-13 — the user approved three self-review corrections: describe current rate limits
  without attributing earlier task limits to preflight, restore preset-mode separators and
  rewrap the isolated-fork recovery paragraph. Swept preset-mode source/tests/fixtures and
  nearby loop limit notices; only the all-target notice incorrectly attributed earlier limits.
- 2026-10-12 — surveyed all help pages, 13 public prose guides and 208 runtime Go sources plus
  the installer. Simplified dense help, usage, service, network, task and fork wording. Removed
  a retired coop ps remedy and unsafe duplicate-task deletion advice. Preserved decision scaffold
  markers, cancellation/staleness comparison strings, shared JSON verdicts and provider bytes.
  Changes remain local and pending the user's review; no publication or prior approval is claimed.
