---
name: acp-auth-is-provider-account-scoped
description: ACP initialize capability truth and successful authentication belong to one provider account
subsystem: acp
sources: [internal/acpproxy/proxy.go, internal/cli/acpcontrol.go, internal/cli/commands.go, internal/acpproxy/scripted_e2e_test.go]
updated: 2026-07-14
---

An editor's `initialize` request can be reused when a child is replaced, but its response is fresh
truth from that child. Replay is therefore phased: initialize, one compatible successful
authenticate for the exact provider and concrete account, then session restoration. A method is
compatible only when the replacement advertises its `methodId`; failed replay authentication is
retired, and legacy unscoped authenticate lines are dropped from resume snapshots.

Coop owns credentials outside ACP. Provider `authMethods` and logout capability are hidden from the
editor-facing initialize response, and editor `authenticate`/`logout` requests are rejected with the
exact `coop login provider@account` recovery. Otherwise a provider switch leaves Zed with an
immutable auth menu for the previous child.

Plain Account=Auto remains policy, not a hidden pin. The controller tracks its concrete replacement
account and failed provider-account pairs separately, persists them through SIGHUP, and retries a
correlated prompt on the next signed-in, non-rate-limited account. Provider retargeting happens before
Auto resolves, so an account selected for Claude cannot leak into a Codex child. Replay-time
`auth_required` uses the same recovery path without discarding restored session identity.

Preset ladders are rate-limit policy only. An authentication failure never advances a preset rung:
the selected provider/model/account stays exact and the editor receives its `coop login` command.
Pinned or exhausted plain accounts use the same explicit recovery instead of entering a restart loop.

## Changelog
- 2026-07-14 - created from provider-switch, preset, live prompt, and replay-time authentication fixes
