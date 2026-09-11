---
name: entity-blocks-with-labeled-fields
description: "useful facts first; compact tables for comparisons, labeled blocks for detailed permissions and roles"
scope: cli-output
sources: [internal/cli/profiles.go, internal/cli/presetcmd.go, internal/cli/session_cmd.go, internal/ui/ui.go]
check: "none"
updated: 2026-09-11
---

# List useful facts, not implementation bookkeeping

Choose the layout from the reader's job. A preset overview compares a few repeated fields,
so a compact table works. Roles and session permissions have variable-length details, so use
one labeled block per entity. Do not turn either into an exhaustive ledger.

A field earns its place by helping the reader choose, act, or understand a permission:
project, selected agent/account, write access and network rules matter in session configuration.
Verification hashes belong in JSON, not ahead of those facts. Account detail gives useful
launch/default/sign-in actions; token ages, "Default yes", and internal storage paths do not
justify a block merely because the implementation can supply them.

**Why:** the user rejected tidy-looking but useless account and session output, and requested
a table for preset comparison. Formatting cannot rescue a view that omits the user's question.

**How to apply:**
- Header: `p.Bold(displayAgentName(id))` at column 0 — the entity's own name, nothing else.
- Fields: two-space indent, the label then the plain value, one fact per line; optional
  fields appear only when set. Blank line between blocks.
- A problem replaces its fact and carries the exact remedy nearby; keep the cause and remedy
  at readable contrast ([[command-output-tiers]]).
- Commands the user should copy-paste render cyan; pad plain first, then style
  ([[no-color-in-width-fields]]).
- An operation's per-entity outcome (a failed refresh, a failed login) folds into that
  entity's block instead of printing a separate status log that repeats every name.
- One-fact entities stay rows or a wrapped list — don't manufacture a second field to
  justify a block.

See also [[command-output-tiers]], [[list-output-echoes-source]].

## Changelog
- 2026-09-11 — refined from the second complete CLI feedback batch: preset overview is a table,
  lead/roles use aligned Agent/Prompt values, account detail gives actions, and remote sessions
  lead with project/agent/access/network facts. Swept the saved account/preset/session examples;
  digest-first current source is mapped to the task, not claimed fixed in code.
- 2026-09-11 — a preset's roles became blocks too (`presetDetail`, presetcmd.go): the role name
  leads, then `Mode:`/`Agent:`/`When:`/`Prompt:`. Two things the card had not said, learned from
  the approved transcript: the LABELS align on one gutter measured across every entity, not just
  within a block, and an optional field costs no line at all — an absent prompt prints nothing,
  never "Coop default" or an empty label.
- 2026-09-11 — `coop models` left this form: its menu is now a header plus width-wrapped ids
  (freshness became automatic upkeep, so `Models:`/`Last refreshed:` had nothing to label), and
  `sources` moved to the surface that still exemplifies the rule, `internal/cli/profiles.go`'s
  single-credential view. Added the "count the facts first" half so the card can't be read as
  "always use a block". Swept every listing in internal/cli: `coop credentials`' overview is a
  measured three-column row per account (one fact each — correct as rows), its single-credential
  view is the canonical block, and `coop tasks`/`coop presets` stay rows. 0 violations.
- 2026-07-10 — created
- 2026-08-06 — card metadata added (format v1); body unchanged
- 2026-08-09 — validate-on-write backfill: read internal/cli/models.go in full (`cmdModels`, its
  only source). 0 violations — exact match: bold-cyan header via
  `p.Bold(p.Cyan(titleName(agent)))`, dim `Label:` + plain-value lines (Models/Last
  refreshed/Default-when-set), a blank line between blocks, and a `--refresh` outcome folded into
  the block's own "Last refreshed" line rather than a separate status log.
