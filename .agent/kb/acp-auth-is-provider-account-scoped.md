---
name: acp-auth-is-provider-account-scoped
description: ACP initialize capability truth and successful authentication belong to one provider account
subsystem: acp
sources: [internal/acpproxy/proxy.go, internal/acpctl/control.go, internal/agent/agent.go, internal/agent/target.go, internal/box/profiles.go, internal/cli/commands.go, internal/cli/rotation.go, internal/acpproxy/scripted_e2e_test.go]
updated: 2026-08-17
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
correlated prompt on the next runnable, non-rate-limited account. Ladder construction omits a native
credential that its adapter already knows requires another login, while preserving env-backed and
opaque credentials; the Account selector uses that same runnable set and does not offer a known-dead
switch. Terminal recovery recognizes ACP's structured `authentication_failed` error kind, while
provider-owned `AuthSignals` classify the preceding notice without treating arbitrary tool-auth
prose as a credential failure. Provider retargeting happens before Auto resolves, so an account
selected for Claude cannot leak into a Codex child. Replay-time `auth_required` uses the same
recovery path without discarding restored session identity.

Preset ladders are rate-limit policy only. An authentication failure never advances a preset rung:
the selected provider/model/account stays exact and the editor receives its `coop login` command.
Pinned or exhausted plain accounts use the same explicit recovery instead of entering a restart loop.
The rewritten RPC error preserves its structural code and names the exact
`coop login provider@account` command instead of forwarding provider-specific dead-end prose.

## Changelog
- 2026-08-17 - excluded known re-login credentials before launch and from the Account selector,
  recognized the observed Claude `authentication_failed` shape, kept prose matching adapter-owned,
  and made pinned or exhausted recovery name the exact shell-safe login command
- 2026-08-10 - re-verified after the ACP control plane moved to internal/acpctl (acpcontrol.go →
  control.go, mechanical rename only; commands.go's half — cmdLogin/loginTo — stayed in cli, source
  unchanged); no line citations in prose to update
- 2026-07-14 - created from provider-switch, preset, live prompt, and replay-time authentication fixes
