# GitLab

A factory starts work from GitLab merge requests, pushes and bot mentions, and replies in merge request and issue threads. It works with GitLab.com and self-managed GitLab.

GitLab support is the `openfactory-plugin-gitlab` plugin. The server image and the Helm chart load it; for other installs, see [Plugins](../self-hosting/plugins.md). You need the Owner or Maintainer role on the GitLab group or projects. For the shared setup steps and variables, see [Integrations](README.md#common-setup).

## Identities

The factory acts as a GitLab user (a bot account). Comments, branches and merge requests show that account as the author. Activity by any of these accounts never matches a trigger:

| Account | Purpose | Config key |
| --- | --- | --- |
| Team bot | One account for all factories of the team. A mention of it emits `bot_mentioned`. | `bot_username` |
| Factory bots | Optional. One account for each factory. A mention routes to that factory. | `factory_bots` |
| Service accounts | Other automation accounts. | `service_accounts` |

## Connect GitLab

### 1. Create the bot account and token

1. Create a GitLab user or service account for the factory, such as `acme-factory`.
2. Add it to the group with the Developer role or higher. Developer lets it push branches, open merge requests and set commit statuses.
3. Create a personal access token for the account with the `api`, `read_repository` and `write_repository` scopes.

### 2. Store the credentials and create the record

```bash
openfactory secret set GITLAB_TOKEN            # the glpat-... token
openfactory secret set GITLAB_WEBHOOK_TOKEN    # a long random value

curl -X POST "$OF_PUBLIC_URL/api/v1/integrations" \
  -H "Authorization: Bearer $OF_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "provider": "gitlab",
    "name": "acme",
    "secret_ref": "GITLAB_TOKEN",
    "config": {
      "org": "acme",
      "bot_username": "acme-factory",
      "webhook_secret_ref": "GITLAB_WEBHOOK_TOKEN"
    }
  }'
```

`org` is the top-level group. The record serves each project under it, also in subgroups (`acme/platform/api`). For self-managed GitLab, add `"base_url": "https://gitlab.example.com"`.

### 3. Add the webhook

1. Open the group (or project) **Settings** > **Webhooks** > **Add new webhook**.
2. Set **URL** to `${OF_PUBLIC_URL}/plugins/gitlab/webhook`.
3. Set **Secret token** to the value of `GITLAB_WEBHOOK_TOKEN`.
4. Select **Push events**, **Comments** and **Merge request events**. **Issues events** and **Pipeline events** are optional: the server receives them, but they cannot start automations.
5. Click **Add webhook**, then **Test** > **Push events**. The response must be `202`.

If your GitLab tier has no group webhooks, add the webhook to each project.

### 4. Connect a factory

```yaml
codeForges: [GITLAB]
repositories:
  - owner: acme/platform
    name: api
```

`owner` is the full group path and `name` is the project path. If `codeForges` lists more than one forge, set `codeForge: GITLAB` on each GitLab repository.

Then add automations with `provider: gitlab` triggers:

```markdown
---
triggers:
  - provider: gitlab
    event: merge_request
    filter:
      repos: [acme/platform/api]
      actions: [open, reopen]
      base_branches: [main]
---

A merge request was opened against main. Review it and post your
findings as a merge request comment.
```

See [Automations](../automations.md#gitlab-gitlab) for all events and filter keys.

## Integration record reference

Provider: `gitlab`. A record needs `org` and `secret_ref`. See [Record selection](README.md#record-selection).

| Key | Description |
| --- | --- |
| `config.org` | Necessary. The top-level group. |
| `secret_ref` | Necessary. The access token of the bot account. |
| `config.token_type` | `private` (default, sent as `PRIVATE-TOKEN`) or `oauth` (sent as a bearer token). |
| `config.base_url` | The API root. Default `https://gitlab.com/api/v4`. The server adds `/api/v4` if it is missing. |
| `config.web_url` | The web and git host root. The default comes from `base_url`. |
| `config.bot_username` | The team bot account. |
| `config.factory_bots` | Factory bot accounts as comma-separated `username=alias` pairs: `api-bot=api,web-bot=web`. |
| `config.service_accounts` | Other service account usernames, comma-separated. |
| `config.webhook_secret_ref` | The team secret that holds the webhook secret token. |

The old key `group` is not read. A GitLab token reaches every project of its account. To keep factories apart, see [Credential isolation between factories](README.md#credential-isolation-between-factories).

## Mention the factory

- Mention a factory bot (from `factory_bots`) in a new merge request or issue comment. The event goes to that factory. If no automation matches, the foreman starts work.
- Mention the team bot (`bot_username`) in a new comment. The event emits `bot_mentioned`. Add an automation with a `bot_mentioned` trigger to handle it.

Edited comments do not count.

## How the factory responds

- The factory comments on the merge request or issue. A reply to a note in a discussion (a diff note or a thread) goes into that discussion.
- For a comment in a discussion, the agent workspace gets the whole discussion and the diff of the file (`context/review_thread.json`, `context/file_diff.patch`). GitLab leaves out the diff of a very large file; the prompt then lists it as left out. See [What the agent gets from the event](../automations.md#what-the-agent-gets-from-the-event).
- When a Self-improvement change is a merge request, the plugin adds the chosen reviewer to its reviewers. The reviewer needs a [personal forge token](../api.md#personal-forge-tokens).
- Protected branches and merge request approval rules still apply.

## Troubleshooting

| Problem | Check |
| --- | --- |
| Deliveries get `401` `invalid_token` | No record has `org` equal to the top-level group of the project, or the secret token differs from `webhook_secret_ref`. |
| Deliveries get `409` `ambiguous_integration` | Records of several teams share the secret token. Give each team its own token. |
| An action fails with `no gitlab integration record for the owner "x"` | Add a record with `"org": "x"` (the top-level group). |
| A mention does not start work | The username must be in `bot_username` or `factory_bots`. For the team bot, add a `bot_mentioned` automation. |
| A merge request event does not start work | The project is in `repositories` with the full group path, and `actions` lists the GitLab action. |
| The bot cannot push or open a merge request | Check the bot's role, the token scopes and the protected branch rules. |

## Related pages

- [Automations and triggers](../automations.md)
- [Integrations](README.md)
- [Other code forges](other-forges.md)
