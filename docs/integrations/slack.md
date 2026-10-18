# Slack

People start and continue factory work from Slack. The factory replies in the thread where the work started, with approval buttons for decisions.

You need permission to create and install apps in the Slack workspace. For the shared setup steps and variables, see [Integrations](README.md#common-setup).

One Slack app can serve several factories. One app for each factory gives each factory its own bot name and keeps routing simple.

## 1. Create the Slack app

1. Open `https://api.slack.com/apps` and click **Create New App** > **From a manifest**.
2. Select the workspace.
3. Paste the manifest below. Replace `factory.example.com` with the host of `OF_PUBLIC_URL`.
4. Click **Create**. Slack cannot verify the events request URL yet. Continue: you verify it in step 4.

```yaml
display_information:
  name: Open Factory
  description: Starts and follows factory work from Slack.
features:
  app_home:
    home_tab_enabled: false
    messages_tab_enabled: true
    messages_tab_read_only_enabled: false
  bot_user:
    display_name: factory
    always_online: true
oauth_config:
  scopes:
    bot:
      - app_mentions:read
      - channels:history
      - channels:read
      - chat:write
      - files:read
      - groups:history
      - groups:read
      - im:history
      - im:read
      - mpim:history
      - mpim:read
      - reactions:read
      - reactions:write
      - users:read
      - users:read.email
settings:
  event_subscriptions:
    request_url: https://factory.example.com/integrations/slack/events
    bot_events:
      - app_mention
      - member_joined_channel
      - message.channels
      - message.groups
      - message.im
      - message.mpim
      - reaction_added
  interactivity:
    is_enabled: true
    request_url: https://factory.example.com/integrations/slack/interactions
  org_deploy_enabled: false
  socket_mode_enabled: false
  token_rotation_enabled: false
```

| Scope | Used for |
| --- | --- |
| `app_mentions:read` | Receive `app_mention` events. |
| `channels:history`, `groups:history` | Receive channel messages. Read the message that got a reaction, and earlier thread messages. |
| `im:history`, `mpim:history` | Receive direct messages and group direct messages. |
| `channels:read`, `groups:read`, `im:read`, `mpim:read` | Read conversation names for the `channels` filter. |
| `chat:write` | Post replies, approval buttons and link prompts. |
| `files:read` | Download thread files for the agent. Without it, the agent is told that files were left out. To add it to an older app, install the app again. |
| `reactions:read` | Receive `reaction_added` events. |
| `reactions:write` | Add the `eyes` reaction that acknowledges a request. |
| `users:read`, `users:read.email` | Read user names and e-mail addresses for the `users` filter and the email link. |

| Bot event | Open Factory event |
| --- | --- |
| `app_mention` | `app_mention` |
| `message.channels`, `message.groups` | `message_posted` |
| `message.im` | `message_im` (also matched by `message_dm`) |
| `message.mpim` | `message_mpim` (also matched by `message_dm`) |
| `reaction_added` | `reaction_added` |
| `member_joined_channel` | `member_joined_channel` |

You can remove the bot events and scopes that you do not need. Keep `app_mentions:read`, `chat:write` and `users:read`.

## 2. Install the app and collect the values

1. Open **Install App** and click **Install to Workspace**. Approve the scopes.
2. Copy the **Bot User OAuth Token** (`xoxb-…`).
3. Open **Basic Information**. Copy the **App ID** (`A…`) and the **Signing Secret**.
4. Find the workspace ID (`team_id`, `T…`) and the bot user ID (`user_id`, `U…`):

   ```bash
   curl -s -H "Authorization: Bearer xoxb-..." https://slack.com/api/auth.test
   ```

## 3. Store the credentials and create the record

```bash
openfactory secret set SLACK_BOT_TOKEN
openfactory secret set SLACK_SIGNING_SECRET

curl -X POST "$OF_PUBLIC_URL/api/v1/integrations" \
  -H "Authorization: Bearer $OF_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "provider": "slack",
    "name": "acme-builder",
    "secret_ref": "SLACK_BOT_TOKEN",
    "config": {
      "team_id": "T0123456789",
      "api_app_id": "A0123456789",
      "bot_user_id": "U0123456789",
      "signing_secret_ref": "SLACK_SIGNING_SECRET"
    },
    "factory_id": "FACTORY_UID"
  }'
```

| Key | Description |
| --- | --- |
| `secret_ref` | The team secret that holds the bot token. |
| `config.team_id` | Necessary. The workspace ID (`T…`). |
| `config.api_app_id` | The app ID (`A…`). When records name the app of an event, only those records can get the event. |
| `config.bot_user_id` | The bot user ID (`U…`). Posts by this user are ignored. |
| `config.signing_secret_ref` | The team secret that holds the signing secret of the app. |
| `config.link_by_email` | `"true"` turns on the [email link](#who-can-start-work). Default off. |

With `factory_id`, events from the app go to that factory only. Without it, events go to every factory that declares `slack`. See [Record selection](README.md#record-selection).

## 4. Verify the request URLs

1. In the app settings, open **Event Subscriptions**. If the request URL is not verified, click **Retry**. It must show **Verified**.
2. Open **Interactivity & Shortcuts**. The request URL must be `${OF_PUBLIC_URL}/integrations/slack/interactions`.

## 5. Connect the factory

```yaml
integrations:
  - type: slack
    slack:
      autoRespondToThreadReplies: true
```

| Setting | Default | Description |
| --- | --- | --- |
| `autoRespondToThreadReplies` | `true` | If `true`, each reply in a work item thread continues the work. If `false`, people must mention the app in the thread. Replies that answer an open question or approval always count. |

Then add automations:

```markdown
---
triggers:
  - provider: slack
    event: app_mention
    filter:
      channels: ["#engineering"]
  - provider: slack
    event: message_dm
---

A teammate asked for help in Slack. Read the request, ask a clarifying
question if needed, and do the work.
```

Invite the app to each channel where it must read messages: `/invite @factory`. See [Automations](../automations.md#slack-slack) for all events and filter keys.

## 6. Test the connection

1. In a channel that matches a trigger, mention the app with a request.
2. The app adds the `eyes` reaction, and a work item appears in the dashboard.
3. The factory replies in the thread.

## Start and continue work

Start work with a mention, a direct message, or a message that matches a `message_posted` or `reaction_added` trigger. Reply in the thread to continue, to answer a question, or to approve (`approve`, `reject` or a button). Only new messages count: edits, deletions and other system messages are ignored.

When a run starts in a thread, the agent workspace gets `context/thread.json` (at most 200 messages) and `context/files/` (at most 10 files of 1 MiB each) under `.openfactory/events/<message_id>/`. See [What the agent gets from the event](../automations.md#what-the-agent-gets-from-the-event).

## Approval buttons

Approval requests and questions with options show buttons. A click from a linked team member resolves the open inbox item, and the buttons change to `<option> selected by @person`. Other people get a private link prompt. Only the first answer counts: a later click gets a private message that the answer was not recorded.

## Who can start work

- Mentions and direct messages need a Slack account that is linked to an active team member. Otherwise, the app posts a private message with a link URL. See [Account linking](README.md#account-linking).
- **Email link.** With `link_by_email: "true"`, the server links a workspace member whose e-mail address matches a team member. Guests, restricted users, users of other workspaces, bots and deleted accounts are never linked this way. The email link never replaces a link.
- `message_posted`, `reaction_added` and `member_joined_channel` events need no linked account. Use filters to limit them.
- Bot messages are marked as bot events. Posts by any Open Factory Slack app on the server never start work.
- A mention is also a channel message in Slack. Both events of one post share one action key, so the work item gets one message.

## Work that does not start in Slack

Work from a schedule, a custom webhook, the API or Factory MCP does not post to Slack. To post a digest to a channel, give the agent the `builtin/slack` tool server with `allowWrite: true`. See [Built-in integration servers](../mcp-servers.md#built-in-integration-servers).

## Troubleshooting

| Problem | Check |
| --- | --- |
| Slack shows `401`, or the URL does not verify | `signing_secret_ref` must hold the signing secret of the app. The server clock must be correct to within 5 minutes. |
| A mention does not start work | The record has the correct `api_app_id` and `team_id`, `factory.yaml` declares `slack`, and a trigger matches. If the app posted a link prompt, link the account. |
| A `channels` filter does not match | The app needs `channels:read` or `groups:read`. Or use `channel_ids`. |
| A `users` filter does not match | The app needs `users:read` and `users:read.email`. |
| No `eyes` reaction | The app needs `reactions:write`, and the bot token must be valid. |
| Channel messages do not arrive | Invite the app to the channel. Subscribe to `message.channels` or `message.groups`. |
| Buttons do nothing | Interactivity must be on, with the correct request URL. The person must have a linked account. |

## Related pages

- [Automations and triggers](../automations.md)
- [Integrations](README.md)
- [Microsoft Teams](teams.md)
