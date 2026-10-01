---
name: usage-recorded-token-value
description: Usage reports native quota and recorded token value independently; historical account ambiguity and missing tariffs stay explicit
subsystem: usage
sources: [internal/agent/usage.go, internal/agent/claude.go, internal/agent/codex.go, internal/agent/gemini.go, internal/agent/grok.go, internal/box/usage.go, internal/box/run.go, internal/cli/usage.go, internal/session/usage.go]
updated: 2026-10-01
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

The CLI reads ordinary credential roots and default session-private ACP roots. Shared editor
ACP roots have no historical account binding; copies shared by two named credentials become
unattributed instead of being charged to today's default or displayed as account zero.
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
subscription quota but can still have valued native history.

## Changelog
- 2026-10-01 — created after verifying the adapters, account dedup, read-only WAL snapshot and
  helper/run lifecycle. Focused fixtures cover pricing boundaries, exact credential selection,
  failed cleanup, ambiguous attribution, orphan child accounting and asynchronous persistence.
  Actual host quota/history reads worked for available accounts; live Gemini OAuth helper is
  unverified because configured accounts select API-key/Vertex modes.
