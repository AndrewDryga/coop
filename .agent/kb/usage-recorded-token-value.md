---
name: usage-recorded-token-value
description: Usage reports native quota and recorded token value independently; historical account ambiguity and missing tariffs stay explicit
subsystem: usage
sources: [internal/agent/usage.go, internal/agent/claude.go, internal/agent/codex.go, internal/agent/gemini.go, internal/agent/grok.go, internal/box/usage.go, internal/box/run.go, internal/cli/usage.go, internal/session/usage.go]
updated: 2026-10-02
---

`coop usage [<provider>[@credential]]` is an inspection, not a model run or a billing report.
Adapters own exact quota authority, native history decoding and current standard-price mappings.
Quota and recorded value can fail independently; no percentage implies missing token use.

`ReadUsageHistory` bounds native roots/files and sees old records needed as cumulative baselines.
`ValueUsageHistory` uses one rolling 30-day window and unrounded, request-ID-deduplicated values.
Claude message revisions, Codex paired response/cumulative rows, Gemini updates/rewinds and Grok
parent/child aggregates have different identity and token inclusion semantics. Grok child usage
is included only when its parent accounting is demonstrably absent; all Grok history remains
partial. Unknown model, cache TTL or long-context tariff inputs stay unpriced.

Provider quota is account-wide; the dollar estimate is not. The CLI reads only Coop-managed
credential roots and default session-private ACP roots, not independent native host homes,
web/mobile usage or other machines. Native runs that write into those same roots can be included.
The compact view leads with quota bars, then the 30-day API estimate as a dim, aligned Sigma total
when one is priced; unavailable/unpriced results show no total in the summary. Sign-ins without
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

Gemini OAuth quota uses a fixed auth/server-only helper in the exact managed base image, with
no repository, CLI initialization, model, MCP, hooks or onboarding. The selected original plain
OAuth home is its only writable host bind. A host shared/exclusive lease protects that refresh
authority against participating Coop runs/logins, including consult peers. Profile container
labels keep failed teardown busy after the host flock closes. Native processes and older Coop
versions do not participate; encrypted/keychain portability and missing runtime remain explicit
unavailable results, not temporary refresh-token copies. API-key/Vertex auth has no Code Assist
subscription quota but can still have valued native history. Its unavailable line names the
selected mode without a second generic limits placeholder.

## Changelog
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
