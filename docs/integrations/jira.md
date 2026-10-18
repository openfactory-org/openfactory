# Jira

A factory starts work from Jira Cloud issues, status changes and agent sessions, and replies with comments on the issue. An agent session starts when a person assigns the issue to the factory's Jira account, or mentions that account.

You need a Jira Cloud site (such as `https://acme.atlassian.net`) and Jira administrator access to create webhooks. A factory can declare Jira or Linear, not both. For the shared setup steps and variables, see [Integrations](README.md#common-setup).

## 1. Create the factory's Jira account

The factory acts as a Jira user. Its comments show that user as the author.

1. Create an Atlassian account for the factory, such as `factory@acme.com`, and add it to the site.
2. Give it the **Browse projects**, **Add comments** and **Assignable user** permissions in the projects of the factory. For [write-back](#what-happens-during-a-run), it also needs permission to transition issues and add links.
3. Sign in as the account. Create an API token at `https://id.atlassian.com/manage-profile/security/api-tokens`.
4. Find the account ID (the `accountId` field):

   ```bash
   curl -s -u "factory@acme.com:$JIRA_API_TOKEN" https://acme.atlassian.net/rest/api/3/myself
   ```

## 2. Create the webhook

1. Open **Settings** > **System** > **WebHooks** > **Create a WebHook**.
2. Set **URL** to `${OF_PUBLIC_URL}/integrations/jira/webhook`.
3. Set **Secret** to a long random value. Jira then signs each delivery with `X-Hub-Signature`.
4. Optionally, enter a JQL filter, such as `project in (ENG, OPS)`.
5. Under **Issue related events**, select **Issue: created**, **Issue: updated** and **Comment: created**.
6. Click **Create**.

## 3. Store the credentials and create the record

```bash
openfactory secret set JIRA_API_TOKEN
openfactory secret set JIRA_WEBHOOK_SECRET

curl -X POST "$OF_PUBLIC_URL/api/v1/integrations" \
  -H "Authorization: Bearer $OF_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "provider": "jira",
    "name": "acme",
    "secret_ref": "JIRA_API_TOKEN",
    "config": {
      "site": "acme.atlassian.net",
      "base_url": "https://acme.atlassian.net",
      "email": "factory@acme.com",
      "bot_account_id": "712020:4f3e2d1c-0b9a-4876-a5b4-c3d2e1f0a9b8",
      "webhook_secret_ref": "JIRA_WEBHOOK_SECRET"
    }
  }'
```

| Key | Description |
| --- | --- |
| `secret_ref` | The team secret that holds the API token, or an OAuth access token. |
| `config.site` | Necessary. The site host name, such as `acme.atlassian.net`. The server compares it with the host of the issue URLs in deliveries. If the issue URLs are `https://api.atlassian.com/ex/jira/<cloudId>/...`, use the cloud ID. A URL is kept as its host name. |
| `config.base_url` | The site URL. For OAuth, `https://api.atlassian.com/ex/jira/<cloudId>`. |
| `config.auth` | `basic` (default: e-mail and API token) or `oauth` (bearer access token). |
| `config.email` | The e-mail address of the factory account, for `basic`. |
| `config.bot_account_id` | The factory account ID. Assigning or mentioning it starts an agent session. Its own activity never starts work. |
| `config.webhook_secret_ref` | The team secret that holds the webhook secret. |
| `config.client_key` | For a Connect app: the client key (JWT issuer). A delivery with a Connect JWT uses only the records with its `client_key`. |
| `config.shared_secret_ref` | For a Connect app: the team secret that holds the shared secret. The server verifies Connect JWTs with it. |

Without `factory_id`, the record is the team record, and events go to every factory that declares `jira`. See [Record selection](README.md#record-selection).

## 4. Connect the factory

```yaml
integrations:
  - type: jira
```

The `jira.projectKeys` setting is accepted for compatibility. Use trigger filters to select projects:

```markdown
---
triggers:
  - provider: jira
    event: agent_session_created
    filter:
      project_keys: [ENG]
  - provider: jira
    event: issue_labeled
    filter:
      project_keys: [ENG]
      labels: [factory-ready]
---

A Jira issue was handed to the factory. Read the issue, write a short plan
as a comment, and implement it.
```

See [Automations](../automations.md#jira-jira) for all events and filter keys.

## Sign-in for account links

The proof of a [link](README.md#account-linking) is an Atlassian sign-in:

1. At `https://developer.atlassian.com/console/myapps/`, create an **OAuth 2.0 (3LO)** app.
2. Under **Permissions**, add the **User identity API** (scope `read:me`).
3. Under **Authorization**, set the callback URL to `${OF_PUBLIC_URL}/integrations/link-callback`.
4. Copy the client ID and secret from **Settings**. Store the secret as a team secret.
5. Add `oauth_client_id` and `oauth_client_secret_ref` to the `config` of the record.

The ID field is `account_id`, the same `accountId` that Jira webhooks send as the actor.

## Names in trigger filters

Filters can use `projects` (for `project_keys`) and `statuses` (for `status_ids`, only on `status_changed`). The Jira account must be able to browse the projects. Jira can have two statuses with the same name: use the status ID then. The general rules are in [Names in trigger filters](README.md#names-in-trigger-filters).

## Events

| Jira activity | Open Factory event |
| --- | --- |
| Issue created | `issue_created`. Also `issue_labeled` if it has labels. |
| Labels added | `issue_labeled` |
| Status changed | `status_changed` |
| Issue assigned to the factory account, or the account mentioned in the description of a new issue or in a comment | `agent_session_created` |
| Comment created | Continues the active work item of the issue. It cannot start an automation. |

For `agent_session_created`, the person must have a Jira account that is linked to an active team member. Otherwise, the factory comments on the issue with a [link prompt](README.md#link-prompts).

## What happens during a run

- The factory comments on the issue. Its Jira permissions, not filters, limit what agents can do in Jira.
- The engine keeps the issue in step with the work item, and adds a new pull request to the issue as a remote link. See [Write-back to the tracker](../agents.md#write-back-to-the-tracker).
- On the first message of a run, the agent workspace gets `context/issue.json` under `.openfactory/events/<message_id>/`: the summary, description, status, labels, assignee and project. See [What the agent gets from the event](../automations.md#what-the-agent-gets-from-the-event).

## Troubleshooting

| Problem | Check |
| --- | --- |
| Deliveries get `401` `invalid_signature` | Set `site` to the host of the issue URLs, so that the server finds the record. The webhook secret must match `webhook_secret_ref`, and the webhook must have a secret. |
| Deliveries get `202` with `no matching integration` | The record is not active. See [Status](README.md#status). |
| Assigning the factory does not start work | Set `bot_account_id`. The person who assigns must have a linked account. The webhook must include **Issue: updated**. |
| The factory cannot comment | Check `email`, the API token, and the **Add comments** permission. |
| A label filter does not match | Jira labels and project keys are case-sensitive. |

## Related pages

- [Automations and triggers](../automations.md)
- [Integrations](README.md)
- [Linear](linear.md)
