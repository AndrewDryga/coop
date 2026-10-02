---
name: usage-leads-with-limits
description: Usage keeps quota bars first and an estimate last; account details hold coverage metadata, not blanket footers
scope: cli-output
sources: [internal/cli/usage.go, internal/cli/usage_test.go, internal/cli/help.go, internal/cli/profiles.go, internal/agent/usage.go]
check: "go test ./internal/cli -run TestUsage"
updated: 2026-10-02
---

# Usage answers how much capacity remains

`coop usage` and provider summaries show account names, quota bars and resets, then the priced
30-day total as `Σ≈$406.48`, dimmed so it stays quieter than the limits. Without a dollar estimate
(unpriced, unavailable or ambiguous history) the summary shows no Σ line at all, and an editor
row, which is nothing but its total, is omitted; account details say why. Narrow terminals put
the total at the normal four-space fact indent. A reset reads `resets in 2d 2h (Oct 4, 08:35 UTC)`:
the time left, then the moment in UTC, never an unlabeled local clock. Exact-account details keep
the labeled estimate. Do not add default/plan tags, repeated pricing/history qualifications or a
blanket disclaimer footer. Default selection belongs in `coop credentials`.

Keep separate model pools, including unused capacity. The summary omits a provider's own
"not allowed" flag, which only restates a full bar and misleads when credits keep an account
running; account details show it as `limit reached` on a percentage limit, never on a balance.
Hide disabled extra usage, unknown reset placeholders and duplicate reset times on unused buckets.
Show useful remaining balances without excessive decimal precision. Missing usage must never
become zero; failed lookups and sign-in remedies remain visible.

Each provider measures its own columns, so one long label never spreads another provider's
rows: its labels (quota buckets and the fixed `Limits`, `Credential`, `Coop records`,
`30-day API estimate`) pad to its longest label, bars follow two spaces later and align across
its accounts, and every value (percentage, balance, reason, sign-in hint and Σ total) starts
in one column, two spaces after the bar, where `100% used` starts. Percentages right-align to the
width of `100%` (`  3%`, ` 16%`), so `used` and the resets line up whatever the digit count; the
field is fixed, never sized to the widest percentage shown, so columns do not jump when an account
reaches 100%. A value too long for the terminal continues in that column, breaking at ` · `
and then before a parenthetical; only a terminal too narrow for the columns stacks the row
under its label.

An account with nothing but a total is one line, `blitz_ai_studio (API key)  Σ≈$0.48`, and
consecutive one-line accounts stack without blank lines; one blank line separates every other
block and nothing trails the last one. A sign-in kind without provider limits (API key, setup
token, Vertex) comes from the adapter as `UsageQuota.Auth` and shows only as that parenthetical,
never as a disclaimer sentence. Another account-level note on an account with limits rides the
account line, dimmed; without limits it is the account's one capitalized reason line. Shared ACP
history reads `Unassigned editor usage`; its identity and accounting remain separate from named
accounts.

The existing `provider@credential` view holds plan, coverage, pricing and retained-record facts.
Help explains the data scope. Neither view ends with an unsolicited disclaimer paragraph.

## Changelog
- 2026-10-02 — the user, on the per-provider columns: "can 3% used be right aligned? like so?",
  with a mockup of `  3%`, ` 16%` and `100%` ending in one column and Σ starting where `100%`
  starts. usageBucketText now formats the percentage at a fixed width; swept the render paths
  (summary, account details, narrow stacked rows all take it from there, and no other usage
  output formats a percentage). The usage fixtures pin `    0% used`, `   26% used`, the `%`
  column and the stacked row; they fail on the left-aligned renderer.
- 2026-10-02 — the user, same review: "drop Σ unpriced - if provider doesn't have api pricing don't
  show it there" and resets as "resets in ... (Oct 4, 10:35 UTC)". The summary now prints Σ only
  for a dollar estimate; resets count down and name UTC (previously unlabeled local time). Longer
  reset text made 80-column rows stack, so overflow now wraps inside the value column at ` · `
  and before the parenthetical. TestUsageResetLabelCountsDownAndNamesUTC and
  TestUsageWrapsValuesInTheirColumn pin both; help and generated docs say UTC.
- 2026-10-02 — the user, from screenshots: "too much extra spacing to the bars", align credits
  with the percentages "not with progress bar", move Σ "on the same vertical line with beginning
  of 100% used", asked "what blocked means?!", and wanted Gemini as "blitz_ai_studio (API Key) …
  Σ≈$0.48, no disclaimers". Replaced the report-wide gutter with per-provider columns, put every
  value at one column after the bar, dropped the summary's restated flag, and moved the four
  adapters' sign-in sentences into `UsageQuota.Auth`. Swept all four adapters (claude, codex,
  gemini, grok: 6 sentences became Auth labels; Claude's token-scope note stays a note) and both
  render paths; the two usage fixtures fail on the previous renderer and pass now.
- 2026-10-02 — the user asked to fix spacing and make the 30-day cost less prominent. Swept every
  row renderUsage prints: Limits, the sign-in hint, Credential, Coop records and the labeled
  estimate used hard-coded 16- and 19-column labels (Grok's `unpriced` sat one column left), the
  Grok `shared credit pool` note printed as an orphan line, and a blank line trailed the output.
  All now share the measured gutter; the Σ total and account notes are dim; the details footer
  keeps its own separating line. TestUsageAlignsEveryFactAndQuietsTheTotal pins the columns,
  styling, stacking and the missing trailing line (it fails on the previous renderer).
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
