---
name: acp-preset-owns-toolbar
description: Active ACP presets own the whole lead target and refuse stale Provider/Account editor replays
subsystem: acp
sources: [internal/agent/agent.go, internal/cli/acpcontrol.go, internal/acpproxy/proxy.go, internal/acpproxy/scripted_e2e_test.go, internal/acpproxy/e2e_test.go, internal/cli/help.go]
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

The active target is not toolbar metadata. `acpControl` retains provider, model, and effort, and
each agent declares the ordered ACP session settings that realize it after `session/new`,
`session/load`, and replay. Codex uses `model` followed by `reasoning_effort`; Claude uses `mode`,
`model`, then `effort`; Gemini uses `session/set_model`; Grok carries the target on its launch
command. The proxy acknowledgement-gates those settings and holds prompts behind the chain because
JSON-RPC write order alone does not prove application order (and a Codex model change resets effort).
A rejected setting produces a visible thread notice and blocks the prompt rather than silently
running a different target.

Replay installs every restored session's target gate in the same critical section that publishes the
new child, so a live editor prompt cannot slip through between swap and configuration. Each setting
has a 30-second response bound; timeout or input-write failure follows the same visible failure path.
Reload closes request admission and fails already-pending requests before stopping the child, which
keeps requests out of the snapshot-to-exec gap. Plain native and synthesized model/effort changes are
remembered only after the adapter accepts them; a rejected selector value cannot poison the next
recreate.

## Changelog
- 2026-07-14 — documented target realization, response-gated ordering, and failure behavior; verified
  with the scripted process harness and a real Codex rollout at `gpt-5.6-sol/xhigh`.
- 2026-07-14 — created; verified against controller transitions, mixed-state restore, the Zed-order
  regression, and the live ACP toolbar contract.
