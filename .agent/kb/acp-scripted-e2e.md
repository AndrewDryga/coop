---
name: acp-scripted-e2e
description: ACP can be tested through the real supervisor and box command path with a scripted COOP_RUNTIME, without Zed, Docker, or credentials
subsystem: acp
sources: [internal/acpproxy/scripted_e2e_test.go, internal/acpproxy/testdata/acpfixture/main.go, internal/acpproxy/proxy.go, internal/cli/acpcontrol.go, internal/runtime/runtime.go]
updated: 2026-07-14
---

`COOP_RUNTIME` is the production seam for deterministic ACP process tests. The fixture answers the
runtime probes, then executes a scripted provider on the inherited stdio when Coop invokes `run`.
That keeps the built outer supervisor, inner re-exec, box argument assembly, ACP controller, and
proxy on the real path while isolating HOME, config, repo state, and provider transcripts.

Use `make acp-scripted-e2e` for the deterministic process test included by `make check`. Use
`make acp-e2e` only for the opt-in real-adapter conformance layer; it builds a temporary Coop binary
and must never install over the user's binary or infer ownership by diffing global containers.

Provider-switch carry is asserted from both sides of this harness. The editor drives two real
`coop_provider` config changes and waits for replayed `config_option_update`; provider transcripts
prove each raw user/assistant turn enters the next fresh session once, while the editor transcript
proves Coop's synthetic `[coop]` preamble never leaks back as a `session/update`. Synthetic resends
share proxy remapping and pending bookkeeping but bypass the editor-origin hook, including when held
behind the target-settings gate.

## Changelog
- 2026-07-14 - added the two-switch carry contract and editor/adapter transcript assertions
- 2026-07-14 - created after replacing manual Zed reproduction with the scripted runtime driver
