---
name: model-tiers-and-role-vs-lead
description: ModelFor picks active>target>fallback>env per PROVIDER, so nothing but the lead's rotation may write a provider's active/target tier; a role's model rides its wrapper target
subsystem: config
sources: [internal/config/config.go, internal/cli/presetflag.go, internal/cli/rotation.go, internal/box/run.go]
updated: 2026-07-14
---

`Config.ModelFor(agent)` resolves ONE model per PROVIDER through four tiers, most specific
first (config.go): active (`SetActiveModel` — an explicit `--model`) → target
(`SetTargetModel` — the rotation's active rung, re-set every `applyTarget`) → fallback
(`SetFallbackModel` — a standing default) → `COOP_<AGENT>_MODEL` env. `EffortFor` mirrors it.
The key consequence: **anything written to a provider's active tier shadows that provider's
rotation target.** So during a `coop loop`, only the lead's rotation may own a provider's
active/target tier — `applyTarget` (rotation.go) is the single choke point that sets it on every
rung. The lead command reads it via each adapter's `base()` (`withModel(cfg.ModelFor(...))`), and
so do the rendered model line and telemetry — they must all agree.

**The trap (fixed 2026-07-14).** A preset role's model must NOT go into global provider state.
Per-provider state can't represent one provider playing several parts at once — the frontier
preset runs codex as the thinker (terra), the fast delegate (luna), AND a lead rung (sol). A role
rides its OWN target in the wrapper env instead: `Role.TargetList()` →
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

## Changelog
- 2026-07-14 — created after fixing preset role models shadowing a rotated cross-provider lead (task 2026-07-14-keep-preset-role-models-from-overriding-rotated); verified against ModelFor tiers, applyPreset, applyTarget, and the wrapper arms.
