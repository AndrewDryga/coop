---
name: list-output-echoes-source
description: "list output echoes the canonical shape and separates grouped sections with whitespace"
scope: cli-output
sources: [internal/tasks/cmd.go, internal/tasks/queue.go, internal/cli/profiles.go]
check: "none"
updated: 2026-09-03
---

# Echo canonical list shapes and give groups room to breathe

A command that lists canonical records should make its output map visibly back to those records.
Keep native markers or state names, and place blank space between groups instead of flattening them
into one wall.

- `coop tasks`/`coop tasks ls` group task folders by lifecycle state and leave a blank line between
  non-empty state or project sections.
- `coop credentials` groups credentials by agent and leaves a blank line between agent blocks.
- New list surfaces should preserve the record's recognizable leading syntax when one exists,
  rather than inventing a second display-only form.

**Why:** list output is scanned and often copied. When it echoes the source shape, a user can move
between terminal output and disk without translating Coop-only notation; whitespace makes separate
authorities and states obvious.

**How to apply:**
- Keep a section header tight to its own items and put vertical space between sections.
- Pad and style only after deciding the plain source-shaped fields; see
  [[no-color-in-width-fields]].
- This remains a review rule: the current task and credential renderers exhibit the behavior, but
  no test honestly gates spacing and source shape across both surfaces or every future list.

Related: [[help-output-style]] and [[no-color-in-width-fields]].

## Changelog
- 2026-09-03 — removed the retired single-file queue narrative, added the actual task renderer and
  credential-list sources, and re-verified current state/project/agent grouping. Kept `check: none`
  rather than claiming a package test enforces future list design.
- 2026-08-10 — task queue implementation moved from `internal/cli` to `internal/tasks`.
- 2026-06-17 — created; revised 2026-07-09.
