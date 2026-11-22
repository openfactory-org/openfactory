# Integrations

Integrations connect a factory to code forges, chat tools and issue trackers, where work starts and where the factory replies.

| Provider | Page | Starts work from | Replies to |
| --- | --- | --- | --- |
| GitHub | [github.md](github.md) | Issues, pull requests, reviews, pushes, CI results, mentions | Issue and pull request comments |
| GitLab (plugin) | [gitlab.md](gitlab.md) | Merge requests, pushes, bot mentions | Merge request and issue comments |
| Azure DevOps (plugin) | [azure-devops.md](azure-devops.md) | Pull requests, work items, mentions | Pull request threads, work item comments |
| Other code forges | [other-forges.md](other-forges.md) | Custom webhooks only | None |
| Slack | [slack.md](slack.md) | Mentions, messages, direct messages, reactions | Threads, with approval buttons |
| Microsoft Teams | [teams.md](teams.md) | Channel mentions and messages | Channel threads |
| Linear | [linear.md](linear.md) | Issues, comments, agent sessions | Issue comments, agent activities |
| Jira | [jira.md](jira.md) | Issues, status changes, agent sessions | Issue comments |

GitLab and Azure DevOps are plugin Workers that the control plane calls over service bindings (see [Plugins](../self-hosting/plugins.md)). The others are built in. Work can also start from [custom webhooks](../webhooks.md), [schedules](../automations.md#schedules-schedule), the [REST API](../api.md) and [Factory MCP](../factory-mcp.md).

## Common setup

Each setup page follows these steps:

1. Create the app, bot or token at the provider. Point its webhook at the [inbound endpoint](#inbound-endpoints).
2. Store each credential as a team secret.
3. Create an [integration record](#integration-records) that names the secrets.
4. Declare the integration in `factory.yaml`: `repositories` (and `codeForges`) for a forge, `integrations[].type` (`slack`, `microsoft-teams`, `linear` or `jira`) for the others. Each type can appear one time, and `linear` and `jira` cannot both appear. Then add automations.

The examples use `OF_PUBLIC_URL` (the public server URL, which the provider must reach; see [Server configuration](../self-hosting/configuration.md)) and `OF_API_KEY` (an admin API key). `openfactory secret set` reads `OF_SERVER` and `OF_API_KEY`, and takes the value from its argument or stdin:

```bash
openfactory secret set SLACK_SIGNING_SECRET
openfactory secret set GITHUB_APP_PRIVATE_KEY < app.private-key.pem
```

## Integration records

A record belongs to the team and holds the credentials (as secret names) and settings of one connection. Admins manage records on the **Integrations** page of the dashboard or with `POST`, `GET`, `PATCH` and `DELETE` on `/api/v1/integrations[/{id}]`. A `PATCH` with `config` replaces the whole map. `GET /api/v1/factories/{uid}/integrations` shows what a factory declares.

| Field | Description |
| --- | --- |
| `provider` | `github`, `gitlab`, `azure_devops`, `slack`, `microsoft_teams`, `linear` or `jira`. It cannot change. |
| `name` | A display name. The default is the provider. |
| `secret_ref` | The team secret that holds the main credential (token, API key or private key). |
| `config` | String settings. Keys that end in `_ref` name other team secrets. Never put a secret value here. See [Secrets](../secrets.md). |
| `factory_id` | Optional. Binds the record to one factory. Set only at creation. The old key `config.factory_id` is not read. |
| `status` | `active` (also when empty), `disabled` or `auth_failed`. You can set `active` or `disabled`. See [Status](#status). |

### Record selection

A record serves one owner or workspace, named by a necessary `config` key:

| Provider | Key | Value |
| --- | --- | --- |
| `github`, `gitlab`, `azure_devops` | `config.org` | The organization or user (for GitLab, the top-level group), without a slash. A credential is also necessary. |
| `slack` | `config.team_id` | The Slack workspace ID (`T…`). |
| `microsoft_teams` | `config.team_id` | The Microsoft Graph team ID. |
| `linear` | `config.organization_id` | The Linear workspace ID. |
| `jira` | `config.site` | The site host name (`acme.atlassian.net`) or its cloud ID. |

A record without the key, or a forge record without a credential, gets `400`. A team has one record for each provider, owner or workspace, and factory: a second one gets `409`. A record without `factory_id` is the team record.

For each action (a run on each repository, a comment, a check, a tracker update, a definition sync, webhook intake), the server selects a record:

1. It takes the team records of the provider whose key matches the repository owner or the event workspace, in any letter case.
2. Outbound actions use the record of the factory, else the team record. A run never uses the record of another factory.
3. Incoming events use the team record, else the one factory record when there is exactly one. The event then belongs to that factory. For Teams, only records whose `app_id` is the bot that got the event count. For Slack, when records set `api_app_id` to the app of the event, only those records count.
4. If no record is left, the action fails with an error that names the owner or workspace. The server never falls back to another owner or to "the first record".
5. If the record is not active, the action fails with `integration_disabled`. The server does not try another record.

Example: with the GitHub records `int_team` (`org: acme`, no `factory_id`) and `int_pay` (`org: acme`, factory `payments`), the factory `payments` uses `int_pay` for `acme/api`, and each other factory uses `int_team`. Every factory fails for `other/x` with `no github integration record for the owner "other"` until a record has `"org": "other"`.

### Credential isolation between factories

Use one record for each factory when factories must not reach the repositories of each other:

- **GitHub App.** Each installation token is requested for the repositories of the run only, also with the team record.
- **Static token** (GitHub personal access token, GitLab, Azure DevOps, generic git). The server gives the token only to runs on the factory repositories, but cannot limit what the token reaches at the provider. Give each factory a record with a token that reaches only its repositories.

### Status

The **Integrations** page shows each status and refreshes every 30 seconds.

| Status | Set by |
| --- | --- |
| `active` | The default. An admin, or the provider when the app is installed again. |
| `disabled` | An admin, or a provider lifecycle webhook (GitHub: the app was removed or suspended). |
| `auth_failed` | The server, when the provider refuses the credential (removed installation, bad app key, revoked token). |

A run that needs a record that is not active fails at once with `integration_disabled`. Correct the cause, then send `PATCH /api/v1/integrations/{id}` with `{"status": "active"}`.

## Inbound endpoints

| Provider | Endpoint (after `${OF_PUBLIC_URL}`) | Authentication |
| --- | --- | --- |
| GitHub | `POST /integrations/github/webhook` | `X-Hub-Signature-256` HMAC |
| GitLab | `POST /plugins/gitlab/webhook` | `X-Gitlab-Token` secret token |
| Azure DevOps | `POST /plugins/azuredevops/webhook` | HTTP basic authentication |
| Slack | `POST /integrations/slack/events`, `POST /integrations/slack/interactions` | `X-Slack-Signature` HMAC, 5-minute timestamp window |
| Microsoft Teams | `POST /integrations/teams/messages` | Bot Framework JWT |
| Linear | `POST /integrations/linear/webhook` | `Linear-Signature` HMAC, 1-minute timestamp window |
| Jira | `POST /integrations/jira/webhook` | `X-Hub-Signature` HMAC or a Connect JWT |
| Custom webhooks | `POST /webhooks/w/{uid}[/{token}]` | See [Custom webhooks](../webhooks.md) |

Bodies can be up to 10 MB. The server ignores a duplicate delivery (same provider delivery ID). It verifies each delivery with the secret of the record that the delivery selects:

| Response | Meaning |
| --- | --- |
| `401` (`invalid_signature`, `invalid_token` or `unauthorized`) | No record serves the owner or workspace of the delivery, or the secret does not match. |
| `409` `ambiguous_integration` | Forges: records of more than one team verify the delivery. Give each team its own webhook secret. |
| `202` with `"reason": "no matching integration"` | Linear and Jira: the record is not active. |

## Who can start work

- These events start work only for an account that is linked to an active team member: a Slack mention or direct message, any Teams message, a Linear or Jira agent session, and a forge mention of a factory. Other users get a [link prompt](#link-prompts).
- A forge automation match starts work from any account that can act on the repository. The run acts as the agent, and the event text is untrusted input. GitHub skips bot events by default (`skip_bots`, see [Automations](../automations.md#provider-defaults)).
- Only a linked team member can approve or answer with a comment or chat reply. `requireLinkedActor: true` in `factory.yaml` drops forge, chat and tracker events from unlinked accounts. See [Sender trust](../security.md#sender-trust).
- Posts by Open Factory bots and service accounts never start work. Events from other bots are limited to 20 per hour for each work item.

## Account linking

A link binds an external account to a team user. It needs a proof that the user controls the account: the provider sign-in (OAuth).

### Configure the sign-in

1. Create an OAuth client at the provider with the callback URL `${OF_PUBLIC_URL}/integrations/link-callback`. Each provider page has the steps.
2. Store the client secret as a team secret.
3. Add the keys below to the `config` of one record of the provider. The server uses the team record that has `oauth_client_id`, else the one factory record that has it. Two records at the same level make the sign-in fail.

| Config key | Description |
| --- | --- |
| `oauth_client_id` | The OAuth client ID. |
| `oauth_client_secret_ref` | The team secret that holds the client secret. |
| `oauth_authorize_url`, `oauth_token_url`, `oauth_userinfo_url`, `oauth_id_field` | Replace the defaults below. Necessary for GitLab and Azure DevOps. `oauth_id_field` is the key of the account ID in the user-info document; use a dotted path for a nested field (`data.viewer.id`). |
| `oauth_handle_field`, `oauth_scopes` | Optional. Separate scopes with spaces or commas. |

| Provider | Default authorize, token and user-info URLs | ID field | Handle | Scopes |
| --- | --- | --- | --- | --- |
| GitHub | `<web_url>/login/oauth/authorize`, `<web_url>/login/oauth/access_token`, `<base_url>/user` | `id` | `login` | none |
| Slack | `https://slack.com/openid/connect/authorize`, `https://slack.com/api/openid.connect.token`, `https://slack.com/api/openid.connect.userInfo` | `https://slack.com/user_id` | `name` | `openid profile` |
| Microsoft Teams | `https://login.microsoftonline.com/<tenant_id>/oauth2/v2.0/authorize` and `/token` (tenant `organizations` without `tenant_id`), `https://graph.microsoft.com/v1.0/me` | `id` | `displayName` | `openid profile User.Read` |
| Linear | `https://linear.app/oauth/authorize`, `https://api.linear.app/oauth/token`, `https://api.linear.app/graphql` (query `{ viewer { id name } }`) | `data.viewer.id` | `data.viewer.name` | `read` (joined with commas) |
| Jira | `https://auth.atlassian.com/authorize` (adds `audience=api.atlassian.com`, `prompt=consent`), `https://auth.atlassian.com/oauth/token` (JSON request), `https://api.atlassian.com/me` | `account_id` | `name` | `read:me` |

Each user then links the account on the **Integrations** page, under **Link your account**, and signs in to the provider. The server links the account that the provider returns.

### Link prompts

When an unlinked user starts work, the integration answers with a prompt:

- In a public place (a GitHub, Jira or Linear comment, a Teams thread), the prompt has no link URL, because anyone who can read the place could open it. It points the user to the dashboard.
- In a private message (Slack ephemeral message), the prompt has a link URL, valid for 24 hours, for members of the team that owns the integration.

A link URL opens a confirmation page that names the external account. A `GET` never links: the user confirms with a button. If the provider has a sign-in, the user then signs in, and the account that the provider returns must be the account in the URL. Without a sign-in, only a URL from a private message completes the link.

### Remove a link

A link is never replaced. If the external account is linked to another user, a new link fails, and the page names the current owner. A team admin removes the old link:

```bash
curl -X DELETE "$OF_PUBLIC_URL/integrations/links/slack/U0123456789" \
  -H "Authorization: Bearer $OF_API_KEY"
```

Each link, failed link and removal writes an audit event (`account.link` or `account.unlink`). See also the Slack [email link](slack.md#who-can-start-work) and the Teams `sign in` and `sign out` commands.

## Names in trigger filters

Slack, Microsoft Teams, Linear and Jira filters can use names in place of IDs, such as `channels` for `channel_ids` (see [Matcher syntax](../automations.md#matcher-syntax)). When you apply the definition, the server reads the lists from the provider with the record credential and replaces each name with its ID.

- Names ignore case.
- A name that matches no object, or more than one, is an error. Use the ID then.
- If the integration is not connected or the provider refuses the request, the server gives a warning and compares the names on each event.

## How the factory replies

The factory posts progress, questions, approval requests and results to the source of the work item: the chat thread, the tracker issue (or Linear agent session), or the forge issue, pull request or work item. Each delivery is retried up to 5 times with exponential backoff.

- Work items from pushes, CI events, custom webhooks, schedules, the API and Factory MCP report only in the dashboard.
- A reply in the same thread, issue or pull request continues the work item. `approve` or `reject` resolves an open approval. A reply to an open question answers it.
- A reply from a person less than 7 days after the work item completed opens it again. See [References of a work item](../agents.md#references-of-a-work-item).

## Related pages

- [Automations and triggers](../automations.md)
- [Custom webhooks](../webhooks.md)
- [Secrets](../secrets.md)
- [Security](../security.md)
