---
name: provider-bundles-carry-function-not-chatter
description: a provider bundle grants what the client needs to function; the client's own update/telemetry chatter is switched off in the box, never granted and never hidden
scope: security
sources: [internal/agent/network_bundle.go, internal/agent/claude.go, internal/agent/codex.go, internal/agent/gemini.go, internal/agent/grok.go, internal/agent/locked_clients_test.go, internal/mcp/mcp.go, internal/cli/provider_network_live_e2e_test.go]
check: "go test ./internal/agent -run 'TestProviderBundlesCarryFunctionNotChatter|TestManagedClientDefaultsAreBoxOnly'"
updated: 2026-09-14
---

# A provider bundle carries function; a managed client's chatter is switched off, not granted or hidden

A core provider bundle holds exactly the endpoints the selected login needs to WORK: the model
API, the token endpoint, and a capability the login turns on by default (Claude's connector proxy
`mcp-proxy.anthropic.com`). It never holds the client's own release feed, package registry,
installer or telemetry intake (`raw.githubusercontent.com`, `registry.npmjs.org`, `api.github.com`,
Datadog, Sentry, `ab.chatgpt.com`). That traffic is stopped at its source with the client's
documented controls, projected box-only — never written to a host settings file — and a request
that still arrives is a real refusal: recorded, visible in `coop net inspect`, explainable, and
eligible for the ordinary `box.egress_rules` + `coop approve` path. Coop keeps no hostname
list that suppresses a denial.

**Why:** the first filtered `coop claude` hello-and-exit run (task
`2026-09-10-make-network-inspection-destination-first-and-ex`) retained DNS refusals for
`mcp-proxy.anthropic.com`, `raw.githubusercontent.com`, `registry.npmjs.org` and
`http-intake.logs.us5.datadoghq.com`, and the inspect view offered to add each to
`box.egress_rules`. The two easy fixes were both wrong: granting all four to silence the run would
have handed every session a host that delivers arbitrary repository content and arbitrary packages,
and filtering the four names out of the report would have hidden a later, deliberate `curl` to the
same hosts. The proxy was the one genuine provider capability; the other three were the client
talking to itself.

**How to apply:**
- Adding a host to a bundle needs an answer to "what does the login fail to DO without it?". A
  feature that is on by default for that login qualifies (the connector proxy); "the client asks
  for it" does not. Bump `NetworkBundleVersion` with the content — the store pins each version's
  bytes on first admission (`networkstate.checkBundles`), so the same version with new content is
  refused as integrity drift.
- Stop chatter with the client's own controls, read out of the LOCKED version (strings in the
  binary, then the vendor docs), and project them box-only: Claude through `BoxEnv`
  (`CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`, `DISABLE_UPDATES=1`), codex through the generated
  `config.toml` overlay (`check_for_update_on_startup`, `analytics.enabled`, the three `otel.*`
  exporters), gemini through the generated `settings.json` (`general.enableAutoUpdate`,
  `general.enableAutoUpdateNotification`, `privacy.usageStatisticsEnabled`) plus
  `GEMINI_TELEMETRY_ENABLED=false`. `EnsureDefaults` writes the host profile for first-run prompts
  only; a managed control there would be a host edit the user never asked for.
- Prove the control keeps the login functional on that exact version: inference, the OAuth
  refresh and the default-on capability (Claude's connector eligibility never consults the traffic
  mode). The live probe then pins the silence: no retained refusal of a chatter host after a
  hello-and-exit session (`verifyProviderNetworkLiveSilence`).
- Never add a denial filter keyed by hostname, and never present a box default as qualification:
  gemini has these defaults and is still unsupported for filtered runs.

Background: [[restricted-networking]] (bundles are one of five layers),
[[mcp-authority-projection]] (the overlays are projections of the host profile, never edits).

## Changelog
- 2026-09-14 — observed Grok's device login request to `auth.x.ai` denied by the filtered gateway,
  added that required token endpoint under a new bundle version, and pinned Grok's exact core set.
- 2026-09-13 — updated the current approval path to the top-level `coop approve` command.
- 2026-09-10 — created with the fix. Swept every `NetworkBundle` (claude, codex; gemini/grok
  refuse) against the chatter list: 0 violations after adding the connector proxy under
  `2026-09-10.1`. The `check:` pins bundle membership, the four upstream control names and that the
  host files stay unwritten; the live silence probe rides `providerlivee2e`.
