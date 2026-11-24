---
name: native-broker-marketplace
description: Claude's cold interactive official-marketplace bootstrap uses pointer-gated, bounded public routes on downloads.claude.ai while preserving ordinary policy on every other request
subsystem: box
sources: [internal/agent/claude.go, internal/box/native_broker.go, internal/networkgateway/native_broker.go, internal/networkgateway/native_marketplace_test.go, docs/networking.md]
updated: 2026-11-23
---

On a cold interactive launch (`!isNonInteractiveSession` and before `officialMarketplaceAutoInstalled`
is recorded in `.claude.json`), Claude Code's `official-marketplace-check` fetches
`GET https://downloads.claude.ai/claude-code-releases/plugins/claude-plugins-official/latest`
followed by `GET https://downloads.claude.ai/claude-code-releases/plugins/claude-plugins-official/<sha1>.zip`,
and falls back to `https://github.com/anthropics/claude-plugins-official.git` only when that CDN
fetch fails. Prompt-only (`-p`) runs and warm Claude homes skip the check, so qualifying this path requires a
cold interactive PTY launch.

`claudeBrokerRoutes` in `internal/agent/claude.go` declares those two public routes for both
`claude-oauth` and `apikey` without adding `downloads.claude.ai` or `github.com` to `NetworkBundle`
(see [[restricted-networking]]). In `NativeBroker`, `SetsPointer` validates a bounded `200 OK` body
as a 40-character lowercase hex SHA-1 before `UsesPointer` admits that exact `<sha1>.zip` archive
within a streaming byte cap and a two-minute transfer deadline. These routes accept bodyless
requests and send broker-owned headers. The broker refuses credentials, request trailers, and
upstream redirects. Bounded upstream HTTP errors pass through.

Because every route on `downloads.claude.ai` sets `RejectCredentials`, `nativeBrokerOrigin.publicOnly`
keeps ordinary network decisions intact: open runs (`nativeRun.origins`) and filtered runs whose
policy already grants `downloads.claude.ai:443` pass `CONNECT` straight through `nativeOrdinaryProxy`,
and unapproved requests on `downloads.claude.ai` emit `tls_denied` (`unapproved_name`) so candidate
rule drafts and visible denials match any other unapproved host.

## Changelog
- 2026-11-23 — created after qualifying pinned Claude Code 2.1.330 cold interactive startup and
  implementing pointer-validated public download routes (task
  2026-11-23-keep-claude-marketplace-bootstrap-quiet-in-filte).
