# 05 - Incident response

A factory that starts investigations from PagerDuty and Sentry webhooks. It
reads Sentry and Datadog through MCP servers and reports in Slack.

## What it shows

### Webhooks

| File | Auth | Notes |
| --- | --- | --- |
| `webhooks/pagerduty-incidents.yaml` | `signature` / `pagerduty` | Verifies `X-PagerDuty-Signature`. Deduplicates on `X-Webhook-Id`. |
| `webhooks/sentry-alerts.yaml` | `signature` / `sentry` | Verifies `Sentry-Hook-Signature`. |

Each webhook gets the ingress URL `${OF_PUBLIC_URL}/webhooks/w/<WEBHOOK_UID>`.

### Triggers

- `webhook_ids` takes exactly one webhook for each trigger. The value is the
  webhook UID or its name (the file name without `.yaml`). So one change can
  add a webhook and the automation that listens to it.
- The `payload` filter matches the JSON body of the delivery. A list gives
  the allowed values, a nested object reaches nested fields, and `in`,
  `not_in` and `exists` state explicit conditions.

### Access

- Factory-wide `mcpServers.sentry` uses a server from the team MCP registry
  (`managedId`). Its OAuth connection is completed before the runs start.
- The responder reaches the `datadog` MCP server by URL. Its headers reference
  managed secrets as `{{SECRET_NAME}}`, so credentials never appear in the
  definition.
- The responder's `networkPolicy` adds Datadog hosts to the factory policy.
  The factory denies `pastebin.com`, and no agent can allow it again.

## Prerequisites

The server rejects a definition that names a missing secret or MCP server.
Create them first.

1. A running server, with `OF_SERVER`, `OF_API_KEY` and `OF_PUBLIC_URL` set
   (see [Quickstart](../../docs/quickstart.md)). PagerDuty and Sentry must
   reach `OF_PUBLIC_URL`.
2. The Slack integration (see [Slack](../../docs/integrations/slack.md)).
3. A Sentry server in the team MCP registry, connected with OAuth
   (`openfactory mcp-registry add` and `openfactory mcp-registry connect`).
   Set `mcpServers.sentry.managedId` in `factory.yaml` to its name.
4. The managed secrets:

   ```bash
   openfactory secret set ANTHROPIC_API_KEY --type anthropic_api_key
   openfactory secret set DATADOG_API_KEY
   openfactory secret set DATADOG_APP_KEY
   openfactory secret set PAGERDUTY_WEBHOOK_SECRET
   openfactory secret set SENTRY_WEBHOOK_SECRET
   ```

PagerDuty issues its signing secret only after it has the URL. For PagerDuty,
do these steps:

1. Store a temporary value in `PAGERDUTY_WEBHOOK_SECRET`.
2. Add `enabled: false` to `webhooks/pagerduty-incidents.yaml` and apply.
3. Get the webhook ID with `openfactory webhook list oncall`. Register
   `${OF_PUBLIC_URL}/webhooks/w/<ID>` in PagerDuty.
4. Store the real signing secret with `openfactory secret set`.
5. Remove `enabled: false` and apply again.

## Apply

1. In `factory.yaml`, under `repositories`, change `owner: acme` and
   `name: payments-service` to your repository.
2. Validate and apply:

   ```bash
   openfactory validate examples/05-incident-response
   openfactory factory apply examples/05-incident-response
   ```

## What to expect

`validate` reports 2 agents, 2 automations and 2 webhooks. `apply` creates the
factory `@oncall`. A triggered PagerDuty incident for `payments-service`, or a
new fatal Sentry issue outside the `sandbox` and `staging` projects, starts a
run. The foreman reports in Slack. For Sentry issues, the responder opens a
draft pull request with a fix and a regression test.
