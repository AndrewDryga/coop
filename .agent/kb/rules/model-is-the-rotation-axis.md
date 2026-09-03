---
name: model-is-the-rotation-axis
description: "every rotation and fallback surface uses the same target grammar; accounts are part of a target, never a second axis"
scope: architecture
sources: [internal/agent/target.go, internal/cli/target.go, internal/cli/rotation.go, internal/cli/presetflag.go, internal/config/config.go, internal/preset/preset.go, internal/loopcfg/loopcfg.go, internal/sessionsvc/service.go]
check: "none"
updated: 2026-09-03
---

# Use targets as the one rotation and fallback grammar

Every surface names who runs as a target: `provider[:model][/effort][@account]`. An ordered
fallback is a ladder of those same targets. Do not introduce a separate credential list, model
list, or surface-specific target shape.

- A preset lead stores one non-empty `LeadTargets` ladder. On a rotating surface, a bare target
  fans out over every runnable account for its provider; a non-rotating launch uses the provider's
  default account. An account-pinned target selects exact accounts.
- A native role has one target because the provider's native subagent hook cannot fail over.
  Consult and delegate roles may have ordered target ladders. Roles never pin accounts; each rung
  uses that provider's default account.
- A direct agent, fork, or ACP launch accepts one positional target or preset. A preset contributes
  its full lead ladder and roles; there is no fork- or editor-specific target schema.
- Loop orchestration has preflight, work, between, signoff, and verify stages. `work.agent` accepts
  target-or-preset ladders; `between.agent`, `signoff.agent`, and `verify.agent` select target
  ladders for their review passes. Preflight intentionally uses the work rotation and has no
  separate `agent:` field.
- Coop's launch grammar is positional (`coop claude:opus@work`), not Coop-level `--model` or
  `--credential` flags. Provider adapters may still emit their provider CLI's native model flag;
  that is an implementation detail, not another Coop grammar.
- There is no persistent rotation pool. On rotating surfaces, a bare target expresses “all
  runnable accounts,” and ladder order expresses fallback.

The remote-session policy is the deliberate spelling exception: its deployed YAML key remains
`target:`, accepting either one target or a list. It uses the same target parser and ladder meaning;
do not rename it to `agent:` or add a parallel `models:` field.

**Why:** separate model, credential, and pool axes created overlapping ways to select the same
execution identity. One target grammar keeps parsing, logging, failover, and policy review aligned.
The previous card later drifted in the opposite direction: it described scalar roles, retired loop
stages, and nonexistent flag tombstones, putting current fallback and provider passthrough at risk.

**How to apply:**
- New fallback configuration stores an ordered target list and uses `agent.ParseTarget`.
- Preserve bare-target account fan-out when constructing a rotation, default-account selection for
  non-rotating launches, and first-seen ladder order.
- Resolve model/effort from most specific to least: explicit one-off target, selected ladder target
  (including a preset lead), the optional internal standing fallback, `COOP_<AGENT>_MODEL`, then the
  provider default. The standing fallback's setters have no production callers; only tests exercise
  this dormant tier. It is not preset behavior or public grammar, and new selection policy must not
  depend on it.
- Keep provider credentials in the host vault. Repository configuration may name an account but
  never contain its login material.

Related: [[loop-failover-profiles]] and [[credentials-not-profiles]].

## Changelog
- 2026-09-03 — re-verified preset, loop, direct-launch, and session-policy grammar after target
  normalization. Corrected the current role ladders and five loop stages, removed nonexistent
  tombstone claims, and explicitly preserved provider-native model flags.
- 2026-08-25 — direct fork launches adopted the shared positional target-or-preset grammar.
- 2026-08-10 — fork orchestration implementation moved into `internal/forkctl`.
- 2026-08-09 — moved the session service to `internal/sessionsvc` and added source metadata.
- 2026-08-07 — session policies gained target ladders while retaining the deployed `target:` key.
- 2026-07-03 — created; revised 2026-07-11.
