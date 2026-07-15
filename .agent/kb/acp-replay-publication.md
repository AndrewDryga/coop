---
name: acp-replay-publication
description: ACP replacement sessions become authoritative as one generation before held editor work is released
subsystem: acp
sources: [internal/acpproxy/proxy.go, internal/acpproxy/proxy_test.go]
updated: 2026-07-15
---

The editor session id is durable; each provider-owned native id is generation-scoped. Replay sorts
the durable sessions, negotiates all load/new results off to the side, rejects duplicate native
bindings, then swaps the complete binding set atomically (`internal/acpproxy/proxy.go:1744`,
`internal/acpproxy/proxy.go:1776`, `internal/acpproxy/proxy.go:2064`,
`internal/acpproxy/proxy.go:2246`). A provider switch therefore
uses direct `session/new`, never probes a foreign id, and only successful bindings publish.

Prompts arriving after a switch acknowledgement wait behind `restarting` until settings, recreation
hooks, and config updates are complete; the final release revalidates each session
(`internal/acpproxy/proxy.go:2185`). Close/delete during either candidate replay or the post-swap tail
retires local state and sends a generation-checked lifecycle cleanup, so a late native update cannot
resurrect the editor thread (`internal/acpproxy/proxy.go:845`).

## Changelog
- 2026-07-15 - created from the replay-publication and lifecycle portions of acp-scripted-e2e
