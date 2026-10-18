# Secrets

Secrets give agents credentials for external services without values in prompts or definition files. Open Factory stores each secret encrypted and injects it into a run only when something references it.

## How secrets work

You create a secret with a name, a type and a value; the API never shows the value again. A definition references secrets **by name** only. A secret does nothing until something references it:

| Use | Reference | Effect |
|---|---|---|
| Agent environment | `secrets` in `factory.yaml`, `agentDefaults`, an agent or an automation | An environment variable with the secret's name, in the sandbox and for the harness. |
| MCP server credentials | `{{NAME}}` in an MCP server's `env` or `headers` | Replaced with the value when the run starts. See [{{SECRET}} templating](definition-reference.md#secret-templating). |
| Harness authentication | `harness.auth.secretName` | Mapped to the harness's provider variables. See [Authentication](harnesses.md#authentication). |
| Webhook verification | `secretName` in `webhooks/<name>.yaml` | Verifies inbound deliveries. See [Webhooks](webhooks.md). |
| Machine registry credential | `platform.linux.registryCredentialSecretName` | Ignored by the `kubernetes` backend, where the kubelet pulls images. Use `imagePullSecrets` in the pod template instead. See [Private registries](self-hosting/private-registries.md). |

These uses are independent. For example, an empty `secrets` list does not remove harness authentication or MCP credentials.

## Names, scopes and types

### Names

- Uppercase letters, digits and `_`, not starting with a digit, at most 128 characters. Example: `METABASE_API_KEY`.
- Names that start with `OF_` are reserved for variables that Open Factory sets.
- The definition accepts `^[A-Za-z_][A-Za-z0-9_]{0,127}$`, but the store accepts only uppercase names, so use uppercase names in definitions.

### Scopes

| Scope | How to create it | Visible to |
|---|---|---|
| Team (default) | No `factory_id`. | Every factory of the team. |
| Factory | `factory_id` in the body or the query. | That factory only. |

A run looks in the factory scope first, then the team scope. A factory secret wins over a team secret with the same name.

### Types

| Type | Value | Used for |
|---|---|---|
| `generic` (default) | Any UTF-8 text. | Agent environment, MCP credentials, webhooks. |
| `anthropic_api_key` | One token without whitespace. | Claude Code. |
| `anthropic_oauth_token` | One token without whitespace, made with `claude setup-token`. | Claude Code with a Claude subscription, for your own use only. A shared install should use `anthropic_api_key`. |
| `anthropic_bedrock_api_key` | One token without whitespace. For Claude Code, JSON without whitespace: `{"api_key":"...","region":"us-east-1"}`. | Claude Code through Amazon Bedrock. |
| `anthropic_bedrock_access_key` | JSON with only `access_key_id`, `secret_access_key`, `region` and optional `session_token`. | Claude Code through Amazon Bedrock. |
| `openai_api_key` | One token without whitespace. | Codex. |
| `docker_registry` | JSON with only `registry`, `username` and `password`. | A machine registry credential (ignored by the `kubernetes` backend). |
| `aws_ecr_credential` | JSON. See [Private registries](self-hosting/private-registries.md). | A machine registry credential (ignored by the `kubernetes` backend). |

Every value is required, UTF-8 and without NUL bytes. `NAME=value` plus a terminating NUL byte must fit in 128 KiB (131072 bytes). For larger payloads, use an object store or an external secret manager and store only the access credential here.

## Create a secret

You need the `editor` role.

```bash
echo "mb_..." | openfactory secret set METABASE_API_KEY            # add --factory <factory> for factory scope
```

Or with the API:

```bash
curl -X POST "$OF_PUBLIC_URL/api/v1/secrets" \
  -H "Authorization: Bearer $OF_API_KEY" -H "Content-Type: application/json" \
  -d '{"name": "METABASE_API_KEY", "type": "generic", "value": "mb_..."}'
```

For factory scope, add `"factory_id": "fac_..."`. The response holds the metadata (`id`, `team_id`, `factory_id`, `name`, `type`, `created_at`, `updated_at`), never the value. A second `POST` with the same name in the same scope fails with a conflict; use `PUT` to change the value. `openfactory secret set` creates or updates.

## Grant a secret to agents

Add the name to a `secrets` list and apply the definition. Never commit the value.

```markdown
---
# agents/reporting/agent.md
description: Answers questions about dashboard metrics
secrets:
  - METABASE_API_KEY
---

Use the read-only Metabase credential to query dashboard metrics.
Never print credentials or include them in outputs.
```

| List | Effect |
|---|---|
| `secrets` in `factory.yaml` | Added to every agent. No agent can remove it. |
| `agentDefaults.secrets` | The default list for agents without their own `secrets`. |
| `secrets` in an agent | Replaces the default list. `secrets: []` removes the defaults but not the factory-wide secrets. |
| `secrets` in an automation | Added to the target agent's list for that automation's runs. |

`run_agents` cannot add secrets to a child run. The full rules are in [Secrets and MCP servers across levels](definition-reference.md#secrets-and-mcp-servers-across-levels).

To verify, start a new run and ask the agent to confirm that the variable is set (without printing it), then to make a permitted read-only request with it.

## Rotate a secret

Replace the value and keep the name, so references keep working:

```bash
curl -X PUT "$OF_PUBLIC_URL/api/v1/secrets/METABASE_API_KEY" \
  -H "Authorization: Bearer $OF_API_KEY" -H "Content-Type: application/json" \
  -d '{"value": "mb_new..."}'
```

- For factory scope, add `"factory_id"` to the body or `?factory_id=` to the URL.
- `type` is optional; without it, the type stays.
- New runs read the new value. This includes integration record tokens: the server drops its cached forge clients when a secret or an integration record changes.
- Check that the new credential works before you revoke the old one.

## List, read and delete secrets

| Request | CLI | Purpose |
|---|---|---|
| `GET /api/v1/secrets` | `openfactory secret list` | List metadata. Add `?factory_id=` for a factory's secrets. |
| `GET /api/v1/secrets/{name}` | | One secret's metadata. |
| `DELETE /api/v1/secrets/{name}` | `openfactory secret delete` | Delete a secret (`editor` role). |

> **Caution:** Deletion is permanent. Every agent, MCP server, machine, harness and webhook that references the secret stops working. Remove or replace the references first.

## Protection

- Each value is encrypted with its own data key under the master key `OF_SECRET_KEY`. See [Secrets at rest](security.md#secrets-at-rest).
- If the master key changes without a rotation, stored secrets cannot be decrypted. Back up the key, and see [Key rotation](self-hosting/configuration.md#key-rotation).

## Troubleshooting

| Problem | Cause and fix |
|---|---|
| The run fails with `secret "<NAME>": ...` | The secret is not in the factory or team scope. Create it or fix the name. |
| The run log says `secret name is not a usable environment variable; skipped` | The name is not a valid environment variable name, or starts with `OF_METADATA_`. Rename the secret. |
| The run fails with `unknown secret(s) referenced: <names>` | An MCP server `env` or `headers` value references a missing secret. |
| Create fails with `must use uppercase letters, digits and underscores` | Use an uppercase name. |
| Create fails with `uses the reserved prefix OF_` | Choose a name without the `OF_` prefix. |
| Create fails with `too large` | `NAME=value` exceeds 128 KiB. Store the payload elsewhere. |
| The variable is set, but authentication fails | Check the credential's permissions and expiry in the external service. |
