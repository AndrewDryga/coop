---
name: eval-spend-gates-follow-account-billing
description: eval spending gates follow the selected account's billing mode rather than assuming all native CLI runs incur new charges
scope: agent-workflow
sources: [internal/cli/eval_cmd.go, internal/cli/eval_trial.go, internal/cli/eval_loop.go, README.md, docs/cli.md]
check: none
updated: 2026-09-30
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

**Why:** after practical evals were blocked on a provider spend cap, the user asked,
"why we need to spend anything if i already have all those" and supplied their signed-in accounts.
The blocker confused using existing subscriptions with incurring additional provider charges.

**How to apply:** inspect the native eval account selection and selected authentication mode,
then distinguish quota scheduling from incremental spending. Retain provider refusals as
incomplete infrastructure outcomes, and keep unknown usage/cost unknown. Official Codex
[authentication guidance](https://learn.chatgpt.com/docs/auth) distinguishes subscription and
usage-billed API-key access; it does not establish the billing mode of every stored Coop account.

## Changelog
- 2026-09-30 — swept the five source files and seven active practical-eval guidance files.
  Native launches already use selected accounts; public docs correctly say runs may spend
  credits. Corrected the parent's unconditional spend gate in task/spec/decision/state and pilot
  guidance, appending its log. The independent loop child's contextual paid-campaign wording
  does not block its work and was left with its active owner. Account billing and current quota
  require external evidence, so this remains a review rule with `check: none`.
