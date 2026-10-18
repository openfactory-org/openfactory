# Linear

A factory starts work from Linear issues, comments and agent sessions, and replies on the issue or in the agent session.

You need admin access to the Linear workspace to create webhooks and applications. A factory can declare Linear or Jira, not both. For the shared setup steps and variables, see [Integrations](README.md#common-setup).

## Choose a credential

| Credential | Supports | Author of factory comments |
| --- | --- | --- |
| Personal API key of a dedicated Linear user | Issue and comment events, issue comments | The user who owns the key |
| OAuth application installed as an app actor | Issue and comment events, agent sessions, agent activities | The application |

Agent sessions (people assign or mention the factory) need an OAuth application.

## Connect with a personal API key

1. Sign in to Linear as the user that the factory acts as.
2. Open **Settings** > **Security & access** > **Personal API keys** and create a key.
3. Find the user ID and the workspace ID:

   ```bash
   curl -s https://api.linear.app/graphql \
     -H "Authorization: lin_api_..." -H "Content-Type: application/json" \
     -d '{"query": "{ viewer { id } organization { id } }"}'
   ```

4. Open **Settings** > **API** > **Webhooks** > **New webhook**.
5. Set **URL** to `${OF_PUBLIC_URL}/integrations/linear/webhook`.
6. Select the **Issues** and **Comments** resource types, and all teams or the teams of the factory.
7. Create the webhook and copy its signing secret.
8. [Create the record](#create-the-integration-record). Set `app_user_id` to `viewer.id`. Do not set `token_type`.

## Connect with an OAuth application

1. Open **Settings** > **API** > **Applications** > **Create new**.
2. Enter a name and the callback URL of your OAuth client.
3. Turn on **Webhooks**. Set the URL to `${OF_PUBLIC_URL}/integrations/linear/webhook`.
4. Select the **Issues**, **Comments** and **Agent session events** resource types. Copy the webhook signing secret.
5. Install the application with the OAuth authorization flow. Use `actor=app` and the scopes `read`, `write`, `app:assignable` and `app:mentionable`.
6. Exchange the authorization code for an access token.
7. Find the application user ID and the workspace ID with the query above. Send the token as `Authorization: Bearer <token>`.
8. [Create the record](#create-the-integration-record). Set `token_type` to `oauth` and `app_user_id` to the application user ID.

## Create the integration record

```bash
openfactory secret set LINEAR_TOKEN            # the access token or API key
openfactory secret set LINEAR_WEBHOOK_SECRET   # the signing secret

curl -X POST "$OF_PUBLIC_URL/api/v1/integrations" \
  -H "Authorization: Bearer $OF_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "provider": "linear",
    "name": "acme",
    "secret_ref": "LINEAR_TOKEN",
    "config": {
      "organization_id": "0b6c1f3e-2a4d-4e5f-8a9b-1c2d3e4f5a6b",
      "app_user_id": "7e8f9a0b-1c2d-4e3f-9a4b-5c6d7e8f9a0b",
      "token_type": "oauth",
      "webhook_secret_ref": "LINEAR_WEBHOOK_SECRET"
    }
  }'
```

| Key | Description |
| --- | --- |
| `secret_ref` | The team secret that holds the access token or API key. |
| `config.organization_id` | Necessary. The Linear workspace ID. Deliveries from this workspace use this record. |
| `config.app_user_id` | The user ID of the application or of the API key owner. Activity by this user never starts work. |
| `config.token_type` | `oauth` sends the token as `Bearer <token>`. If empty, the server sends the API key as it is. |
| `config.webhook_secret_ref` | The team secret that holds the webhook signing secret. |

Without `factory_id`, the record is the team record, and events go to every factory that declares `linear`. See [Record selection](README.md#record-selection).

## Connect the factory

```yaml
integrations:
  - type: linear
```

The `linear.teamIds` setting is accepted for compatibility and ignored. Use trigger filters to select teams:

```markdown
---
agent: foreman
triggers:
  - provider: linear
    event: issue_labeled
    filter:
      teams: [Engineering]
      labels:
        in: [factory-ready]
        not_in: [blocked]
  - provider: linear
    event: agent_session_created
    filter:
      teams: [Engineering]
---

A Linear issue is ready for the factory. Triage it, write a short plan,
and implement it.
```

See [Automations](../automations.md#linear-linear) for all events and filter keys, and [`examples/06-common-automations`](../../examples/06-common-automations) for more examples.

## Events

| Linear activity | Open Factory event |
| --- | --- |
| Issue created | `issue_created`. Also `issue_labeled` if it has labels, and `issue_assigned` if it has an assignee. |
| Labels added | `issue_labeled` |
| Assignee changed | `issue_assigned` |
| Workflow state changed | `issue_state_changed` |
| Comment created | `comment_created` |
| Agent session created (the app is assigned or mentioned) | `agent_session_created` |
| Prompt in an existing agent session | Continues the work item of the session |

For comment and agent session events, the server reads the team, project, state and labels of the issue from the Linear API, so their filters need a valid token.

On the first message of a run, the agent workspace gets `context/issue.json` under `.openfactory/events/<message_id>/`: the title, description, state, labels, assignee and project. See [What the agent gets from the event](../automations.md#what-the-agent-gets-from-the-event).

## Agent sessions

When a person assigns or mentions the application, Linear creates an agent session. The person must have a Linear account that is linked to an active team member. Otherwise, the server posts an `elicitation` activity with a [link prompt](README.md#link-prompts).

The factory replies in the session as agent activities. A prompt in the session continues the same work item.

| Factory message | Activity type |
| --- | --- |
| Question or approval request | `elicitation` |
| Progress update | `thought` |
| Error | `error` |
| Other messages | `response` |

Work items that did not start from an agent session get replies as issue comments.

## Write-back to the issue

The engine adds new pull requests to the issue as attachments, and moves the issue through the states of its team. The credential needs write access to issues. See [Write-back to the tracker](../agents.md#write-back-to-the-tracker).

## Sign-in for account links

The proof of a [link](README.md#account-linking) is a Linear sign-in:

1. Open **Settings** > **API** > **Applications**. Use the OAuth application of the factory, or create one.
2. Add `${OF_PUBLIC_URL}/integrations/link-callback` to the callback URLs of the application.
3. Store the client secret as a team secret.
4. Add `oauth_client_id` and `oauth_client_secret_ref` to the `config` of the record.

The ID field is `data.viewer.id`, the same user ID that Linear webhooks send as the actor.

## Names in trigger filters

Filters can use `teams`, `projects`, `states` and `issues` (an issue number, such as `42`). `projects`, `states` and `issues` use the teams of the same trigger when it has `teams` or `team_ids`. Put the team in the same filter: two teams can have a state with the same name, and the same issue number. The general rules are in [Names in trigger filters](README.md#names-in-trigger-filters).

## Troubleshooting

| Problem | Check |
| --- | --- |
| Deliveries get `401` `invalid_signature` | No record has `organization_id` equal to the workspace ID, or the signing secret in `webhook_secret_ref` does not match the webhook. |
| Deliveries get `401` `stale_webhook` | The server clock differs from Linear by more than one minute. |
| Deliveries get `202` with `no matching integration` | The record is not active. See [Status](README.md#status). |
| A filter on a comment or session event does not match | The token must be valid. The server needs it to read the issue details. |
| Assigning the app does not start work | Use an OAuth application with `actor=app` and the `app:assignable` scope, and subscribe to agent session events. Link the account of the person who assigns it. |
| The factory reacts to its own comments | Set `app_user_id` to the user ID of the token. |

## Related pages

- [Automations and triggers](../automations.md)
- [Integrations](README.md)
- [Jira](jira.md)
