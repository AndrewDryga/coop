---
name: native-broker-managed-policy
description: Claude policy retrieval needs its exact native settings route and must retain genuine upstream policy and errors
subsystem: box
sources: [internal/agent/claude.go, internal/agent/claude_managed_settings_test.go, internal/agent/claude_policy_limits_test.go, internal/networkgateway/native_settings_response_test.go]
updated: 2026-10-11
---

The pinned Claude CLI fetches `GET /api/claude_code/settings` during ordinary startup. Missing
admission produces a broker403 that the client describes as access-denied managed settings,
even when inference works. OAuth and API-key adapter families both declare the exact request
with their existing authentication header. The same pinned client separately fetches exact
`GET /api/claude_code/policy_limits` for both families; admitting it only for OAuth would silently
replace an API-key organization-policy lookup with a local broker403. Its API-key declaration
must use X-Api-Key, not the OAuth Bearer header. Inference and MCP routes remain separate.

The broker passes genuine policy bodies, authentication errors, absent-policy statuses and
cache/signature headers unchanged. Never synthesize empty policy or hide403 to clear a warning.
Real startup plus multiple local-tool turns verifies the user-visible behavior; normal exit0
is not a crash. A simultaneous native-home writer can invalidate the independent startup digest
witness; that is not evidence that policy retrieval failed.

## Changelog
- 2026-10-11 — additional public-marker/API-key native startup proved the separate policy-limit
  request. Added its missing exact API-key declaration; adapter regression fails before repair,
  and both policy paths retain CONNECT/HTTP2 status/body/cache/signature and denial coverage.
- 2026-10-11 — pinned offline and real CLI probes isolated the missing route; exact path/query,
  both auth-family and CONNECT/HTTP2 policy/status/header regressions pass.
