---
name: acp-target-commit
description: ACP model and effort choices become controller truth only from the effective provider response
subsystem: acp
sources: [internal/acpctl/control.go, internal/acpctl/control_test.go, internal/cli/fork_acp.go, internal/cli/fork_acp_test.go, internal/agent/grok.go]
updated: 2026-09-29
---

Native and synthesized target requests carry a monotonic provider/field sequence. A provider success
commits the preference; rejection restores the newest accepted value. The effective response is the
newest settled choice, including the reverse ordering where a newer request rejects before an older
one succeeds (`internal/acpctl/control.go:747`). Local proxy rejections also pass through this response
path, so a reused JSON-RPC id cannot commit stale target intent.
The local writable `coop fork <name> acp <target>` supervisor observes native model/effort
config-option requests and native `session/set_model` requests, plus their adapter responses.
Accepted choices feed the same target commit path before
`SessionReady` reapplies them after a child restart; rejected choices do not change the target.
An adapter request with the same numeric ID in the opposite RPC direction is not an acknowledgment.
The supervisor leaves native toolbar frames unchanged and never enables Coop provider/account/preset selection.

For a translated `session/set_model`, only the exact structured Grok pair
`MODEL_SWITCH_INCOMPATIBLE_AGENT` plus `start_new_session` is an accepted launch-target migration.
It forces every active session through fresh `session/new`, arms in-flight resends, and persists the
recreation intent across SIGHUP; prose, near misses, stale responses, and other provider errors remain
errors (`internal/acpctl/control.go:771`, `internal/acpctl/control.go:2744`).

Live conformance changes a model only when the installed adapter advertises a distinct option. Grok
0.2.101 can truthfully expose only its current agent model. Coop must not merge the illustrative
`Agent.Models()` list into native ACP choices: scripted E2E owns the cross-agent migration contract,
while live E2E still requires a nonempty current model, reload, and a second successful prompt.

## Changelog
- 2026-09-29 - rechecked the fixed-fork supervisor and native target response path; covered
  accepted/rejected config-option and set-model choices, same-ID opposite-direction requests, and
  restart behavior without widening fixed fork selection.
- 2026-08-10 - re-verified after the ACP control plane moved to internal/acpctl (acpcontrol.go →
  control.go, mechanical rename only); sources and line citations updated to the new paths/numbers
- 2026-07-16 - made live switching capability-aware after Grok exposed one native model option
- 2026-07-15 - created after live Grok exposed response-gating and cross-agent migration behavior
