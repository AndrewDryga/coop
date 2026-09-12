---
name: selectors-need-a-real-choice
description: Hide an already-selected singleton dropdown without discarding its underlying native state
scope: cli-output
sources: [internal/acpctl/control.go, internal/acpctl/visible_selects.go, internal/acpctl/visible_selects_test.go]
check: go test ./internal/acpctl -run 'TestACPOnlyMeaningfulSelects|TestACPGroupedAndUnknownSelects'
updated: 2026-09-12
---

# Hide dropdowns that offer only their already-selected value

A select with exactly one selectable value equal to its current value has no action to offer.
Do not render it. In particular, a repository without presets must not show Preset=None.
Apply this to native provider options and Coop-owned options, on setup, updates, and replay.

**Why:** The user reported the one-option None popup on 2026-09-12 and requested this behavior
for dropdowns generally.

**How to apply:** Filter at the presentation boundary. Preserve native option/cache truth so a
hidden model still participates in validation, acknowledgements, and restoration. Preserve
booleans and unknown shapes; a lone alternative different from the current value remains useful.

## Changelog
- 2026-09-12 — swept all six ACP output projections in control.go; fixed inert Coop/native selects there and covered flat/grouped values and all initial/update/replay/ack routes. Empty or unknown option sets remain unchanged because the correction concerns a selected singleton.
