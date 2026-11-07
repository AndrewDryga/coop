---
name: acp-auth-is-provider-account-scoped
description: ACP initialize capability truth and successful authentication belong to one provider account
subsystem: acp
sources: [internal/acpproxy/proxy.go, internal/acpctl/control.go, internal/acpctl/network.go, internal/agent/agent.go, internal/agent/target.go, internal/box/network_bundles.go, internal/box/profiles.go, internal/cli/acp_cmd.go, internal/cli/acp_network.go, internal/cli/commands.go, internal/cli/rotation.go, internal/acpproxy/scripted_e2e_test.go]
updated: 2026-11-07
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
A session started with `--egress offline` (`MarkOffline`) is the exception. Its box reaches no
provider, so a failed sign-in check says nothing about the host credential. Recovery neither rotates
nor names `coop login`; it says the session is offline. Prompts and replay re-creates take the same
path, because ReplayFailure is `maybeRecoverAuthentication`. Offline from COOP_EGRESS or the project
keeps the sign-in recovery, as a loop's does (298ae529): the scripted ACP harness pins
COOP_EGRESS=offline and tests rotation, and its fixture cannot run those tests open.

Filtered ACP freezes provider **and account** eligibility at supervisor admission. An adapter whose
network bundle depends on its auth family describes the selected family without exposing secret
bytes; the host separately proves that exact account's credential authority. The toolbar, automatic
account rotation, warm/model probes, preset spawns and restored targets all use the frozen account
set. After any rate-limit wait the complete preset closure and each authentication family are
revalidated; the supervisor then passes exact role/peer account bindings to the re-exec, which
applies them before the child validates its required credential scope and creates any mount.
Reusable API-key accounts are ACP-eligible through the broker, which shadows a native key file
beside a Coop-held key; a key only that file holds is refused. One policy cannot grant a provider's
API to a sign-in and withhold it for a key, so the scope offers each provider's accounts of one
kind — that of the account the session names (editor target, peer, preset), else of its first
qualified account (`acpNetworkScope`); explicit targets that mix kinds fail admission. Changing
settings or deleting a role credential after capture refuses the next child rather than
reusing one account's grant for a sibling or silently dropping the role. A changed default does not
refuse: the child pins the default its supervisor admitted (`PinDefaultProfile`, in memory, and it holds
through `NativeAuthorityConfig`, which rereads the authority's defaults file for a session-service child) so a
`coop credentials ... default` in another terminal cannot end or widen a running session. The supervisor does not
make an editor-named account active, so classification reads the admitted scope's accounts, never
the provider's active one.

## Changelog

- 2026-11-07 - an offline editor session said "Sign in to codex@personal on the host" when a restored
  session failed its sign-in check (the account was signed in; offline boxes cannot reach OpenAI). A session
  started with --egress offline now says it is offline and does not rotate. Pinned by
  `TestACPControlOfflineAuthenticationNamesTheNetwork` and `TestACPOwnOfflineFlagNamesTheNetworkOnAFailedSignIn`
  (task 2026-11-07-say-an-offline-editor-session-is-offline-instead).
- 2026-11-04 - the pin now holds in a session-service child's account authority, which rereads the defaults file
- 2026-11-04 - a changed default account no longer refuses filtered children; they pin the admitted default
- 2026-09-19 - API-key accounts are ACP-eligible again, through the broker; the scope offers one
  credential kind per provider
- 2026-09-15 - removed reusable API-key accounts from filtered ACP eligibility; their direct
  CLI/loop broker never falls back to mounting a key in an ACP child
- 2026-09-13 - froze filtered ACP admission at provider/account granularity, pinned role/peer
  accounts across the supervisor re-exec, and added post-wait plus pre-mount validation so one
  portable account cannot admit a host-bound sibling or hide a deleted role
- 2026-08-17 - excluded known re-login credentials before launch and from the Account selector,
  recognized the observed Claude `authentication_failed` shape, kept prose matching adapter-owned,
  and made pinned or exhausted recovery name the exact shell-safe login command
- 2026-08-10 - re-verified after the ACP control plane moved to internal/acpctl (acpcontrol.go →
  control.go, mechanical rename only; commands.go's half — cmdLogin/loginTo — stayed in cli, source
  unchanged); no line citations in prose to update
- 2026-07-14 - created from provider-switch, preset, live prompt, and replay-time authentication fixes
