---
name: acp-carry-echo
description: Best-effort cross-provider context is injected once and its exact provider echo is hidden from the editor
subsystem: acp
sources: [internal/cli/acpcontrol.go, internal/cli/acpcontrol_test.go, internal/acpproxy/scripted_matrix_e2e_test.go]
updated: 2026-07-15
---

`SessionRecreated` arms one budgeted plain-text history block for the next admitted prompt; tool
payloads/results stay out, while message text and terminal tool narration carry
(`internal/cli/acpcontrol.go:284`, `internal/cli/acpcontrol.go:1523`,
`internal/cli/acpcontrol.go:1582`). The flag is consumed only when the next prompt enters the ordinary
admission path, so superseded replacements do not lose context (`internal/cli/acpcontrol.go:2540`).

Some adapters echo submitted prompt blocks as `user_message_chunk`. Coop records the exact injected
first block at admission and removes only that exact substring from the echo, preserving any real user
text around it (`internal/cli/acpcontrol.go:1432`, `internal/cli/acpcontrol.go:1460`). The two-session Grok process case emits this echo
while migrating and asserts that no `[coop]` block reaches the editor
(`internal/acpproxy/scripted_matrix_e2e_test.go:293`).

## Changelog
- 2026-07-15 - created after reproducing Grok's synthetic carried-context user bubble
