---
name: acp-preset-owns-toolbar
description: Active ACP presets own the whole lead target and refuse stale Provider/Account editor replays
subsystem: acp
sources: [internal/cli/acpcontrol.go, internal/cli/acpcontrol_test.go, internal/acpproxy/e2e_test.go, internal/cli/help.go]
updated: 2026-07-14
---

ACP selection is tagged: `Preset != ""` means Provider and Account are empty. A preset's ladder
owns provider, model, effort, and account fan-out, including cross-provider rungs; no plain
Provider/Account override is applied in `presetRotation` or `spawnTarget`.

The stale editor replay trap has two orders. If an editor replays Provider=codex and then
Preset=frontier, entering the preset clears the plain state and the first frontier rung wins. If
it replays Preset=frontier and then hidden Provider or Account, those writes are acknowledged as
no-ops and do not restart. Selecting Preset=None keeps the current effective `c.lead` as the plain
Provider, clears Account to Auto, and never restores hidden pre-preset values.
Manual Provider or Account selection therefore starts by selecting Preset=None.

## Changelog
- 2026-07-14 — created; verified against controller transitions, mixed-state restore, the Zed-order
  regression, and the live ACP toolbar contract.
