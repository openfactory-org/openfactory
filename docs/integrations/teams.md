# Microsoft Teams

People start and continue factory work from Teams channels. The factory replies in the channel thread where the work started. The integration supports channel conversations only: it ignores personal chats and group chats with the bot.

You need an Azure subscription for the bot, and a Teams administrator (or a team owner, if custom apps are allowed) to upload the app. The Bot Framework must reach `OF_PUBLIC_URL` over HTTPS. For the shared setup steps and variables, see [Integrations](README.md#common-setup).

## 1. Register the bot in Azure

1. In the Azure portal, create an **Azure Bot** resource. Under **Type of App**, select **Multi Tenant** or **Single Tenant**, and let Azure create a new Microsoft App ID.
2. Open the resource and click **Configuration**.
3. Set **Messaging endpoint** to `${OF_PUBLIC_URL}/integrations/teams/messages`. Click **Apply**.
4. Write down the **Microsoft App ID**.
5. Click **Manage Password** next to the App ID. Under **Certificates & secrets**, create a client secret. Copy its value.
6. Open **Channels** and add the **Microsoft Teams** channel.

For replies, the server gets Bot Framework tokens (client-credentials grant) from this tenant:

1. `config.app_type: SingleTenant`: the record's `config.tenant_id`, which is then required.
2. `config.app_type: MultiTenant`: `botframework.com`.
3. No `app_type`: `botframework.com`. If that request fails and the record has `tenant_id`, the server uses `tenant_id` from then on.

## 2. Build the Teams app package

The package is a ZIP file with `manifest.json`, `color.png` (192 x 192) and `outline.png` (32 x 32, transparent). Replace `<MICROSOFT_APP_ID>` and `factory.example.com`:

```json
{
  "$schema": "https://developer.microsoft.com/en-us/json-schemas/teams/v1.17/MicrosoftTeams.schema.json",
  "manifestVersion": "1.17",
  "version": "1.0.0",
  "id": "<MICROSOFT_APP_ID>",
  "developer": {
    "name": "Acme",
    "websiteUrl": "https://factory.example.com",
    "privacyUrl": "https://factory.example.com",
    "termsOfUseUrl": "https://factory.example.com"
  },
  "name": { "short": "Open Factory", "full": "Open Factory" },
  "description": {
    "short": "Start factory work from Teams.",
    "full": "Mention the bot in a channel to start factory work. Reply in the thread to continue it."
  },
  "icons": { "color": "color.png", "outline": "outline.png" },
  "accentColor": "#FFFFFF",
  "bots": [
    {
      "botId": "<MICROSOFT_APP_ID>",
      "scopes": ["team"],
      "isNotificationOnly": false,
      "supportsFiles": false,
      "commandLists": [
        {
          "scopes": ["team"],
          "commands": [
            { "title": "help", "description": "Show how to use the factory." },
            { "title": "sign in", "description": "Link your Microsoft account." },
            { "title": "sign out", "description": "Remove the link." }
          ]
        }
      ]
    }
  ],
  "validDomains": ["factory.example.com"],
  "webApplicationInfo": {
    "id": "<MICROSOFT_APP_ID>",
    "resource": "api://factory.example.com/<MICROSOFT_APP_ID>"
  },
  "authorization": {
    "permissions": {
      "resourceSpecific": [
        { "name": "ChannelMessage.Read.Group", "type": "Application" }
      ]
    }
  }
}
```

| Part | Why |
| --- | --- |
| `bots[].scopes: ["team"]` | The bot works in team channels. |
| `ChannelMessage.Read.Group` | The bot receives all channel messages, not only mentions. Needed for `message_posted` triggers, thread replies without a mention, and the [thread context](#what-the-agent-gets-from-the-thread). |
| `webApplicationInfo` | Required for the resource-specific permission. |

## 3. Install the app in a team

1. In Teams, open **Apps** > **Manage your apps** > **Upload an app** > **Upload a custom app** (or use the Teams admin center).
2. Select the ZIP file, add the app to the team, and accept the permission request.
3. In the team menu, click **Get link to team**. The `groupId` value in the link is the Microsoft Graph team ID.
4. In each channel menu, click **Get link to channel**. The URL-decoded part after `/channel/` is the channel ID, such as `19:a1b2c3d4e5f6@thread.tacv2`.

## 4. Store the credential and create the record

Create one record for each connected team:

```bash
openfactory secret set TEAMS_BOT_SECRET

curl -X POST "$OF_PUBLIC_URL/api/v1/integrations" \
  -H "Authorization: Bearer $OF_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "provider": "microsoft_teams",
    "name": "platform-team",
    "secret_ref": "TEAMS_BOT_SECRET",
    "config": {
      "team_id": "0f5c2a1e-8b4d-4c3a-9e2f-7a6b5c4d3e21",
      "tenant_id": "9a8b7c6d-5e4f-4a3b-8c2d-1e0f9a8b7c6d",
      "app_id": "<MICROSOFT_APP_ID>"
    },
    "factory_id": "FACTORY_UID"
  }'
```

| Key | Description |
| --- | --- |
| `secret_ref` | Necessary. The team secret that holds the bot client secret. |
| `config.team_id` | Necessary. The Microsoft Graph team ID. |
| `config.app_id` | Necessary. The Microsoft App ID of the bot. The server accepts Bot Framework requests whose token audience is the `app_id` of any Teams record. |
| `config.tenant_id` | Advised. The Microsoft 365 tenant. Only its users can start or continue work. The token tenant of a single-tenant bot, the thread context, name resolution and the account sign-in use it. |
| `config.app_type` | Optional. `SingleTenant` or `MultiTenant`. See [step 1](#1-register-the-bot-in-azure). |
| `config.channel_ids` | Optional. Comma-separated channel IDs. If set, `builtin/teams` can post only in these channels. |

Without `factory_id`, events go to every factory that declares `microsoft-teams`. See [Record selection](README.md#record-selection).

## 5. Connect the factory

```yaml
integrations:
  - type: microsoft-teams
    microsoft-teams:
      autoRespondToThreadReplies: true
```

`autoRespondToThreadReplies` works as in [Slack](slack.md#5-connect-the-factory). The default is `true`.

A Teams trigger must name exactly one team and at least one channel:

```markdown
---
triggers:
  - provider: teams
    event: app_mention
    filter:
      team_ids: ["0f5c2a1e-8b4d-4c3a-9e2f-7a6b5c4d3e21"]
      channel_ids: ["19:a1b2c3d4e5f6@thread.tacv2"]
---

A teammate asked for help in Teams. Read the request and do the work.
```

See [Automations](../automations.md#microsoft-teams-teams) for all filter keys.

## 6. Test the connection

1. In a connected channel, mention the bot with `help`. The bot replies in the thread.
2. [Set up the sign-in](#sign-in-for-account-links), then link your account on the **Integrations** page. (`sign in` in Teams posts a public prompt without a link URL.)
3. Mention the bot with a request that matches a trigger. A work item appears in the dashboard.

## Sign-in for account links

The proof of a [link](README.md#account-linking) is a Microsoft sign-in:

1. In the Microsoft Entra admin center, open the app registration of the bot, or create one.
2. Under **Authentication**, add the platform **Web** with the redirect URI `${OF_PUBLIC_URL}/integrations/link-callback`.
3. Under **API permissions**, make sure that the delegated Microsoft Graph permission `User.Read` is there.
4. Under **Certificates & secrets**, use the bot client secret or create one. Store it as a team secret.
5. Add `oauth_client_id` (the application ID; it can equal `app_id`) and `oauth_client_secret_ref` to the `config` of the record.

Set `config.tenant_id` too. With it, only users of that tenant can sign in. Without it, any work or school account can sign in. The ID field is `id`, the Entra object ID of the user (`aadObjectId` in a Teams message).

## Names in trigger filters

A trigger can use `channels` (for `channel_ids`, counts as the required `channel_ids`) and `users` (for `user_ids`: a display name or an e-mail address). It still needs `team_ids`. The general rules are in [Names in trigger filters](README.md#names-in-trigger-filters).

```yaml
filter:
  team_ids: ["0f5c2a1e-8b4d-4c3a-9e2f-7a6b5c4d3e21"]
  channels: [Incidents]
```

To read the channels and members from Microsoft Graph, the server needs `tenant_id` on the record and these `Application` resource-specific permissions in the manifest (install the app again after you add them): `ChannelSettings.Read.Group` for channels and `TeamMember.Read.Group` for users.

If Graph refuses the request, the warning names the missing permission. The names are then compared on each event. Events carry the channel name (except for the General channel) and the sender display name, not the e-mail address. Use the ID for the General channel, and display names for `users`.

## Start and continue work

- **Start.** Mention the bot in a channel, or post a message that matches a `message_posted` trigger.
- **Continue.** Reply in the thread.
- **Answer.** Reply in the thread to a question. Reply `approve` or `reject` to an approval request. Teams messages from the factory have no buttons.

| Command (mention the bot with only the command) | Result |
| --- | --- |
| `help` | Shows how to use the bot. |
| `sign in` (also `signin`, `login`, `link`) | Posts an account link prompt. |
| `sign out` (also `signout`, `logout`, `unlink`) | Removes the link. |

## Who can start work

- Messages count only from users whose Microsoft account is linked to an active team member (and, with `tenant_id`, who belong to that tenant). Other users are ignored. If they mention the bot, it replies with a link prompt.
- Messages from Open Factory bots are ignored.
- If the Microsoft team has no record, a mention gets the reply that the channel is not connected.

## What the agent gets from the thread

On the first message of a run, the agent workspace gets `context/thread.json` under `.openfactory/events/<message_id>/`. See [What the agent gets from the event](../automations.md#what-the-agent-gets-from-the-event).

- The file holds the first message and the replies, oldest first: ID, time, sender, text without HTML, and attachment names (files are not downloaded). System and deleted messages and secret values are left out.
- The limit is 256 KiB and 200 messages. A longer thread keeps the first message and the newest replies.
- The server reads the thread from Microsoft Graph as the bot. This needs `ChannelMessage.Read.Group` and `tenant_id`. For a multi-tenant bot, the app must be allowed in that tenant.
- If Microsoft Graph refuses the request, the run still starts. The agent gets a note with the reason, such as a missing permission or `tenant_id`.

## Work that does not start in Teams

Work from a schedule, a custom webhook, the API or Factory MCP does not post to Teams. To post a digest to a channel, give the agent the `builtin/teams` tool server with `allowWrite: true`. The agent can then post as the bot, but cannot read messages. See [Built-in integration servers](../mcp-servers.md#built-in-integration-servers).

## Troubleshooting

| Problem | Check |
| --- | --- |
| The bot does not answer | The messaging endpoint is `${OF_PUBLIC_URL}/integrations/teams/messages` over HTTPS. Look for `rejected bot framework request` in the server log. The record `app_id` must match the bot. |
| The bot says that the channel is not connected | Create a record with the Graph team ID in `team_id`. |
| The bot cannot reply | The client secret in `secret_ref` is wrong or expired. For a single-tenant bot, set `app_type: SingleTenant` and `tenant_id`. |
| Messages without a mention do not start work | Add `ChannelMessage.Read.Group` to the manifest and install the app again. |
| The agent does not get the thread | Read the note in the run. Check `ChannelMessage.Read.Group` (install the app again after you add it) and `tenant_id`. |
| The account link fails after the Microsoft sign-in | Check `oauth_client_id`, `oauth_client_secret_ref` and the redirect URI. The user must sign in with the same account that wrote in Teams. |
| A trigger does not match | Set both `team_ids` and `channel_ids`, with correct IDs. The user must be linked. |

## Related pages

- [Automations and triggers](../automations.md)
- [Integrations](README.md)
- [Slack](slack.md)
