---
name: acp-warm-pool-serves-only-bare-targets
description: coop acp's warm pool parks a box per other signed-in provider but serves only a bare Target{Provider}; every Provider-selector switch carries account/model/effort, so on a signed-in host it never hits — prove a hit from the trace, never from timing
subsystem: acp
sources: [internal/acpctl/warm.go, internal/acpctl/resume.go, internal/acpctl/control.go, internal/cli/acp_cmd.go, internal/acpproxy/proxy.go, tools/lifecycle_bench.py]
updated: 2026-09-19
---
`coop acp` fans out one parked box per other signed-in provider at launch (`acpctl.WarmPool`, filled
sequentially), and its factory checks one out only when `acpctl.BareProviderSwitch` holds: a target
with no model, effort or account. `Control.SpawnTarget` never produces that for a Provider-selector
switch: it resolves Auto to the provider's first signed-in account, and the selector offers — and the
pool warms — only providers that have one (model and effort add to it only when configured). So on a
host with accounts every editor switch starts a cold box while the parked ones idle. The one hit left
is a same-provider respawn (a crashed or reloaded box) of a lead with no signed-in account at all. Measured 2026-09-19 (task
2026-09-18-measure-editor-warm-hits-misses-and-provider-swi): the pool readied codex, the switch
to codex still traced `spawn: cold box`. Making the pool serve those targets is the job of task
2026-09-15-reuse-existing-warm-agents-for-matching-editor-t.

**Evidence, not timing.** With `COOP_ACP_TRACE=1` the trace says which box served each spawn:
`spawn: warm box for P@A` (a pool checkout) or `spawn: cold box for P@A` (the factory started one),
`warm pool: P@A ready` when a parked box is stored, and `replay: P@A is live on <adapter> <version>`
once the replayed session runs on it. `tools/lifecycle_bench.py --cases acp_switch_cold,acp_switch_warm`
times a switch from `session/set_config_option` to the replayed `config_option_update` that names the
new provider, and counts a sample only when the trace's box kind is the one the case measures — a
second switch is not a warm one just because it came second. The bench parses these lines, so change
them together.

## Changelog
- 2026-09-19 — created: the pool's bare-target limit, the trace evidence lines, and the bench cases
