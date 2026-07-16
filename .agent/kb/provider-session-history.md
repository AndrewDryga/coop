---
name: provider-session-history
description: Native session layouts, lookup bounds, and the large-history regression contract
subsystem: testing
sources: [internal/agent/claude.go, internal/agent/codex.go, internal/agent/gemini.go, internal/agent/grok.go, internal/agent/session_history_large_test.go]
updated: 2026-07-16
---

Resume is exact by provider, account, cwd, and canonical UUID. Claude checks one
`projects/<cwd-key>/<id>.jsonl` path. Codex cannot preset an ID, so it scans bounded first-line
rollout metadata for an already persisted native ID. Gemini scans version-dependent `tmp` buckets:
current buckets carry a bounded absolute `.project_root` marker that rejects foreign cwd histories
before chat inspection, while markerless legacy buckets fall back to bounded JSON/JSONL metadata.
Legacy whole-record JSON decoding is capped at 4 MiB because `encoding/json` buffers one complete
top-level value. Current JSONL's first metadata record is expected to be small and must decode
completely within 1 MiB; oversized metadata fails closed.
Grok scans cwd buckets and accepts only a regular `summary.json` under the exact session directory.

`TestSessionLookupLargeHistory` generates every native layout under disposable account roots. It
locks exact hit, full miss, wrong cwd and ID, malformed input, alternate-account isolation, and
descriptor closure for every registered provider. `BenchmarkSessionLookupLargeHistory` reports
time and allocations diagnostically; it has no machine-sensitive threshold. Do not add a secondary
index or cache unless this evidence first demonstrates a real bound the adapters cannot meet.

## Changelog
- 2026-07-16 - created from the four-provider large-history investigation and Gemini bounded-scan fix
