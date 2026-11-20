# Custom webhooks

A custom webhook lets any system that can send an HTTP `POST` with a JSON body start factory work.

## How custom webhooks work

A webhook is a factory resource with a name, an authentication mode, a secret, and an ingress URL that contains the webhook UID:

```text
${OF_PUBLIC_URL}/webhooks/w/{uid}            token and signature modes
${OF_PUBLIC_URL}/webhooks/w/{uid}/{token}    url_token mode
```

1. The sender posts JSON to the ingress URL.
2. Open Factory authenticates the request, checks the [delivery rules](#delivery-rules) and records the delivery.
3. Each enabled automation with a matching `provider: webhook` trigger starts a run. Without such an automation, the webhook starts nothing.

## Authentication modes

| Mode | `authMode` | How the sender authenticates | Use it for |
| --- | --- | --- | --- |
| Bearer token (default) | `token` | Sends the secret in an `Authorization: Bearer <secret>` header. | CI jobs, scripts, alerting tools and other senders that can set headers. |
| URL token | `url_token` | Posts to a URL that contains the secret as the last path segment. | Senders that accept only a URL. |
| Provider signature | `signature` | Signs each request. Open Factory verifies the signature with the signing secret. | GitHub, PagerDuty, Sentry, Stripe, Vercel and senders that implement Standard Webhooks. |

A `url_token` URL is a credential. Keep it secret, and rotate it if it leaks. Its secret can contain only letters, digits, `-`, `.`, `_` and `~`.

### Signature schemes

| `signatureScheme` | Signature header | Algorithm | Delivery ID header |
| --- | --- | --- | --- |
| `github` | `X-Hub-Signature-256: sha256=<hex>` | HMAC-SHA256 of the body | `X-GitHub-Delivery` |
| `pagerduty` | `X-PagerDuty-Signature: v1=<hex>` (several comma-separated values during rotation) | HMAC-SHA256 of the body | `X-Webhook-Id` |
| `sentry` | `Sentry-Hook-Signature: <hex>` | HMAC-SHA256 of the body | `Request-ID` |
| `standard_webhooks` | `Webhook-Signature: v1,<base64>` with `Webhook-Id` and `Webhook-Timestamp` (`Svix-*` headers are also accepted) | HMAC-SHA256 of `id.timestamp.body` | `Webhook-Id`, then `Svix-Id` |
| `stripe` | `Stripe-Signature: t=<timestamp>,v1=<hex>` | HMAC-SHA256 of `timestamp.body` | None |
| `vercel` | `X-Vercel-Signature: <hex>` | HMAC-SHA1 of the body | None |

For `stripe` and `standard_webhooks`, the timestamp must be within 5 minutes of the server time. For `standard_webhooks`, a `whsec_<base64>` secret is decoded from base64.

## Create a webhook

Create a webhook through the API or the CLI, or as a file in the definition tree. The API and the CLI write the file for you, so they work only for factories with a `managed` definition.

### Create a webhook with the CLI or the API

```bash
openfactory webhook create "$FACTORY_UID" --name internal-ci --auth-mode token
```

The same request through the API:

```bash
curl -X POST "$OF_PUBLIC_URL/api/v1/factories/$FACTORY_UID/webhooks" \
  -H "Authorization: Bearer $OF_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"name": "internal-ci", "auth_mode": "token"}'
```

| Field | Required | Description |
| --- | --- | --- |
| `name` | Yes | 1-63 lowercase letters, digits, `-` or `_`. It becomes `webhooks/<name>.yaml`. |
| `auth_mode` | No | `token` (default), `url_token` or `signature`. |
| `signature_scheme` | For `signature` | One of the schemes in the table above. |
| `secret_name` | No | The team secret that holds the credential. Default: `<NAME>_WEBHOOK_SECRET` in uppercase, with `-` and `.` changed to `_`. The server writes a new value to it and the response shows the value, with these exceptions. If you name an existing secret, or another webhook of the team uses it, the webhook uses it unchanged: the response does not show its value and has `secret_reused: true`. A `signature` webhook whose provider issues the secret uses an existing secret unchanged. If the definition does not apply, the server restores the secret to its previous state. |
| `secret` | No | The provider's signing secret, for `signature` mode only. |
| `delivery_id_header` | No | A header that the sender fills with its own event ID. |
| `enabled` | No | `true` (default) or `false`. |

The response contains `webhook` (with its `id`, the UID), `ingress_url` and `secret`:

- For `token` and `url_token`, Open Factory generates a `whsec_` secret. For `url_token`, `ingress_url` already contains the token.
- For `standard_webhooks`, if you do not send `secret`, Open Factory generates a signing secret. Give it to the sender.
- For `github` and `sentry`, send the provider's signing secret in `secret`.
- For `vercel`, `stripe` and `pagerduty`, the provider issues the secret only after it has the URL. Without `secret`, the webhook is created disabled with `"awaiting_secret": true`. Then [add a late signing secret](#add-a-late-signing-secret).

Copy the secret now: the API shows a generated secret only once. Later, a `url_token` `ingress_url` shows a `{token}` placeholder.

### Add a late signing secret

1. Create the webhook without `secret`. Copy `ingress_url`.
2. Enter the ingress URL in the provider's webhook settings.
3. Copy the signing secret that the provider shows.
4. Send the secret to Open Factory. This also enables the webhook.

```bash
curl -X PATCH "$OF_PUBLIC_URL/api/v1/webhooks/$WEBHOOK_UID" \
  -H "Authorization: Bearer $OF_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"secret": "PROVIDER_SIGNING_SECRET"}'
```

### Create a webhook in the definition tree

For a factory with a `git` definition, add a file `webhooks/<name>.yaml`. The `<name>` must be 1-64 letters, digits, `.`, `_` or `-`, and must start with a letter or digit.

```yaml
# webhooks/sentry-alerts.yaml
authMode: signature
signatureScheme: sentry
secretName: SENTRY_WEBHOOK_SECRET
```

`secretName` is required. `authMode` (default `token`), `signatureScheme` (required for `signature`, not valid otherwise), `deliveryIdHeader` (1-64 characters, not a credential or signature header) and `enabled` are optional. All rules are in the [webhook field table](definition-reference.md#webhooksnameyaml).

Create the secret before the first delivery, for example with `openfactory secret set SENTRY_WEBHOOK_SECRET` (see [Secrets](secrets.md)). If the secret does not exist, deliveries get `401`.

Open Factory assigns the UID when the file first applies. To get the UID and the ingress URL, run `openfactory webhook list "$FACTORY_UID"` or call `GET /api/v1/factories/{uid}/webhooks`.

Complete example: [`../examples/05-incident-response`](../examples/05-incident-response).

## Configure the sender

Bearer token:

```bash
curl -X POST "$OF_PUBLIC_URL/webhooks/w/$WEBHOOK_UID" \
  -H "Authorization: Bearer $WEBHOOK_SECRET" \
  -H "Content-Type: application/json" \
  -d '{"event": "deploy.finished", "env": "production", "service": "payments"}'
```

URL token:

```bash
curl -X POST "$OF_PUBLIC_URL/webhooks/w/$WEBHOOK_UID/$WEBHOOK_SECRET" \
  -H "Content-Type: application/json" \
  -d '{"event": "deploy.finished", "env": "production"}'
```

Provider signature: enter the ingress URL in the provider's webhook settings, then send a test event from the provider. An unsigned `curl` request gets `401`.

An accepted delivery gets `202` with `{"delivery_id": "sha256:4f2c...", "status": "accepted"}`.

The sender must reach the ingress URL, so set `OF_PUBLIC_URL` to the public address of the server. See [Server configuration](self-hosting/configuration.md).

## Add a webhook trigger to an automation

Subscribe an automation with a `provider: webhook`, `event: received` trigger whose filter names the webhook in `webhook_ids` and can match the body with `payload`. See [Custom webhooks (`webhook`)](automations.md#custom-webhooks-webhook) and [Payload patterns](automations.md#payload-patterns). The [Vercel example](#example-vercel-deployment-failures) shows a complete automation.

## Test a filter

Test a trigger filter against a stored delivery:

```bash
curl -X POST "$OF_PUBLIC_URL/api/v1/webhooks/$WEBHOOK_UID/test-filter" \
  -H "Authorization: Bearer $OF_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"filter": {"webhook_ids": ["'"$WEBHOOK_UID"'"], "payload": {"env": ["production"]}}}'
```

| Field | Description |
| --- | --- |
| `filter` | A webhook trigger filter with `webhook_ids` and `payload`. In this request, use the webhook UID in `webhook_ids`. |
| `pattern` | A payload pattern without `webhook_ids`. It replaces `filter.payload`. |
| `delivery_id` | The record `id` of a stored delivery. The default is the newest accepted delivery. |
| `body` | A JSON body to test instead of a stored delivery. |

The response has `result` (`Would trigger` or `Would not trigger`), `would_trigger`, `webhook_check` and `payload_check`. Each check has `passed` and `detail`. A disabled webhook never triggers.

## What the run receives

A run started by a delivery gets the automation prompt and a summary of the event: the provider (`webhook`), the event (`received`), the webhook name and the delivery identity. The full body is in `.openfactory/events/<message_id>/payload.json` (a delivery is at most 256 KiB, so it is never cut). See [What the agent gets from the event](automations.md#what-the-agent-gets-from-the-event).

Say in the prompt which fields of the body matter. For example: "Read `build.status` and `build.id` from the payload file. If the status is `failed`, find the cause in the logs of that build."

Each accepted delivery starts new runs or nothing. Deliveries do not continue earlier work items, and deliveries have no order.

## Delivery rules

| Rule | Behavior |
| --- | --- |
| Method | `POST` only. |
| Body | Valid JSON (object, array or scalar). `Content-Type` is not checked. |
| Size | At most 256 KiB. |
| Rate limit | 60 deliveries per minute per webhook, counted after authentication. The database keeps the count, so the limit holds across replicas. If the database cannot count, each replica counts on its own. |
| Failed authentication | A separate budget of 60 failed attempts per minute per webhook. When it is used up, all requests get `429` until the window resets. Each replica counts this budget on its own. |
| Webhooks per factory | 20. |

Responses:

| Status | Meaning |
| --- | --- |
| `202` `{"status": "accepted"}` | The delivery was accepted for evaluation. It does not mean that an automation matched. |
| `202` `{"status": "duplicate"}` | The delivery identity was already accepted. Nothing starts. |
| `400` | The body is not valid JSON, or it cannot be read. |
| `401` | Authentication failed, the URL form does not match the mode, or the secret does not exist. |
| `404` | The webhook is unknown, disabled or deleted. |
| `413` | The body is larger than 256 KiB. |
| `429` | A rate limit was exceeded. The `Retry-After` header gives the wait in seconds. |
| `500` | The server cannot read the secret or record the delivery. |

### Delivery identity and deduplication

Open Factory gives each delivery an identity. It uses the first value it finds, in this order:

1. The header named in `deliveryIdHeader`.
2. The provider's delivery header, for `signature` webhooks (see [Signature schemes](#signature-schemes)).
3. The `X-OF-Delivery-Id` header, which any sender can set.
4. `sha256:<hex>` of the body.

Header values are cut to 200 characters. A delivery with the identity of an earlier accepted delivery is a duplicate and starts nothing, so provider retries do not start duplicate runs. Without a delivery ID, two identical bodies are duplicates.

## Manage webhooks

| Action | Request |
| --- | --- |
| List webhooks | `GET /api/v1/factories/{uid}/webhooks` |
| Get a webhook | `GET /api/v1/webhooks/{id}` |
| Enable or disable | `PATCH /api/v1/webhooks/{id}` with `{"enabled": false}` |
| Change the delivery ID header | `PATCH /api/v1/webhooks/{id}` with `{"delivery_id_header": "X-Event-Id"}`. An empty string removes it. |
| Rotate the secret | `POST /api/v1/webhooks/{id}/rotate-secret` |
| Delete | `DELETE /api/v1/webhooks/{id}` |
| List deliveries | `GET /api/v1/webhooks/{id}/deliveries?limit=50` (maximum 200), or `openfactory webhook deliveries <webhook-id>` |
| Get one delivery | `GET /api/v1/webhooks/{id}/deliveries/{deliveryId}` |

Notes:

- The name, `auth_mode` and `signature_scheme` cannot change. Create a new webhook instead.
- A disabled or deleted webhook returns `404` to senders. Automations that select a deleted webhook stop matching.
- `rotate-secret` generates and shows once a new secret for `token`, `url_token` and `standard_webhooks`. For other schemes, send the provider's new secret in `{"secret": "..."}`. The old secret stops working at once. For a rollover with no downtime, create a second webhook, move the sender to it, then delete the first.
- These calls change the definition, so they work only for `managed` factories. For a `git` factory, change `webhooks/<name>.yaml` in the repository, and the secret value with `PUT /api/v1/secrets/{name}`.
- The ingress reads the secret on each delivery, so a new value applies to the next delivery.

### Delivery log

Each delivery is recorded, also when it starts nothing. A record has `id`, `delivery_id` (the identity), `status`, `reason`, `headers`, `body`, `run_ids` and `received_at`. The API adds `label` and `size`.

| `status` | `label` | Recorded data |
| --- | --- | --- |
| `accepted` | `Accepted` | Headers, body, identity, and the IDs of the runs that started. |
| `deduplicated` | `Duplicate` | Headers and identity. No body. |
| `rejected` | `Rejected (auth)`, `Rejected (invalid JSON)`, `Rejected (too large)`, `Rejected (rate limited)` or `Rejected (secret unavailable)` | Headers and `reason` only. No body and no identity. |

Rejection reasons are `auth`, `invalid_json`, `too_large`, `rate_limited` and `secret_unavailable`.

### Delivery retention

A sweep runs every hour on the leader replica. For each webhook:

- The newest 50 deliveries stay whole.
- Older deliveries lose their headers and body, but keep the identity, status and run IDs, so a retry is still a duplicate.
- Deliveries older than 7 days are deleted. After that, a retry with the same delivery ID counts as new.

These limits have no setting. [Test a filter](#test-a-filter) before a delivery is stripped, or send the body yourself.

The log never stores credentials. It drops the `Authorization`, `Proxy-Authorization`, `Cookie`, `Set-Cookie`, `X-Api-Key` and `Api-Key` headers, every signature header, and every header whose name contains `signature`, `secret`, `token`, `password`, `api-key`, `apikey`, `credential` or `authorization`.

## Example: Vercel deployment failures

1. Create the webhook without a secret:

   ```bash
   openfactory webhook create "$FACTORY_UID" --name vercel-deploys \
     --auth-mode signature --signature-scheme vercel
   ```

2. In Vercel, add a webhook with the `ingress_url` and select the deployment events.
3. Copy the signing secret that Vercel shows, then send it with `PATCH /api/v1/webhooks/{id}` as in [Add a late signing secret](#add-a-late-signing-secret).
4. Add an automation:

   ```markdown
   ---
   triggers:
     - provider: webhook
       event: received
       filter:
         webhook_ids: [vercel-deploys]
         payload:
           type: [deployment.error]
           payload:
             target: [production]
   ---

   A production deployment failed on Vercel. Find the failing commit,
   read the build output, and open a draft pull request with a fix.
   ```

Stripe and PagerDuty use the same procedure.

## Troubleshooting

| Problem | Check |
| --- | --- |
| The sender gets `401` | The credential does not match the mode: `token` needs the `Authorization: Bearer` header, `url_token` the token in the URL, `signature` the current signing secret. The secret in `secretName` must exist. Stripe and Standard Webhooks signatures older than 5 minutes are rejected. |
| The sender gets `404` | The webhook is disabled or deleted, or the UID in the URL is wrong. |
| The sender gets `202` but no run starts | Read the delivery in the delivery log. A `Duplicate` delivery reused an identity. For an `Accepted` delivery, make sure that an enabled automation selects the webhook, then [test the filter](#test-a-filter) against that delivery. |
| The sender gets `429` | The webhook exceeded 60 deliveries per minute, or 60 failed authentications per minute. Wait for the time in `Retry-After`. |
| A definition does not apply | Check `webhooks/<name>.yaml` against the field table. `signatureScheme` is required for `signature` and not valid for other modes. |
