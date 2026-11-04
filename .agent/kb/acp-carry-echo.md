---
name: acp-carry-echo
description: Best-effort cross-provider context is injected once, and hidden from the editor both in its live echo (exact text) and in a later replay (fixed format)
subsystem: acp
sources: [internal/acpctl/control.go, internal/acpctl/replay.go, internal/acpctl/control_test.go, internal/acpctl/carry_queue_test.go, internal/acpctl/replay_test.go, internal/acpproxy/scripted_matrix_e2e_test.go, internal/acpproxy/scripted_reopen_e2e_test.go, internal/acpproxy/e2e_test.go]
updated: 2026-11-04
---

`SessionRecreated` arms one budgeted plain-text history block for the next admitted prompt; tool
payloads/results stay out, while message text and terminal tool narration carry. The controller's
flag is consumed only when the next prompt enters the ordinary admission path, so superseded
replacements do not lose context (`internal/acpctl/control.go`).

Adapters expose submitted prompts through three observed paths: ACP `user_message_chunk` content,
Codex `session_info_update.title`, and Grok `_x.ai/queue/changed` prompt entries plus `runningText`. Coop records the
exact injected first block at admission and removes only that block. Codex collapses title whitespace,
so its path matches the complete whitespace-canonical form; message and Grok queue content remain
byte-exact. Surrounding real text, sibling queue entries, and unknown JSON fields survive. Empty
queue updates still pass through to clear prior queue state and preserve running/version metadata.

One turn can emit several echo forms. The filter therefore stays armed until the correlated terminal
response, with cleanup before authentication/rate-limit recovery can consume that correlation, and
on restart/session teardown. Unit coverage exercises both echo orders, canonical titles, queue
metadata, near misses, and terminal cleanup. The strict live ACP suite proves provider switches to
both Codex and Grok retain carried context without exposing the `[coop]` block to the editor.

The transcript keeps the block, so a `session/load` gives it back as the person's first message, and the
coop acp doing the load may never have seen it. Between the editor's load or resume and its answer,
`filterReplay` (`internal/acpctl/replay.go`) reads `user_message_chunk`s against the fixed format the builder
writes: chunks wait until the end marker arrives, or until something rules a block out, which includes the
message ending (an agent chunk, a tool call, the answer) and a size limit. Only the exact format at the start of
a message goes. So that the first end marker is the real one, the builder rewrites a quoted marker in the history.
Claude replays the block and the typed text as separate chunks; Codex-style adapters may join them, with or
without line breaks, which is the `---hi` the owner saw.

The thread's start message follows that same answer, so it cannot land inside a replayed message. A load answer
can name the native session id, so the message goes to the id the editor asked about, not the answer's.

## Changelog
- 2026-11-04 - a reopened thread showed the block as the person's words, since the exact-text filter lives in memory for one turn; added the format-based replay filter and moved the start message for a reopen after the answer
- 2026-09-12 - strict live switching into Grok exposed an additional runningText echo; added exact armed-block filtering and empty-queue/metadata/terminal regressions, preserving the strict live visibility assertion
- 2026-08-10 - re-verified after the ACP control plane moved to internal/acpctl (acpcontrol.go →
  control.go, mechanical rename only); sources and line citations updated to the new paths/numbers
- 2026-07-16 - covered Codex canonical titles and Grok prompt-queue echoes after strict live switching exposed both wire shapes
- 2026-07-15 - created after reproducing Grok's synthetic carried-context user bubble
