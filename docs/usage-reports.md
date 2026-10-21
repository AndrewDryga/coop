# Usage estimates and provider reports

`coop usage` shows account limits first, then estimates what your recorded activity would cost
at API prices. It reads Coop histories and supported native CLI histories on this computer.
Browser and mobile conversations, and histories on other computers, are not included. Deleted
histories and unknown model prices can leave gaps.

| Provider | Default history home | Override |
|---|---|---|
| Claude | `~/.claude` | `CLAUDE_CONFIG_DIR` |
| Codex | `~/.codex` | `CODEX_HOME` |
| Gemini | `~/.gemini` | `GEMINI_CLI_HOME/.gemini` |
| Grok | `~/.grok` | `GROK_HOME` |

If you set an override, Coop scans both it and the default location when they differ. It does
not change either folder or its sign-ins, and it counts copied requests only once.

Coop assigns a request to an account only when it finds the request in exactly one Coop account.
Other native history appears as `Unassigned local usage`. Signing in today cannot establish
which account made an old request. Shared editor histories appear as `Unassigned editor usage`.
If copies point to conflicting accounts, Coop leaves the request unassigned instead of assigning
it to your currently selected account.

Provider subtotals and Total count unassigned history once. `≈$.../30d` estimates the activity's
value at standard API prices, including activity covered by a subscription. It does not show
what you paid. Use `coop usage codex@personal` (or another provider/account) to see which history
and prices the estimate includes.

## Connect an API billing source

To enable provider reports, create `~/.config/coop/usage-reports.json`. If you set the XDG config
home, use `$XDG_CONFIG_HOME/coop/usage-reports.json` instead. The file must be a regular file,
not a symlink, and can define one reporting source per provider. `name` labels the source;
it does not select a Coop account.

Coop reads this file for `coop usage` and provider summaries. It does not read it when you
inspect a specific account.

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

Set the named environment variables in the host shell where you run Coop. Do not put the keys
in this JSON file, your project or `agents/env`, which is forwarded into boxes. Reporting keys
can have broader permissions than model API keys, so grant only the access needed for reports.

Coop keeps these keys out of agent homes and containers. Its error messages include neither the
key nor the provider's response body.

- OpenAI: use an organization Admin API key. `organization` selects the organization sent to the
  [Costs API](https://developers.openai.com/api/reference/resources/admin/subresources/organization/subresources/usage/methods/costs).
- Anthropic: use an organization Admin API key. The key determines the organization. Its
  [cost report](https://platform.claude.com/docs/en/manage-claude/usage-cost-api) excludes Priority
  Tier charges, so this source is marked partial.
- xAI: use a separate Management API key authorized to read the configured team's
  [billing usage](https://docs.x.ai/developers/rest-api-reference/management/billing), not an inference
  key or prepaid balance.

These sources report API usage for an organization or team. That can include other applications
and devices in the same organization or team. Personal subscriptions and ChatGPT or Claude web
activity are not included.

## Google billing exports

For Google reports, set up a [standard Cloud Billing export](https://docs.cloud.google.com/billing/docs/how-to/export-data-bigquery).
The host's `gcloud` account needs access to that export and permission to create BigQuery query
jobs in the selected project. If the configured email is not signed in, use `gcloud auth login`.
This sign-in is separate from your Gemini CLI sign-in.

Coop obtains a fresh short-lived access token through
`gcloud auth print-access-token <email> --quiet`. It does not copy a Google refresh credential.

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

In your billing export, find the exact SKU IDs for the Gemini or Vertex API charges you want
to report. A SKU identifies a billing item. Replace the example SKU list with those IDs.

Coop uses a fixed query with your chosen project and SKU IDs as parameters. It includes their
billing credits and accepts only USD billing. It does not include unrelated Vertex AI or
cloud services.

If the query is incomplete, rows are missing or the currency is unsupported, Coop shows the
report as unavailable. It does not report those missing amounts as zero.

Queries can cost money. Set `maximum_bytes_billed` to limit how much data you can be charged
for. The largest limit Coop allows is 1 GiB. Google rejects a query that exceeds the limit
without charging for that query. Coop requests cached results, but a query may still cost money.

You must create the export and arrange its permissions yourself. Coop does not create it,
change permissions or enable billing. The report remains partial because the export may
lack some usage or older records, and your SKU list may leave out charges.

## Reading the reports

Provider reports appear in a separate section without an approximation sign. These amounts come
from the provider's usage reports; they are not a final invoice. Each report covers 30 completed
UTC days and excludes the end date. Local estimates instead use a rolling 30-day window.

`Checked` shows when Coop fetched the report. The provider may still report requests late or
revise costs. Reports with excluded usage or missing data are marked `partial`.

A failed source stays visible. If one configured source fails, the reported subtotal is marked
partial too. If none returns a usable report, Coop shows no reported spending subtotal.

Coop keeps reported API spend separate from local estimates because they can cover the same
requests and measure different things. A provider command such as `coop usage codex` filters the
reports. An account command such as `coop usage codex@personal` neither requests an organization
report nor assigns its bill to that login.
