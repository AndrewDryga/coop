---
name: eval-spend-gates-follow-account-billing
description: eval spending gates follow the selected account's billing mode rather than assuming all native CLI runs incur new charges
scope: agent-workflow
sources: [internal/cli/eval_cmd.go, internal/cli/eval_trial.go, internal/cli/eval_loop.go, internal/testutil/liveprovider/credentials.go, tools/qualify/main.go, Makefile, README.md, docs/cli.md]
check: none
updated: 2026-10-01
---

# Base eval spending gates on the selected account's billing mode

Native evals use the operator's selected CLI accounts. Use existing included subscription
allowance where available; do not invent an additional API-spending prerequisite for it.
Confirm billing mode before claiming a selected account incurs new charges or is free:
`coop credentials` reports stored account availability, not billing terms or remaining quota.

Before a campaign, pin exact model/preset/account configurations and bound trial count, concurrency
and runtime. Included allowance is governed by provider quota and rate limits. Additional billed
API usage, purchased credits or paid overage require the user's spend authorization; do not switch
to them automatically when included quota is exhausted. Missing campaign configuration or spend
authorization must not block independent provider-free suite qualification.

One unavailable account or provider is not a global work blocker. Recheck the existing account
catalog and current allowance, then select an existing working included account explicitly for
the independent approved checks it can run. Keep persistent defaults unchanged. Do not ask for
new credentials or spending machinery while those checks remain available. A stored credential
or recent refresh is not proof of current server acceptance; verify a bounded real workflow
before carrying a historical account blocker forward. Conversely, success on another provider
does not satisfy a required provider-specific qualification row.

The Coop catalog is not every native authority on the host. If the user reports a working account,
check the native client's declared host store/keychain too, without exposing secrets or changing
defaults. A refusal on the catalog default is not a refusal on that separate login. Diagnose with
bounded real requests, then qualify the exact pinned client through access-only selected authority.

**Why:** after practical evals were blocked on a provider spend cap, the user asked,
"why we need to spend anything if i already have all those" and supplied their signed-in accounts.
The blocker confused using existing subscriptions with incurring additional provider charges.

**How to apply:** inspect the native eval account selection and selected authentication mode,
then distinguish quota scheduling from incremental spending. Retain provider refusals as
incomplete infrastructure outcomes, and keep unknown usage/cost unknown. Official Codex
[authentication guidance](https://learn.chatgpt.com/docs/auth) distinguishes subscription and
usage-billed API-key access; it does not establish the billing mode of every stored Coop account.

## Changelog
- 2026-10-01 — corrected the missed Claude macOS keychain authority after the user's account
  correction. Swept catalog selection, live projection, native auth status and host-vault seams:
  Coop Claude intentionally has no host-vault import. Native keychain and personal authority differ;
  normal native refresh followed by access-only temporary projection passes pinned2.1.285 prompt.
  Preserved defaults and refresh tokens on the host. The catalog's rate limit is not global.
- 2026-10-01 — the user corrected stopping for new Gemini authority despite existing working
  accounts. Swept the eight source files plus dependency/usage/release task checkpoints:
  explicit per-run account selection already exists, and the strict collector correctly refuses
  incomplete provider scope. Corrected task sequencing, verified current Codex/Grok allowances
  and real prompt/resume success without changing defaults; retained the independently rejected
  Emisar prompt and the remaining Claude/Gemini qualification gaps. No new spending guard or
  authentication flow. Account billing/readiness and independent-scope selection remain review
  judgments, so `check: none` is still honest.
- 2026-09-30 — swept the five source files and seven active practical-eval guidance files.
  Native launches already use selected accounts; public docs correctly say runs may spend
  credits. Corrected the parent's unconditional spend gate in task/spec/decision/state and pilot
  guidance, appending its log. The independent loop child's contextual paid-campaign wording
  does not block its work and was left with its active owner. Account billing and current quota
  require external evidence, so this remains a review rule with `check: none`.
