---
name: provider-session-history
description: Native session layouts, lookup bounds, pre-v11 import completion stamps, and the large-history regression contract
subsystem: testing
sources: [internal/agent/claude.go, internal/agent/codex.go, internal/agent/gemini.go, internal/agent/grok.go, internal/agent/session_history_large_test.go, internal/agent/history_import.go, internal/agent/history_json.go, internal/box/native_home.go, internal/box/native_history.go, internal/box/native_history_index.go, internal/box/native_history_stamp.go, internal/acpctl/bindings.go, internal/cli/acp_cmd.go]
updated: 2026-10-19
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

Planning a pre-v11 import hashes every owned retained file, and the Claude planner parses every
row of every transcript, so a finished import must not be planned again on each launch. A
completion stamp (`internal/box/native_history_stamp.go`) beside the receipts, outside the mounted
home, records one source's fingerprint per ownership scope: every entry's path, type, size, inode,
mtime and ctime from a stat walk, taken before planning, written only after the import completed.
A matching stamp skips planning entirely; any write, rename, link or chmod in the source moves a
ctime, which nothing can set back. Which transcripts a project owns depends on its fork records, so
a project's stamps also cover its fork records' `*.generation.json` and the fork anchors (`forkRecordsStore`, in `forkspace.StateDir`): a fork
recorded after an import reopens it. A finished receipt is kept per file, so an edited transcript
that was already imported is not imported again; new history is. Private
ACP homes scope stamps by their exact owned working folders, because the session's child prepares
the same home under repository ownership. The `coop acp` binding migration has its own stamp: the
flat retained binding files (not the per-repository subfolders, which change on every save),
every retained ACP session store, the project's fork records, and each ACP home's `imports/`
listing without its own `complete-` stamps. The homes themselves are
left out, since they change on every turn and only with sessions newer than any retained binding.
The walk pays one stat per entry, caches the planners never read included: on 2026-10-15 a
9.7k-entry Codex profile (8k of them under `.tmp`) still cost ~0.1 s on a loaded host.

`TestSessionLookupLargeHistory` generates every native layout under disposable account roots. It
locks exact hit, full miss, wrong cwd and ID, malformed input, alternate-account isolation, and
descriptor closure for every registered provider. `BenchmarkSessionLookupLargeHistory` reports
time and allocations diagnostically; it has no machine-sensitive threshold. Do not add a secondary
index or cache unless this evidence first demonstrates a real bound the adapters cannot meet.

## Changelog
- 2026-10-19 — fork records now live in `forkspace.StateDir` (Coop's private folder); a stamp
  taken before the move no longer matches, so the next launch plans the import once more.
- 2026-10-15 — added completion stamps so launches stop re-planning finished pre-v11 imports
  (task 2026-10-15-stop-rescanning-pre-v11-history-on-every-launch); reverified the planners and
  receipts against the sources above.
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
