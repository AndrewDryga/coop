---
name: acp-empty-thread-reopen
description: Start a fresh native session when reopening a known empty editor thread whose transcript is missing
scope: architecture
sources: [internal/acpproxy/proxy.go, internal/acpproxy/bindings_test.go, internal/agent/codex.go, internal/agent/gemini.go]
check: go test ./internal/acpproxy -run 'TestProxyReopenUnpromptedThread|TestProxyLoadRefusesUnrelatedFailures|TestProxyLoadWithNoTranscript'
updated: 2026-11-26
---

# Let an empty editor thread accept its first prompt after reopening

Try loading the saved native session first. When the provider confirms its transcript is missing,
start a fresh session with the same editor thread ID and the current child's shared tools.

**Why:** Zed failed to open a thread created before Codex had written its first rollout.
The user expected to be able to start typing in that thread.

**How to apply:** For Codex and Gemini, check the missing-session detail against the bound native ID.
Gemini can also report that the project has no saved sessions.
Generic internal errors, an unknown thread, and errors naming another ID go back to the editor.

## Changelog
- 2026-11-26 — checked cold loads, closed sessions in a running proxy, and replacement-child replay.
  Restart replay already recreates unprompted sessions. Added wire checks for both editor reopen paths.
