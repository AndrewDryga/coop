---
name: usage-history-cache
description: coop usage's per-transcript token cache; changed files invalidate entries, and a complete full check prunes each history folder's unused entries
subsystem: usage
sources: [internal/agent/usage.go, internal/agent/usage_cache.go, internal/cli/usage.go, internal/box/history.go, internal/box/profiles.go]
updated: 2026-10-15
---

`ReadUsageHistory` caches normalized token records in private `BoxHome/usage-cache`: one folder
per provider history root, named by the SHA-256 of the provider kind and absolute root path, and
one entry per transcript, named by the SHA-256 of its root-relative path. An entry keeps tokens
and coverage flags, versioned by decoder schema. Device/inode, size, mtime and ctime invalidate
changed sources, including same-size rewrites with restored mtime. Only stable, uncancelled reads
are saved atomically. Invalid or unwritable caches fall back to native parsing. Changed files are
fully reparsed; unchanged files are not reopened. Prices, account attribution and the rolling
window are not cached. Grok's dependent parent filter runs before cache reuse.

Pruning: one `UsageCache` per check records the entries each history folder's walk looked up.
`cmdUsage` calls `Prune` only after an uninterrupted bare `coop usage` whose root listing reported
no failure (`!partial`). Prune then decides per history folder, so one incomplete folder never
stops the others: a folder whose walk missed something keeps every entry; a complete folder loses
the entries it did not look up (deleted transcripts); a folder the check did not read loses every
entry, then its directory once empty. A walk misses something on cancellation or excess depth, an
unstattable path, a link, a non-regular or oversized file, an unreadable directory, a filter
error, or a root or cache folder it could not open. A transcript that reached its cache lookup
counts as seen even if it then fails to open or decode, so its folder can still be pruned. Prune
removes only private regular files with digest names from real 0700 folders, through `os.Root`.

Trap: `box.HistoryStores` (history.go) and `box.EffectiveProfiles` (profiles.go) drop listing
errors silently, so a failed listing makes a live folder look unread: a full check deletes its
entries and the next check reparses it once.

## Changelog
- 2026-10-15 — created: cache facts moved from [[usage-recorded-token-value]]; added per-folder
  pruning (audit GC-10). Verified against usage.go, usage_cache.go and cli/usage.go; fixtures
  cover complete, interrupted, linked, unreadable-directory, filter-error, unusable-cache-folder,
  unreadable-root and provider-filtered checks, plus links and foreign files in the cache.
