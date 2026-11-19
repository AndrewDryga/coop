---
name: native-broker-dropped-queries
description: A broker route can admit a query parameter and not forward it; only a filtered run's broker drops it, since open runs share the same broker
subsystem: box
sources: [internal/agent/codex.go, internal/agent/native_credentials.go, internal/box/native_broker.go, internal/networkgateway/native_broker.go, internal/networkgateway/native_client_requests_test.go]
updated: 2026-11-19
---

Codex 0.166.0 reads `/backend-api/ps/plugins/installed?...&includeDownloadUrls=true` at every
start, then downloads each account plugin from `sdmntpr*.oaiusercontent.com`, ChatGPT's store for
every user's uploads. A filtered box must never allow those hosts, so it blocked 8 TLS attempts
per run. A `NativeBrokerQuery` marked `Dropped` is still checked like any other parameter, then
removed from the forwarded query: the provider answers without download links and the client logs
"skipping remote installed plugin bundle download" (`remote_installed_plugin_sync.rs`), the same
outcome as a blocked download without the network attempts. Every other parameter goes upstream
byte for byte, because re-encoding would rewrite the provider's opaque `pageToken`.

The native broker serves open runs too (`startNativeOpen`), so the adapter's `Dropped` flag alone
would also stop downloads an open box can make. `nativeRun.filtered` gates it when the box writes
the guard's plan. Prefer a dropped request parameter over rewriting a response body: no buffering,
no shape to fail on.

## Changelog
- 2026-11-19 — added with the Codex installed-plugin change; live filtered run went from 8 denied
  TLS connections to 0 with the same answer (task 2026-11-19-find-what-codex-s-blocked-oaiusercontent-com-req).
