# Usage estimates and provider reports

`coop usage` keeps account limits first, then estimates the API value of recorded activity.
It reads Coop histories and the supported native CLI histories on this computer. It does not
read browser/mobile conversations or histories on other computers. Deleted histories and unknown
model prices can leave gaps.

| Provider | Default history home | Override |
|---|---|---|
| Claude | `~/.claude` | `CLAUDE_CONFIG_DIR` |
| Codex | `~/.codex` | `CODEX_HOME` |
| Gemini | `~/.gemini` | `GEMINI_CLI_HOME/.gemini` |
| Grok | `~/.grok` | `GROK_HOME` |

Coop scans the default and the override when they differ. It never changes those homes or their
sign-ins. Copied requests count once. A request found in exactly one Coop account keeps that
account's attribution. Native history without that evidence appears as `Unassigned local usage`;
signing in today does not identify the account that made an old request. Shared editor histories
remain `Unassigned editor usage`. Conflicting account copies stay unassigned rather than being
charged to whichever account is selected now.

Provider subtotals and Total include unassigned history once. `≈$.../30d` is an estimate at standard
API prices, including subscription activity, not what you paid. Select `coop usage codex@personal`
(or another provider/account) for coverage and pricing details.

## Connect an API billing source

Reporting is off unless you create `~/.config/coop/usage-reports.json`, or
`$XDG_CONFIG_HOME/coop/usage-reports.json` when that config home is set. The file must be a regular
file, not a symlink, and can contain one source per provider. `name` is a label, not a Coop account.
Coop reads the file only for `coop usage` and provider summaries, not exact-account inspection.

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

Set the referenced variables in the host shell where you run Coop. Keep the keys out of this JSON
file, your project and `agents/env`; that last file is forwarded into boxes. These keys can have
broader permissions than a model API key, so grant only the access reporting needs. Coop does not
copy them into an agent home or container, and its errors never include a key or provider body.

- OpenAI: use an organization Admin API key. `organization` selects the organization sent to the
  [Costs API](https://developers.openai.com/api/reference/resources/admin/subresources/organization/subresources/usage/methods/costs).
- Anthropic: use an organization Admin API key. The key determines the organization. Its
  [cost report](https://platform.claude.com/docs/en/manage-claude/usage-cost-api) excludes Priority
  Tier charges, so this source is marked partial.
- xAI: use a separate Management API key authorized to read the configured team's
  [billing usage](https://docs.x.ai/developers/rest-api-reference/management/billing), not an inference
  key or prepaid balance.

These sources report API organizations/teams, not personal subscriptions or ChatGPT/Claude web
activity. They can include API usage from other applications and devices within that scope.

## Google billing exports

Google needs a [standard Cloud Billing export](https://docs.cloud.google.com/billing/docs/how-to/export-data-bigquery)
and host `gcloud` credentials with access to the export and permission to create BigQuery query jobs
in the selected project. Use `gcloud auth login` for the configured email if it is not signed in.
This is separate from your Gemini CLI sign-in. Coop refreshes the short-lived access token through
`gcloud auth print-access-token <email> --quiet`; it does not copy a Google refresh credential.

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

Replace the SKU list with the exact Gemini/Vertex API SKU IDs from your export. Coop never counts
all Vertex AI or all cloud services as Gemini spend. The fixed, parameterized query reads only the
selected project/SKUs and includes their billing credits. It accepts USD billing only. Missing
rows, incomplete queries and unsupported currencies show unavailable, not zero.

BigQuery queries can incur charges. `maximum_bytes_billed` is required and capped at 1 GiB; Google
rejects a query that exceeds it without charging for that query. Coop asks for cached results but
does not promise a free query. It never creates the export, changes permissions or enables billing.
The source stays partial because export coverage/backfill and unlisted SKU IDs can leave gaps.

## Reading the reports

Provider reports appear in their own section. Amounts have no approximation sign, but they remain
provider-reported usage costs, not a final invoice. The dates cover 30 completed UTC days; the end
date is excluded. This differs from the local estimate's rolling 30-day window.

`Checked` is when Coop fetched the report, not proof that the provider has reported every request.
Providers can report late or revise costs. Scope exclusions and gaps are marked `partial`.
Failed sources stay visible, and a reported subtotal becomes partial if another configured source
failed. No usable report means no reported money subtotal.

Reported API spend is never added to local estimates: the records can overlap and measure different
things. Provider selection, such as `coop usage codex`, filters reports. Selecting
`coop usage codex@personal` does not request any organization report or assign its bill to that login.
