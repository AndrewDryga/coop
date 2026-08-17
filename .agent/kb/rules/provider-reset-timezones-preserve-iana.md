---
name: provider-reset-timezones-preserve-iana
description: Preserve an exact provider reset timezone; parse safe IANA names and fail closed on ambiguous abbreviations
scope: loop
sources: [internal/ladder/limit.go, internal/ladder/limit_test.go]
check: go test ./internal/ladder -run 'TestParseResetTime|TestDetectIterationLimitCarriesTheProviderIANAReset'
updated: 2026-08-17
---

# Preserve the provider's exact reset timezone

When a provider states an absolute quota-reset time, parse a safe IANA location
such as `America/Merida` with its real zone rules. Keep ambiguous abbreviations
and unknown or path-shaped names fail-closed; never reinterpret them in the host
timezone.

**Why:** A Claude weekly-limit notice stated `America/Merida`, but the parser
accepted only short abbreviations. Coop discarded the real multi-day reset and
made Responder repeatedly retry and report generic short waits.

**How to apply:** Keep provider reset parsing in `internal/ladder/limit.go`; add
the exact provider prose to the table and end-to-end limit detector tests before
changing accepted timezone syntax.

## Changelog
- 2026-08-17 — created after sweeping the reset parser and its two test surfaces;
  the IANA rejection was the only current violation and is fixed here.
