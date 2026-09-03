---
name: provider-session-history
description: Native session layouts, lookup bounds, and the large-history regression contract
subsystem: testing
sources: [internal/agent/claude.go, internal/agent/codex.go, internal/agent/gemini.go, internal/agent/grok.go, internal/agent/session_history_large_test.go]
updated: 2026-09-03
---

Resume is exact by provider, account, cwd, and canonical UUID. Claude checks one
`projects/<cwd-key>/<id>.jsonl` path. Codex cannot preset an ID, so it scans bounded first-line
rollout metadata for an already persisted native ID. Gemini scans version-dependent `tmp` buckets:
each accepted bucket carries a bounded absolute `.project_root` marker that exactly matches the
requested workspace. Within owned buckets, only regular `*.jsonl` chats are considered, and their
first metadata record must decode completely within 1 MiB with both the exact session ID and
native `sha256(cwd)` project hash. Missing or malformed markers, whole-file JSON, symlinks, and
oversized or malformed current metadata fail closed. The cross-bucket scan remains because Gemini
has used both slug and hash bucket names.
Grok scans cwd buckets and accepts only a regular `summary.json` under the exact session directory.

`TestSessionLookupLargeHistory` generates every native layout under disposable account roots. It
locks exact hit, full miss, wrong cwd and ID, malformed input, alternate-account isolation, and
descriptor closure for every registered provider. `BenchmarkSessionLookupLargeHistory` reports
time and allocations diagnostically; it has no machine-sensitive threshold. Do not add a secondary
index or cache unless this evidence first demonstrates a real bound the adapters cannot meet.

## Changelog
- 2026-09-03 - removed absent whole-file JSON and markerless/hash-bucket fallbacks after verifying
  Gemini 0.50.0 state across every managed profile and the ACP session store; retained current
  marker, JSONL metadata, multi-bucket, bounds, symlink, account, and descriptor-leak coverage
- 2026-07-16 - created from the four-provider large-history investigation and Gemini bounded-scan fix
