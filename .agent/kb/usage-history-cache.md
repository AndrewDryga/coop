---
name: usage-history-cache
description: coop usage's per-transcript token cache; changed files invalidate entries, and a complete full check prunes each history folder's unused entries
subsystem: usage
sources: [internal/agent/usage.go, internal/agent/usage_cache.go, internal/cli/usage.go, internal/box/history.go, internal/box/profiles.go, internal/config/config.go]
updated: 2026-10-16
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
`cmdUsage` calls `Prune` only after an uninterrupted bare `coop usage` whose root listings all
succeeded (`usageUnlisted` is empty). Prune then decides per history folder, so one incomplete
folder never stops the others: a folder whose walk missed something keeps every entry; a complete
folder loses the entries it did not look up (deleted transcripts); a folder the check did not read
loses every entry, then its directory once empty. A walk misses something on cancellation or
excess depth, an unstattable path, a link, a non-regular or oversized file, an unreadable
directory, a filter error, or a root or cache folder it could not open. A transcript that reached
its cache lookup counts as seen even if it then fails to open or decode, so its folder can still
be pruned. Prune removes only private regular files with digest names from real 0700 folders,
through `os.Root`.

Root listing: `usageCredentials` finds the folders through `box.EffectiveProfiles` (the
`profiles/` listing in `Config.Profiles`, then `credentials/`), `box.HistoryStores` and
`box.ACPHistoryStores` (every repository `home` of an account, of the shared editor and of each
private editor session) and the private session listing. `usageUnlisted` keeps each provider's
first failure, apart for accounts (`EffectiveProfiles` or a session store; a session store failure
counts for every provider) and for history folders; a missing folder is no failure. Either kind
makes only that provider's rows partial and stops `Prune`, because a folder a failed listing hides
is live but looks unread. The provider's block then ends with `⚠ Could not list every <Provider>
account, so usage is partial` (or `every history folder`) and, beneath it, the failure's cause: a
path and a reason, undimmed and unwrapped. Only a failed account listing replaces the sign-in line
of a block with no rows, and turns an exact `provider@account` that was not found into a listing
error (exit 1) instead of `noAccountErr`: a failed history folder hides no account. Other product
callers ignore these errors on purpose and say why; the tagged live account qualifier refuses a
catalog it cannot read.

## Changelog
- 2026-10-15 — created: cache facts moved from [[usage-recorded-token-value]]; added per-folder
  pruning (audit GC-10). Verified against usage.go, usage_cache.go and cli/usage.go; fixtures
  cover complete, interrupted, linked, unreadable-directory, filter-error, unusable-cache-folder,
  unreadable-root and provider-filtered checks, plus links and foreign files in the cache.
- 2026-10-16 — root listings report errors instead of hiding them, replacing the trap that a
  failed listing made a live folder look unread and pruned: `Config.Profiles`, `EffectiveProfiles`
  and `nativeHistoryHomes` return any error but a missing folder, and `coop usage` folds it into
  `partial`. Fixtures lock `profiles/`, `credentials/`, an account's homes folder, one repository
  home and `acp-homes/`.
- 2026-10-16 — `partial` is per provider: a failed listing marks only its provider, whose block
  says so instead of offering a sign-in, and an exact account it may hide is a listing error.
  Pruning still needs no failure anywhere.
- 2026-10-16 — the warning names what failed (accounts or history folders) with the cause beneath
  it; only a failed account listing drops the sign-in line. Bare system errors from the history
  listing now carry their path (`withPath`), and `usageDirs` errors name their folder.
