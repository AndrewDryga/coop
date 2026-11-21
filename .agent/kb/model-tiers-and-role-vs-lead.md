---
name: model-tiers-and-role-vs-lead
description: ModelFor picks active>target>env per PROVIDER, so nothing but the lead's rotation may write a provider's active/target tier; a role's model rides its wrapper target
subsystem: config
sources: [internal/config/config.go, internal/cli/presetflag.go, internal/cli/target.go, internal/cli/acp_network.go, internal/cli/acp_cmd.go, internal/loop/rotation.go, internal/box/run.go, internal/preset/preset.go, internal/agent/claude.go, internal/agent/agent.go, internal/agent/target.go]
updated: 2026-11-21
---

`Config.ModelFor(agent)` resolves ONE model per PROVIDER through three tiers, most specific
first (config.go): active (`SetActiveModel` — an explicit one-off target) → target
(`SetTargetModel` — the rotation's active rung, re-set every `applyTarget`) →
`COOP_<AGENT>_MODEL` env or conf (checked at `config.Load` by the target grammar via `agent.CheckModelSetting`, registered through `RegisterAdapterConfig`). `EffortFor` mirrors it. Ordered target ladders, including
controller-owned remote jobs, provide model/provider/account failover; these defaults do not.
The key consequence: **anything written to a provider's active tier shadows that provider's
rotation target.** So during a `coop loop`, only the lead's rotation may own a provider's
active/target tier — `applyTarget` (`internal/loop/rotation.go`) is the single choke point that sets it on every
rung. The lead command reads it via each adapter's `base()` (`withModel(cfg.ModelFor(...))`), and
so do the rendered model line and telemetry — they must all agree.

**The trap (fixed 2026-07-14).** A preset role's model must NOT go into global provider state.
Per-provider state can't represent one provider playing several parts at once — the frontier
preset runs codex as the thinker (terra), the fast delegate (luna), AND a lead rung (sol). A role
rides its OWN ordered `Role.Targets` in the wrapper env instead: `resolvedRoleTargetList` →
`COOP_CONSULT/DELEGATE_<ROLE>_TARGETS` (run.go), which every provider's consult/delegate arm passes
straight through as `${model:+--model "$model"}` (parsed from the target token, falling back to
`COOP_PEER_MODEL_<agent>` only for a model-less rung). `applyPreset` therefore seeds ONLY the
lead's model/credentials globally; it does not touch roles. Before the fix it called
`SetActiveModel(role.Agent, role.Model)`, so when codex later became the lead via rotation,
`ModelFor("codex")` returned the stale role model (terra) instead of the rotated rung (sol) — the
lead launched terra while the announcement and telemetry said sol.

Corollary: `spec.Peers` is populated only from `--peer`, never preset roles, so a preset run's
`COOP_PEER_MODEL_<agent>` derives from `cfg.ModelFor` purely as a fallback — a role with an
explicit model never depends on it.

An explicit peer may name the lead provider. Its `COOP_PEER_*` defaults resolve from the peer
target, while the lead's native environment and effort validation resolve from the run config.
Credential mounts remain deduplicated, but direct peer authorization retains the explicitly
named primary. One account per provider means that peer shares the selected lead account.
Rotating launch validation checks each concrete lead account on a cloned config; a peer on a
different-provider rung still needs its ordinary account. Filtered ACP admission and child bindings
resolve same-provider peers to that concrete lead account, never an unrelated supervisor default.

Native CLI acceptance is not proof an effort was applied. Captured on Claude SDK 0.3.284 and ACP 0.84.0
(the pins are now 0.3.318 and 0.101.0): the SDK reported applied effort null for Haiku 4.5, and ACP
exposed no effort option. Claude's existing EffortSpec.Validate
refuses explicit effort for the three known alias/current IDs during target parsing and resolved box
configuration validation, before a client starts. Haiku without effort remains valid; unknown future IDs stay delegated
to the client. Do not ignore an ACP setting failure to hide an impossible target. The same Validate
also refuses a level outside `claudeEfforts` (low…max): Claude Code 2.1.330 only warns about one and
runs at its default effort. Codex (0.170 prints `reasoning effort: bogus` and sends it; its catalog
adds levels such as `ultra` per model) and Grok (free-form `--reasoning-effort`) stay unchecked.

## Changelog
- 2026-11-21 — Claude refuses an effort its pinned CLI does not list (task 2026-11-21-refuse-a-reasoning-effort-the-agent-does-not-hav); checked `claude --help`, `codex debug models`, `grok --help`.
- 2026-11-21 — `COOP_<AGENT>_MODEL` is now refused at load when a target would refuse its model[/effort] (task 2026-11-21-check-coop-agent-model-like-a-target).
- 2026-11-17 — dated the client-version observations as captures and named the current pins (audit K3-5; checked the pins in internal/agent/codex.go, grok.go, claude.go).
- 2026-10-10 — real Claude same-provider peer launch omitted the consult wrapper and authorization;
  repaired that projection and kept flagless ACP lead model/effort independent of peer targets.
- 2026-10-10 — real Haiku/low preset ACP prompt failed Unknown config option: effort. Inspected
  pinned adapter/SDK and verified parser plus inherited config prelaunch refusal without changing
  supported ACP settings, model-tier ownership or unknown-model pass-through.
- 2026-09-27 — removed the never-populated standing-default maps after checking all callers.
  Preserved active/target ownership and the separate ordered target ladders used for real failover.
- 2026-09-03 — re-verified the four model tiers and role isolation while presets were normalized to
  one target-list representation. Role wrapper rendering now reads `Role.Targets` directly; the
  active-target ownership rule is unchanged.
- 2026-07-14 — created after fixing preset role models shadowing a rotated cross-provider lead (task 2026-07-14-keep-preset-role-models-from-overriding-rotated); verified against ModelFor tiers, applyPreset, applyTarget, and the wrapper arms.
- 2026-08-10 — sources repointed: the loop engine moved out of `internal/cli` into `internal/loop`: `applyTarget`/`rotateOnLimit` are now
  `internal/loop/rotation.go` (`internal/cli/rotation.go` keeps ladder EXPANSION — `expandLadder`,
  `buildRotation`); `applyPreset` stays in `internal/cli/presetflag.go`. Choke point unchanged.
