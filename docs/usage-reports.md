# Usage estimates and provider reports

`coop usage` shows account limits first, then estimates what your recorded activity would cost at
API prices. It reads co:op histories and supported native CLI histories on this computer, and
doesn't include browser or mobile conversations or histories on other computers. Deleted histories
and unknown model prices can leave gaps.

| Provider | Default history home | Override |
|---|---|---|
| Claude | `~/.claude` | `CLAUDE_CONFIG_DIR` |
| Codex | `~/.codex` | `CODEX_HOME` |
| Gemini | `~/.gemini` | `GEMINI_CLI_HOME/.gemini` |
| Grok | `~/.grok` | `GROK_HOME` |

If you set an override, co:op scans both it and the default location when they differ. It doesn't
change either folder or its sign-ins, and it counts copied requests only once.

co:op assigns a request to an account only when it finds the request in exactly one co:op account.
Other native history appears as `Unassigned local usage`, because signing in today can't show which
account made an old request. Shared editor histories appear as `Unassigned editor usage`. If copies
point to conflicting accounts, the request stays unassigned instead of going to your currently
selected account.

Provider subtotals and Total count unassigned history once. `≈$.../30d` estimates the activity's
value at standard API prices, including activity covered by a subscription. It doesn't show what
you paid. Use `coop usage codex@personal` (or another provider/account) to see which history and
prices the estimate includes.

Incomplete coverage shows as `partial`, and missing or unpriced history never counts as zero. Saved
co:op turn summaries have no prices, so the native-history totals leave them out.

co:op caches the token records it parses under `usage-cache` in `~/.local/state/coop`, so later
checks read only the files that changed. The cache holds no conversation text. A finished check of
every provider removes the records of deleted files.

## Account limits

Limits come from the provider and cover the whole account. A dimmed reset time shows the time left
and the date in UTC. Claude and Codex also show available resets and their reported expiry dates
when the provider supplies them. Checking usage never redeems a reset. Credits appear in the
provider's own units, not dollars.

co:op checks the limits of the accounts it manages using credentials that stay on the host, and it
sends no model prompt. A Gemini login that still uses its original credential store needs Docker
for this check. If an account is busy, wait for its run to finish and try again.

API-key and Vertex accounts may have priced token history even when they have no subscription
limits to report.

## Connect an API billing source

To enable provider reports, create `~/.config/coop/usage-reports.json`, or
`$XDG_CONFIG_HOME/coop/usage-reports.json` if you set the XDG config home. The file must be a
regular file, not a symlink, and can define one reporting source per provider. `name` labels the
source and doesn't select a co:op account.

```json
{
  "reports": [
    {
      "provider": "codex",
      "name": "work",
      "organization": "org-example",
      "key_env": "COOP_REPORT_OPENAI_KEY"
    },
    {
      "provider": "claude",
      "name": "work",
      "key_env": "COOP_REPORT_CLAUDE_KEY"
    },
    {
      "provider": "grok",
      "name": "work",
      "team": "your-team-id",
      "key_env": "COOP_REPORT_XAI_KEY"
    }
  ]
}
```

Set the named environment variables in the host shell where you run co:op. Don't put the keys in
this JSON file, your project or `agents/env`, which is forwarded into boxes. Reporting keys can have
broader permissions than model API keys, so grant only the access needed for reports.

co:op keeps these keys out of agent homes and containers, and its error messages don't include the
key or the provider's response body.

- OpenAI: use an organization Admin API key. `organization` selects the organization sent to the
  [Costs API](https://developers.openai.com/api/reference/resources/admin/subresources/organization/subresources/usage/methods/costs).
- Anthropic: use an organization Admin API key. The key determines the organization. Its
  [cost report](https://platform.claude.com/docs/en/manage-claude/usage-cost-api) excludes Priority
  Tier charges, so co:op marks this source as partial.
- xAI: use a separate Management API key authorized to read the configured team's
  [billing usage](https://docs.x.ai/developers/rest-api-reference/management/billing). Don't use an
  inference key. The report reads billing usage, not a prepaid balance.

These sources report API usage for an organization or team, which can include other applications
and devices in it. They don't include personal subscriptions or ChatGPT or Claude web activity.

## Google billing exports

For Google reports, set up a [standard Cloud Billing export](https://docs.cloud.google.com/billing/docs/how-to/export-data-bigquery).
The host's `gcloud` account needs access to that export and permission to create BigQuery query jobs
in the selected project. If the configured email isn't signed in, run `gcloud auth login`. This
sign-in is separate from your Gemini CLI sign-in.

co:op gets a fresh short-lived access token through
`gcloud auth print-access-token <email> --quiet`. It doesn't copy a Google refresh credential.

```json
{
  "reports": [
    {
      "provider": "gemini",
      "name": "work",
      "project": "my-gemini-project",
      "billing_table": "billing-project.billing.gcp_billing_export_v1_example",
      "sku_ids": ["your-gemini-api-sku-id"],
      "location": "US",
      "gcloud_account": "billing@example.com",
      "maximum_bytes_billed": 104857600
    }
  ]
}
```

`billing_table` names the export table as `project.dataset.table`. In your billing export, find the
exact SKU IDs for the Gemini or Vertex API charges you want to report, and replace the example SKU
list with those IDs.

co:op uses a fixed query with your chosen project and SKU IDs as parameters. The query includes
their billing credits and accepts only USD billing. It doesn't include unrelated Vertex AI or cloud
services.

If the query is incomplete, rows are missing or the currency is unsupported, co:op shows the report
as unavailable. It doesn't report those missing amounts as zero.

Queries can cost money. Set `maximum_bytes_billed` to limit how much data you can be charged for.
The largest limit co:op allows is 1 GiB. Google rejects a query that exceeds the limit without
charging for that query. co:op requests cached results, but a query may still cost money.

You must create the export and set up its permissions yourself. co:op doesn't create it, change
permissions or enable billing. The report stays partial because the export may lack some usage or
older records, and your SKU list may leave out charges.

## Reading the reports

Provider reports appear in a separate section without an approximation sign. These amounts come
from the provider's usage reports and aren't a final invoice. Each report covers 30 completed UTC
days and excludes the end date. Local estimates use a rolling 30-day window instead.

`Checked` shows when co:op fetched the report, beside the report's dates and scope. The provider may
still report requests late or revise costs. co:op marks reports with excluded usage or missing data
as `partial`. Denied access, a malformed or truncated report and an unsupported currency show the
source as unavailable, never as zero.

A failed source stays visible. If one configured source fails, co:op marks the reported subtotal as
partial too. If none returns a usable report, it shows no reported spending subtotal.

co:op keeps reported API spend separate from local estimates because they can cover the same
requests and measure different things. A provider command such as `coop usage codex` filters the
reports. An account command such as `coop usage codex@personal` doesn't request an organization
report or assign its bill to that login.
