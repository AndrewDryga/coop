---
name: acp-target-commit
description: ACP model and effort choices become controller truth only from the effective provider response
subsystem: acp
sources: [internal/cli/acpcontrol.go, internal/cli/acpcontrol_test.go, internal/agent/grok.go]
updated: 2026-07-15
---

Native and synthesized target requests carry a monotonic provider/field sequence. A provider success
commits the preference; rejection restores the newest accepted value. The effective response is the
newest settled choice, including the reverse ordering where a newer request rejects before an older
one succeeds (`internal/cli/acpcontrol.go:755`). Local proxy rejections also pass through this response
path, so a reused JSON-RPC id cannot commit stale target intent.

For a translated `session/set_model`, only the exact structured Grok pair
`MODEL_SWITCH_INCOMPATIBLE_AGENT` plus `start_new_session` is an accepted launch-target migration.
It forces every active session through fresh `session/new`, arms in-flight resends, and persists the
recreation intent across SIGHUP; prose, near misses, stale responses, and other provider errors remain
errors (`internal/cli/acpcontrol.go:777`, `internal/cli/acpcontrol.go:2736`).

## Changelog
- 2026-07-15 - created after live Grok exposed response-gating and cross-agent migration behavior
