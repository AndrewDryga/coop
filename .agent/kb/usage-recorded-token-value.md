---
name: usage-recorded-token-value
description: Usage reports native quota and recorded token value independently; historical account ambiguity and missing tariffs stay explicit
subsystem: usage
sources: [internal/agent/usage.go, internal/agent/usage_report.go, internal/agent/claude.go, internal/agent/codex.go, internal/agent/gemini.go, internal/agent/grok.go, internal/box/usage.go, internal/box/run.go, internal/cli/usage.go, internal/cli/usage_reports.go, internal/session/usage.go]
updated: 2026-10-13
---

`coop usage [<provider>[@credential]]` never starts a model run. Local recorded value is not billing;
explicitly configured provider reports are a separate API-spend inspection.
Adapters own exact quota authority, native history decoding and current standard-price mappings.
Quota and recorded value can fail independently; no percentage implies missing token use.

`ReadUsageHistory` bounds native roots/files and sees old records needed as cumulative baselines.
`ValueUsageHistory` uses one rolling 30-day window and unrounded, request-ID-deduplicated values.
Claude message revisions, Codex paired response/cumulative rows, Gemini updates/rewinds and Grok
parent/child aggregates have different identity and token inclusion semantics. Grok child usage
is included only when its parent accounting is demonstrably absent; all Grok history remains
partial. Unknown model, cache TTL or long-context tariff inputs stay unpriced.

Provider quota is account-wide; the dollar estimate is not. The CLI reads Coop-managed credential
roots, session-private ACP roots and adapter-declared native host homes (default plus supported
override). Gemini's override is a parent home and gets `.gemini` appended; the others select the
provider root. Web/mobile usage and other machines remain outside local coverage. Native records
without a unique historical managed account owner stay `Unassigned local usage`. Shared/shared
copies retain one source owner; unique managed account ownership wins, retaining better event
versions from copied histories. Conflicting named owners stay unassigned. Deduped-away shared rows
are unavailable, not a fabricated known zero.
The compact view leads with quota bars, then the 30-day API estimate as a dim, aligned Sigma total
as `Σ≈$N/30d` when one is priced; unavailable/unpriced results show no account total in the summary.
Provider Subtotal and bold grand Total sum unrounded deduplicated rows, including unassigned
editor history once. Missing, ambiguous, partial and unpriced coverage marks aggregates partial;
no known recorded amount means no money aggregate, not a fabricated zero. Exact-account selection
omits aggregate rows. Timing is a separately measured dim gray suffix with two spaces, no dot.
Sign-ins without
provider limits (API key, setup token, Vertex) report `UsageQuota.Auth` and render as one line
beside their total. Exact-account selection adds plan, coverage, pricing and retained-record
details without a blanket disclaimer footer.
Shared editor ACP roots have no historical account binding; copies shared by two named credentials
become unattributed instead of being charged to today's default or displayed as account zero.
The view calls that shared row `Unassigned editor usage`; this does not change its account binding.
`ReadUsageSnapshot` opens existing SQLite read-only, including live WAL, without `Store.Open`,
schema migration or checkpoint. Creation/rotation events provide historical targets; rotation
during a turn and implicit defaults remain ambiguous. Old aggregate usage is unpriced coverage,
never dollars added to overlapping native requests. Custom service roots are not auto-discovered.

Canonical quota reads use the host account authority's Current callback and renewal path for all
four providers; no quota helper receives a copied refresh grant. Gemini issues native Code Assist
load/quota requests directly from the host. The legacy plain-OAuth helper remains only for
pre-cutover inspection; it leases its original home, never creates a second authority.
Encrypted-only legacy caches remain unavailable. API keys have no subscription quota but can have
valued history. Discovery includes ordinary repository homes and both local and private remote ACP
homes; account-independent editor history remains unassigned.

Codex 0.159.2 quota GET `/backend-api/wham/usage` supplies nullable
`rate_limit_reset_credits.available_count`. An optional bounded GET of
`/backend-api/wham/rate-limit-reset-credits` supplies its own authoritative count and available
Codex reset expiry details. List length is never the count; it may be capped. Detail failures keep
usable quota/count, and missing expiry is not guessed. Only expiry timestamps survive adaptation;
opaque IDs, profile information and upstream prose are discarded. No inspection request redeems
a reset or opts into reserve capacity. Credentials and account headers remain host-only.

Claude Code 2.1.285 reads optional reset grants using OAuth GET
`/api/oauth/usage?cedar_ember=1&skip_spend=1`. The adapter keeps the ordinary usage response for
limits/extra usage, then makes a bounded optional reset read with the same canonical authority.
For an eligible account with a complete valid grants array it sums `resets_left`, matching the
native client, never grant-list length. `usable_now` describes current redemption eligibility,
not remaining resets. Missing/ineligible/malformed metadata remains unknown; failure never
discards ordinary quota. Only positive remaining grants contribute reported `ends_at` expiry.
This is inspection only: no organization reset route, grant IDs, event properties or redemption.

## Changelog
- 2026-10-13 — added adapter-declared host history roots and generalized shared deduplication.
  Optional host-only usage-reports.json sources read organization/team billing, or explicitly
  selected Google project/SKUs, for 30 completed UTC days. Strict source schema, fixed endpoints,
  bounded responses/pages, USD units and failure paths are fixture-tested. Scope/exclusions and
  fetch time remain visible; reports never join rolling local estimates or personal accounts.
- 2026-10-12 — verified Claude reset query, grants schema and remaining-count sum in the exact
  locked native client and the live selected-account OAuth response. Added optional failure-safe
  mapping with missing/null/invalid/overflow/expiry/authority regressions.
- 2026-10-12 — verified reset endpoints and response keys against pinned upstream backend-client
  source/tests; added nullable count, reported expiry and read-only failure coverage. Verified
  compact style and aggregates against focused CLI deduplication/rounding/partial/layout fixtures.
- 2026-10-09 — verified canonical quota callbacks, repository history discovery and unassigned
  private remote ACP homes against box/usage.go and cli/usage.go.
- 2026-10-02 — summary totals now appear only when priced; verified against usageTotal.
- 2026-10-02 — sign-ins without limits now report `UsageQuota.Auth` instead of a reason sentence;
  verified against the four adapters and renderUsage.
- 2026-10-02 — summary non-currency estimates now share the dim Σ column; verified against
  renderUsage in internal/cli/usage.go. Collector, pricing and attribution unchanged.
- 2026-10-02 — verified the read scope against `AgentProfileDir` and `usageCredentials`; documented
  the approved quota-first summary and existing exact-account detail view. Collector, pricing,
  attribution and exit-status semantics are unchanged; focused CLI fixtures cover both views.
- 2026-10-01 — created after verifying the adapters, account dedup, read-only WAL snapshot and
  helper/run lifecycle. Focused fixtures cover pricing boundaries, exact credential selection,
  failed cleanup, ambiguous attribution, orphan child accounting and asynchronous persistence.
  Actual host quota/history reads worked for available accounts; live Gemini OAuth helper is
  unverified because configured accounts select API-key/Vertex modes.
