---
name: acp-carry-echo
description: Best-effort cross-provider context is injected once and its exact provider echo is hidden from the editor
subsystem: acp
sources: [internal/cli/acpcontrol.go, internal/cli/acpcontrol_test.go, internal/acpproxy/scripted_matrix_e2e_test.go, internal/acpproxy/e2e_test.go]
updated: 2026-07-16
---

`SessionRecreated` arms one budgeted plain-text history block for the next admitted prompt; tool
payloads/results stay out, while message text and terminal tool narration carry
(`internal/cli/acpcontrol.go:284`, `internal/cli/acpcontrol.go:1523`,
`internal/cli/acpcontrol.go:1582`). The flag is consumed only when the next prompt enters the ordinary
admission path, so superseded replacements do not lose context (`internal/cli/acpcontrol.go:2540`).

Adapters expose submitted prompts through three observed paths: ACP `user_message_chunk` content,
Codex `session_info_update.title`, and Grok `_x.ai/queue/changed` prompt entries. Coop records the
exact injected first block at admission and removes only that block. Codex collapses title whitespace,
so its path matches the complete whitespace-canonical form; message and Grok queue content remain
byte-exact. Surrounding real text, sibling queue entries, and unknown JSON fields survive.

One turn can emit several echo forms. The filter therefore stays armed until the correlated terminal
response, with cleanup before authentication/rate-limit recovery can consume that correlation, and
on restart/session teardown. Unit coverage exercises both echo orders, canonical titles, queue
metadata, near misses, and terminal cleanup. The strict live ACP suite proves provider switches to
both Codex and Grok retain carried context without exposing the `[coop]` block to the editor.

## Changelog
- 2026-07-16 - covered Codex canonical titles and Grok prompt-queue echoes after strict live switching exposed both wire shapes
- 2026-07-15 - created after reproducing Grok's synthetic carried-context user bubble
