---
name: acp-target-commit
description: ACP model and effort choices become controller truth only from the effective provider response
subsystem: acp
sources: [internal/acpctl/control.go, internal/acpctl/control_test.go, internal/cli/fork_acp.go, internal/cli/fork_acp_test.go, internal/agent/grok.go, internal/acpproxy/proxy.go, internal/acpproxy/scripted_matrix_e2e_test.go]
updated: 2026-11-06
---

Native and synthesized target requests carry a monotonic provider/field sequence. A provider success
commits the preference; rejection restores the newest accepted value. The effective response is the
newest settled choice, including the reverse ordering where a newer request rejects before an older
one succeeds (`registerNativeTarget` numbers each request and `commitNativeTarget` settles its
response, both in internal/acpctl/control.go). Local proxy rejections also pass through this
response path, so a reused JSON-RPC id cannot commit stale target intent. The local writable
`coop fork <name> acp <target>` supervisor observes native model/effort config-option requests and
native `session/set_model` requests, plus their adapter responses. Accepted choices feed the same
target commit path before `SessionReady` reapplies them after a child restart; rejected choices do
not change the target. An adapter request with the same numeric ID in the opposite RPC direction is
not an acknowledgment. The supervisor leaves native toolbar frames unchanged and never enables Coop
provider/account/preset selection.

For a translated `session/set_model`, only the exact structured Grok pair
`MODEL_SWITCH_INCOMPATIBLE_AGENT` plus `start_new_session` is an accepted launch-target migration.
It forces every active session through fresh `session/new`, arms in-flight resends, and persists the
recreation intent across SIGHUP; prose, near misses, stale responses, and other provider errors remain
errors (`modelRestartSuggested`, called from `commitNativeTarget`, and `setModelFromEditor` in
internal/acpctl/control.go).

The editor's session/new|load|resume answer is forwarded before `SessionReady`'s settings go out, so
it carries the adapter's defaults. ACP has an adapter answer `session/set_config_option` with its
complete option list, and when the last setting succeeds the proxy sends that list to the editor as
a `config_option_update` through `ToEditor`, before a prompt held behind the settings reaches the box
(`handleInjectedResponseControlled` and `settledConfigLine` in internal/acpproxy/proxy.go). An answer
without options, such as `session/set_model`'s `{}`, sends nothing. The fork supervisor passes it on
unchanged, so a fork's editor also sees the mode Coop sets. The older `models` field, which ACP
dropped in 0.13.5 but codex-acp 2 still sends, changes only in a session answer, so `retargetModels`
points its `currentModelId` at the model `SessionReady` applies; codex-acp ids name the effort too
(`gpt-6-luna[high]`). `TestScriptedACPEditorSeesTheTargetTheBoxRuns` compares what the editor shows
with what the box got.

An editor that ignores `config_option_update` would still show the adapter's `reasoning_effort=low`
or `effort=default` from the session/new answer, so a plain session's answer is also pointed at the
settings: `retargetSettings` (internal/acpctl/control.go) gives each effort option the target's
value when it lists it, as `rewriteConfigOptions` does for the model. Only a result with a
`sessionId` is touched. A notification or an option answer already holds the adapter's current
values, and the target would lag the choice that produced them. A fork's native frames pass
unchanged, so a fork's editor learns the effort from the update alone.
`TestACPControlSessionAnswerShowsTheTargetEffort` covers it.

Live conformance changes a model only when the installed adapter advertises a distinct option. Grok
0.2.101 can truthfully expose only its current agent model. Coop must not merge the illustrative
`Agent.Models()` list into native ACP choices: scripted E2E owns the cross-agent migration contract,
while live E2E still requires a nonempty current model, reload, and a second successful prompt.

## Changelog
- 2026-11-06 — a plain session's session/new answer now shows the target's effort too
  (`retargetSettings`), so an editor that ignores the settled update does not show the adapter's
  default; verified against each agent's `ACPSessionSettings` and the option ids the 2026-11-06
  re-test saw in session/new answers (`reasoning_effort`, `effort`).
- 2026-11-06 — the editor now gets the adapter's options after Coop's settings, and the legacy
  `models.currentModelId` names the target; verified against codex-acp 2.0.1's `ModelId` and
  `setSessionConfigOption`, and the ACP config-options spec.
- 2026-10-17 — named `registerNativeTarget`, `commitNativeTarget`, `modelRestartSuggested` and
  `setModelFromEditor` instead of control.go line numbers; re-verified each against
  internal/acpctl/control.go.
- 2026-09-29 - rechecked the fixed-fork supervisor and native target response path; covered
  accepted/rejected config-option and set-model choices, same-ID opposite-direction requests, and
  restart behavior without widening fixed fork selection.
- 2026-08-10 - re-verified after the ACP control plane moved to internal/acpctl (acpcontrol.go →
  control.go, mechanical rename only); sources and line citations updated to the new paths/numbers
- 2026-07-16 - made live switching capability-aware after Grok exposed one native model option
- 2026-07-15 - created after live Grok exposed response-gating and cross-agent migration behavior
