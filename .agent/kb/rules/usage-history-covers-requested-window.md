---
name: usage-history-covers-requested-window
description: Lifetime scan budgets must not omit recent usage; caches preserve request accounting
scope: architecture
sources: [internal/agent/usage.go, internal/agent/usage_cache.go, internal/cli/usage.go]
check: "go test ./internal/agent -run 'TestUsageHistoryBeyondFormerByteLimits|TestUsageHistoryCacheReuseAndInvalidation'"
updated: 2026-10-14
---

# Scan the requested usage window without lifetime truncation

Large historical transcripts must not consume a fixed whole-root budget before newer requests
are read. Bound individual records and memory, preserve cancellation, and cache normalized token
records from unchanged files. Never cache a truncated aggregate as the account's 30-day total.
Keep prices and window filtering live, reevaluate dependent inclusion filters, and reparse changed
sources when the provider decoder needs cumulative baselines or message revisions.

**Why:** The owner's OpenUsage comparison revealed about a tenfold Claude and 115-fold Codex
gap. Coop's 256 MiB root budget read only 42/801 Claude and 121/1880 Codex files on that host;
its latest parsed Codex usage was ten days old. The owner requested: "Get it fixed, maybe also
use cache so we do not read large files all the time".

**How to apply:** Exhaustively stream declared native paths with safe record bounds and private
per-file caching. A warm cache must preserve attribution, deduplication, failure flags and tariff
changes; corrupt/unavailable caches fall back to native data. Provider lookup deadlines do not
bound cold history coverage. Snapshot formats that allocate whole records retain explicit bounds.

## Changelog
- 2026-10-14 — swept the four adapters and shared CLI collector. One shared lifetime-budget
  violation fixed here; Claude/Codex now stream large JSONL files with record recovery. Gemini/Grok
  snapshot decoder bounds retained. Regression scans exceed both old byte limits and prove cache
  reuse, invalidation and uncached window/tariff semantics.
