---
name: usage-leads-with-limits
description: Usage keeps quota bars first and an estimate last; account details hold coverage metadata, not blanket footers
scope: cli-output
sources: [internal/cli/usage.go, internal/cli/usage_test.go, internal/cli/help.go, internal/cli/profiles.go]
check: "go test ./internal/cli -run TestUsage"
updated: 2026-10-02
---

# Usage answers how much capacity remains

`coop usage` and provider summaries show account names, quota bars and resets, then the priced
30-day total as `Σ≈$406.48`, aligned under the percentage number. Narrow terminals put it at the
normal four-space fact indent. Exact-account details and non-currency results keep the estimate
label. Do not add default/plan tags, repeated pricing/history qualifications or a
blanket disclaimer footer. Default selection belongs in `coop credentials`.

Keep separate model pools, including unused capacity, and bucket-specific blocks. Hide disabled
extra usage, unknown reset placeholders and duplicate reset times on unused buckets. Show useful
remaining balances without excessive decimal precision. Missing usage must never become zero;
failed lookups and sign-in remedies remain visible.

Measure one quota-label gutter for all displayed accounts so every progress bar starts in the
same column. Shared ACP history reads `Unassigned editor usage`; its identity and accounting
remain separate from named accounts. If a quota has an unavailable reason, show that single
line instead of an empty `Limits unavailable` placeholder plus the reason.

The existing `provider@credential` view holds plan, coverage, pricing and retained-record facts.
Help explains the data scope. Neither view ends with an unsolicited disclaimer paragraph.

## Changelog
- 2026-10-02 — adopted the user's compact Sigma total. Swept the two estimate output paths;
  both use one renderer, preserving shared attribution and non-currency results. Existing fixtures
  now pin total alignment and narrow-terminal fallback; help explains the unchanged 30-day meaning.
- 2026-10-02 — applied the approved follow-up after inspecting actual output: one measured bar
  gutter across accounts, plain-English shared editor label, and one accurate Gemini unavailable
  line for API-key or Vertex modes. Swept both render paths and the Gemini quota note; focused
  fixtures now check common columns, shared values and absence of the duplicate placeholder.
- 2026-10-02 — recorded the approved bar-based preview and corrections against default tags and
  disclaimer footers. Swept the usage renderer, help, README and credential renderer: usage had
  one unconditional three-line footer and default tags; credentials retains its useful selection
  marker. Focused fixtures pin summary omissions, detail facts, independent failures and reserve
  capacity. Generated manual/site/manpage copies follow the same help source.
