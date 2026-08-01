---
name: acp-generated-output-boundary
description: Generated images bypass transcript bytes but remain bounded, immutable turn artifacts.
subsystem: acp
sources: [internal/cli/session_acp.go, internal/cli/session_output.go, internal/session/store.go, internal/cli/session_http.go]
updated: 2026-07-31
---

ACP image chunks can be several megabytes of base64. Counting those bytes as ordinary transcript
causes a valid image turn to fail before its terminal answer is available. Coop decodes and validates
image chunks separately, charges only bounded metadata to transcript accounting, and stores the
binary against the completed turn. Agents may also save images under the exact per-turn output
directory named in their prompt. Coop removes that scratch directory after capture.

Image blocks may be direct assistant chunks or nested typed content/resource blocks in tool-call
updates. Tool-returned images receive deterministic `generated-N.<ext>` names so the final response
can reference them without embedding bytes. Arbitrary strings that merely resemble base64 are not
treated as artifacts.

Public turn JSON contains metadata only. Consumers fetch one immutable image through the turn
artifact endpoint and must verify its advertised size and SHA-256 digest before publication.

## Changelog
- 2026-07-31 — created with the bounded generated-image turn contract.
