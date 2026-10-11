---
name: native-broker-account-discovery
description: Guard-selected public account aliases must match native discovery without changing workspace routing
subsystem: box
sources: [internal/agent/codex.go, internal/agent/native_credentials.go, internal/box/native_broker.go, internal/networkgateway/native_broker.go, internal/networkgateway/native_account_response.go, internal/networkgateway/native_account_response_test.go]
updated: 2026-10-11
---

The pinned native client compares its public auth account with upstream discovery. A stand-in
auth file alone is insufficient: discovery still contains the private account ID, so startup
reports a missing selected workspace. The adapter declares one exact authenticated GET plus a
typed set of literal identity/collection keys; the private guard config binds real and public
account headers. The gateway copies that declaration and aliases only the uniquely selected
identity. It never chooses the upstream default, fabricates unrestricted routing or imports an
adapter into the trusted helper.

Array and ordered-map documents preserve sibling accounts and unknown metadata. Map normalization
skips absent ordering references and deduplicates keys before projecting. Duplicate JSON keys,
ambiguous selection, alias collision, oversize/deep documents and unsupported encodings refuse
before body forwarding. Projected responses have corrected framing, no stale integrity headers
and `Cache-Control: no-store`; genuine upstream authentication errors pass through unchanged.

## Changelog
- 2026-10-11 — reproduced the pinned client bootstrap failure, verified native array/map contracts,
  and tested exact CONNECT mediation, alternate adapter keys, routing preservation and denial paths.
