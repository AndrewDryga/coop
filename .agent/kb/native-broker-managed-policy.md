---
name: native-broker-managed-policy
description: Claude policy retrieval needs its exact native settings route and must retain genuine upstream policy and errors
subsystem: box
sources: [internal/agent/claude.go, internal/agent/claude_managed_settings_test.go, internal/networkgateway/native_settings_response_test.go]
updated: 2026-10-11
---

The pinned Claude CLI fetches `GET /api/claude_code/settings` during ordinary startup. Missing
admission produces a broker403 that the client describes as access-denied managed settings,
even when inference works. OAuth and API-key adapter families both declare the exact request
with their existing authentication header. Inference, policy-limit and MCP routes are separate.

The broker passes genuine policy bodies, authentication errors, absent-policy statuses and
cache/signature headers unchanged. Never synthesize empty policy or hide403 to clear a warning.
Real startup plus multiple local-tool turns verifies the user-visible behavior; normal exit0
is not a crash. A simultaneous native-home writer can invalidate the independent startup digest
witness; that is not evidence that policy retrieval failed.

## Changelog
- 2026-10-11 — pinned offline and real CLI probes isolated the missing route; exact path/query,
  both auth-family and CONNECT/HTTP2 policy/status/header regressions pass.
