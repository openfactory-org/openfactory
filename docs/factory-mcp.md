# Factory MCP for coding agents

Factory MCP is an MCP server in your Open Factory server. A local coding agent, such as Claude Code or Codex, uses it to:

- send work to a factory,
- pull a work item into a local checkout, continue it with local tools, and hand the result back,
- list and search work items, read a work item's conversation, and message its foreman,
- create a managed factory with repositories, integrations and default agents,
- read the definition JSON Schemas and validate a definition tree.

To give a factory's own agents tools for external systems, use `mcpServers` in the definition instead. See [MCP servers](mcp-servers.md).

## Endpoint and authentication

Factory MCP uses the streamable HTTP transport at `${OF_PUBLIC_URL}/mcp`. `${OF_PUBLIC_URL}/api/v1/mcp/factory` is an alias with the same tools.

Each request needs a bearer token:

| Kind | For | How to get it |
|---|---|---|
| Browser sign-in | A person at an MCP client. | Add only the URL to the client. See [Sign in with the browser](#sign-in-with-the-browser). |
| API key | CI and other clients without a person, and clients without MCP sign-in. | `POST /api/v1/api-keys`, or the first key from bootstrap. See [Authentication](api.md#authentication). Send `Authorization: Bearer of_<prefix>_<secret>`. |

A request without a valid token gets `401` with a `WWW-Authenticate` header that names the protected resource metadata. Clients that support MCP sign-in start the browser sign-in from it.

One connection reaches each factory of the user's team.

> **Important:** Factory MCP has no read-only scope, and the core has no read-only role. A connected client acts with the full team role and factory roles of its user: a `user` can send work, send messages and cancel work items. Keep an API key in an environment variable or the client's secret storage. Never commit it.

## Sign in with the browser

1. Add the server URL to the client, without a header. For example, with Claude Code:

   ```bash
   claude mcp add --transport http openfactory https://factories.example.com/mcp
   ```

2. Start the sign-in in the client. In Claude Code, run `/mcp`, select `openfactory` and select the authenticate action. The client opens the browser.
3. If you are not signed in to the dashboard, sign in with your API key or single sign-on.
4. The page shows the client name. Select **Allow**. The browser returns to the client, which is now connected.

- The client registers itself and uses PKCE. You do not create a client ID.
- The access token is valid for 1 hour. The refresh token is valid for 30 days and changes at each use. If the client does not connect for 30 days, sign in again.
- The client acts as you, with your current role. If you leave the team, the sign-in stops working.
- **MCP servers** > **Factory MCP sign-ins** in the dashboard lists each client with its sign-in and last-use times. **End sign-in** stops its access at once. Admins see each sign-in of the team.

Browser sign-in needs `OF_PUBLIC_URL`, reachable from the browser. See [Factory MCP sign-in](security.md#factory-mcp-sign-in).

## Connect a client with an API key

In the examples, `https://factories.example.com` is your `OF_PUBLIC_URL` and `OF_API_KEY` holds your key.

### Claude Code

```bash
export OF_API_KEY=of_abcd1234_...
claude mcp add --transport http openfactory https://factories.example.com/mcp \
  --header "Authorization: Bearer $OF_API_KEY"
```

The shell expands `$OF_API_KEY`, and Claude Code stores the value. `--scope user` adds the server to all projects. `--scope project` writes it to `.mcp.json` in the repository: then do not commit the key, and use a variable reference instead:

```json
{
  "mcpServers": {
    "openfactory": {
      "type": "http",
      "url": "https://factories.example.com/mcp",
      "headers": { "Authorization": "Bearer ${OF_API_KEY}" }
    }
  }
}
```

`claude mcp list` shows whether the server is connected. If the session cannot load a new server, restart it with `claude --continue`.

### Codex

Add this to `~/.codex/config.toml`:

```toml
[mcp_servers.openfactory]
url = "https://factories.example.com/mcp"
bearer_token_env_var = "OF_API_KEY"
```

Codex reads `OF_API_KEY` when it starts, so export it first. `codex mcp list` shows whether Codex loaded the server.

### Other MCP clients

Point a client that supports remote streamable HTTP servers at the URL, with the `Authorization` header. Clients that use the `mcpServers` JSON format take the same entry as the `.mcp.json` example above. Use an environment variable for the key if the client supports substitution.

### Clients that support only stdio

`openfactory mcp` runs a local stdio MCP server that forwards each message to `<server>/mcp` with your API key:

```json
{
  "mcpServers": {
    "factory": {
      "command": "openfactory",
      "args": ["mcp"],
      "env": { "OF_SERVER": "https://factories.example.com", "OF_API_KEY": "of_..." }
    }
  }
}
```

- Server URL: `--server` or `OF_SERVER` (default `http://localhost:8080`). Key: `--api-key` or `OF_API_KEY`. Without a key, the command stops with an error.
- `--path` sets the endpoint path (default `/mcp`), for example `/api/v1/mcp/factory`.

With Claude Code: `claude mcp add openfactory -e OF_SERVER=https://factories.example.com -e OF_API_KEY=$OF_API_KEY -- openfactory mcp`.

### Check the connection

Ask the agent to call `list_factories`. A missing, wrong or expired token gives `401` before any tool runs, and a browser sign-in client signs in again.

## Send new work to a factory

The agent calls `send_work_item` with `factory_id`, `title` and `note`. The foreman starts from the note, so state the goal and what "done" means, the context and constraints, and the work already done.

The factory cannot see unpushed work. To build on local changes, push the branch or open a pull request first, then pass `branch` or `pull_request_url`. A pull request URL is linked to the new work item.

The result has `work_item_id`, `run_id`, `url` (the work item in the dashboard) and `run_url`.

## Continue a work item locally

1. **Find the work item** with `list_work_items` or `search_work_items`. Or pass `get_work_item` a reference in `work_item`: a work item ID, a work item or run URL, a pull request or issue URL, `owner/repo#123`, a Slack permalink, a Linear or Jira key or URL, or a branch name.
2. **Get the context.** Call `get_work_item` with `start_working=true`. The result has the status, run history and Git setup commands for each repository. The commands clone the repository into an isolated `git worktree`, on the open pull request's branch or on a new branch `factory/<short-id>-<slug>`.
3. **Run the setup commands yourself.** Factory MCP never changes local files.
4. **Keep the foreman informed** with `message_foreman`, and read replies with `get_conversation`. Messages do not move or hand back the work item.
5. **Commit and push** after you validate the change.
6. **Hand the work item back.** Call `send_work_item` with `work_item_id`, the pushed `branch` or `pull_request_url`, and a note: what changed, what you validated, what remains. The foreman decides the next step.
7. **Close the work item** with `complete_work_item` when nothing remains for the factory. A handback does not complete it.

> **Caution:** `start_working=true` records you as the local owner, but does not lock or pause the work item. The factory can continue its own runs, and `get_work_item` warns you when runs are active. Tell the foreman that you take the work item, to prevent duplicate changes.

A work item that you take over locally does not count as autonomous in the [dashboard metrics](measure-and-improve.md#metric-definitions).

## Get notified when a work item needs you

1. Call `list_notification_routes` with `factory_id`. It lists your available destinations: the inbox and, if the factory has the integration, `slack_dm`, `teams_dm`, `linear_issue` and `jira_issue`.
2. Pass the route ID as `notify_route` to `send_work_item`.

Delivery is best-effort.

## Tool reference

The client gets each tool's input schema from the server. Results contain dashboard links. A failed tool returns `isError: true` and `{"error": {"code": "...", "message": "..."}}`, with the [REST API error codes](api.md#errors).

| Tool | Read-only | What it does |
|---|---|---|
| `list_factories` | Yes | Lists the factories you can access. `search` filters on name or alias. |
| `get_factory_file_schema` | Yes | Returns the definition JSON Schema catalog, or one document (`document`, `version`). |
| `validate_factory_files` | Yes | Validates a complete definition tree (`files`: path to content) without saving it. |
| `list_teams` | Yes | Lists your team. A self-hosted user belongs to one team, so there are no joinable teams. |
| `create_team` | No | Returns `conflict`, because you already belong to a team. Kept for client compatibility. |
| `join_team` | No | Returns `already_member` for your own team, `operation_not_supported` for others. |
| `list_forge_repositories` | Yes | Lists repositories of a connected GitHub or GitLab account (`code_forge`, `query`). |
| `list_tracker_scopes` | Yes | Lists the Linear teams or Jira projects that can route issues to a new factory. |
| `start_connection` | No | Starts or checks the setup of GitHub, GitLab, Slack, Linear or Jira. |
| `get_connection_status` | Yes | Checks whether an authorization flow completed. |
| `create_factory` | No | Creates a managed factory: `name`, `alias`, `repositories`, `integrations` (`slack`, `microsoft-teams`, `linear` or `jira`; not both Linear and Jira), `agents` (`triage`, `spec`, `implement`, `review`; all by default), `model` (default `claude-opus-5-5`, which selects the `claude` harness). |
| `list_work_items` | Yes | Lists one factory's work items. Filters: `creator` (`me` or a user ID), `stage`, `since`, `limit` (default 20, maximum 100), `cursor`. |
| `search_work_items` | Yes | Searches work item titles in all your factories. |
| `get_work_item` | No | Reads status, run history and outputs. `needs_decision` lists open inbox items that wait on you, with their questions. `start_working=true` adds local setup guidance and records you as local owner. |
| `message_foreman` | No | Sends a message to the foreman of a work item. |
| `get_conversation` | Yes | Reads the foreman conversation. `after` returns only newer messages. |
| `send_work_item` | No | Creates a work item (`factory_id`, `title`, `note`) or hands work back (`work_item_id`, `note`, `branch`, `pull_request_url`). Optional `notify_route`. |
| `list_notification_routes` | Yes | Lists your notification destinations in a factory. |
| `complete_work_item` | No | Marks a work item complete, with an optional `note`. |

`list_forge_repositories` and `list_tracker_scopes` return `integration_not_configured` when the server cannot list repositories or tracker scopes. Then call `start_connection` for the provider. It returns `connected` if the team already has the integration. Only an admin can connect a new team integration. See [Integrations](integrations/README.md).

## Factory MCP and the orchestration server

The engine gives each agent run its own orchestration MCP server at `/mcp/run/{runId}`, with a token for that run only. Agents use it to start other agents, ask people and manage the work item. You do not connect to it. See [The orchestration server](agents.md#the-orchestration-server).
