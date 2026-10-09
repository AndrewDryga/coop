---
name: provider-session-history
description: Native session layouts, lookup bounds, and the large-history regression contract
subsystem: testing
sources: [internal/agent/claude.go, internal/agent/codex.go, internal/agent/gemini.go, internal/agent/grok.go, internal/agent/session_history_large_test.go, internal/agent/history_import.go, internal/agent/history_json.go, internal/box/native_home.go, internal/box/native_history.go, internal/box/native_history_index.go, internal/acpctl/bindings.go]
updated: 2026-10-09
---

Resume is exact by provider, account, cwd, and canonical UUID. Claude checks one
`projects/<cwd-key>/<id>.jsonl` path. Codex cannot preset an ID, so it scans bounded first-line
rollout metadata for an already persisted native ID. Gemini scans version-dependent `tmp` buckets:
each accepted bucket carries a bounded absolute `.project_root` marker that exactly matches the
requested workspace. Regular JSON and JSONL chats are supported by the pinned native reader.
Streaming parsing bounds ownership fields, not the whole conversation, and requires the exact
session ID and native `sha256(cwd)` project hash. Missing/malformed markers, symlinks and
oversized ownership metadata fail closed. The cross-bucket scan remains because Gemini
has used both slug and hash bucket names.
Grok scans cwd buckets and accepts only a regular `summary.json` under the exact session directory.

`TestSessionLookupLargeHistory` generates every native layout under disposable account roots. It
locks exact hit, full miss, wrong cwd and ID, malformed input, alternate-account isolation, and
descriptor closure for every registered provider. `BenchmarkSessionLookupLargeHistory` reports
time and allocations diagnostically; it has no machine-sensitive threshold. Do not add a secondary
index or cache unless this evidence first demonstrates a real bound the adapters cannot meet.

## Changelog
- 2026-10-09 — reverified four-provider adapters and pinned Gemini JSON/JSONL reader support.
  Ordinary complete homes are repository/provider/account scoped; ACP homes are account-independent,
  including remote private roots. Validated forks share their parent domain. Streaming imports keep
  original transcripts and require exact cwd ownership. Source/destination leases and writer checks
  fence publication. Receipts outside the writable home prevent resurrection of deleted transcripts
  and consumed index rows; before/after digests recover interrupted index publication without
  overwriting changed native state. Gemini rebuilds projects.json from .project_root markers, so
  no cross-project registry is merged. ACP binding tombstones preserve native deletions too.
- 2026-09-03 - removed absent whole-file JSON and markerless/hash-bucket fallbacks after verifying
  Gemini 0.50.0 state across every managed profile and the ACP session store; retained current
  marker, JSONL metadata, multi-bucket, bounds, symlink, account, and descriptor-leak coverage
- 2026-07-16 - created from the four-provider large-history investigation and Gemini bounded-scan fix
