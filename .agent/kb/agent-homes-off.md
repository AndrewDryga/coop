---
name: agent-homes-off
description: COOP_HOMES=0 connects no account — the credential scope is empty and every brokered sign-in lives in files in the home — so a plain agent run that reaches a provider is refused in the CLI, never in box.Run
subsystem: credentials
sources: [internal/box/auth.go, internal/box/native_broker.go, internal/box/credential_broker.go, internal/box/network_bundles.go, internal/box/run.go, internal/cli/commands.go]
updated: 2026-11-06
---
`credentialScope` is empty when `spec.Homes` is false. Everything that connects an account walks
that scope, so with `COOP_HOMES=0` nothing does: `planNativeAccounts` returns no plan,
`selectCredentialPlan` returns none, and `NetworkProviderBundles` admits no provider, so a filtered
box even blocks the provider's API. It has always meant no sign-in. Before the native broker, the
login itself lived in the home.

Giving such a run a sign-in would need a new home. Each adapter's broker seed (`Seed` in
`internal/agent`) is placeholder files the agent CLI reads from its home: Claude OAuth writes
`.credentials.json` and `.claude.json`, Codex and Grok write `auth.json`, and Gemini writes
`settings.json` and `google_accounts.json`. `prepareNativeAuth` writes them into the mounted native
home. Only a Claude API key is env-only. The unused `persistent=false` branch of
`planNativeAccounts` writes no files.

`box.SignInNeedsHomes` refuses a plain agent command whose resolved posture is filtered or open. It
runs in the CLI's `runInBoxMode`, after admission, next to `refuseOfflineOnePrompt`. It is not in
`box.Run`, because box tests run agent commands with `Homes` false and open egress as an ordinary
setup, and eval graders rely on `Homes:false` meaning no model credentials. With homes off,
`box.Run` still refuses a preset, peers and a consult lead, which every fork agent run is. A plain
loop iteration has none of those and is not refused up front. An offline run starts, since it
reaches no provider.

## Changelog
- 2026-11-06 — created with `SignInNeedsHomes` and the preset, peer and lead refusals in the
  shared block; verified against credentialScope, planNativeAccounts, the four broker seeds,
  runInBoxMode, the fork run spec and the loop iteration spec.
