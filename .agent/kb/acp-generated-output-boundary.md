---
name: acp-generated-output-boundary
description: Discarded tool streams bypass transcript bytes while durable outputs remain bounded.
subsystem: acp
sources: [internal/sessionsvc/acp.go, internal/sessionsvc/output.go, internal/session/store.go, internal/sessionsvc/http.go]
updated: 2026-08-09
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

Text tool updates follow the same retained-state distinction. Coop inspects each already
frame-bounded update for artifacts and then discards it, so cumulative tool traffic is not retained
transcript and must not become a correctness limit. Assistant text, individual frames, turn
deadlines, artifacts, and the native provider context remain independently governed.

## Changelog
- 2026-08-09 — sources repointed: the sessions service moved out of `internal/cli/session_*.go` into `internal/sessionsvc/`; the facts here are unchanged (a move-only extraction).
- 2026-08-02 — stopped cumulative discarded text tool updates from aborting long valid turns.
- 2026-07-31 — created with the bounded generated-image turn contract.
