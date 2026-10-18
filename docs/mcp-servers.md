# MCP servers

MCP servers give agents tools for external systems. An agent gets an MCP server in one of three ways:

| Way | Declare it with | Use it when |
|---|---|---|
| Inline | `url` or `command` in the agent's `mcpServers` | One factory uses the server, and its credentials are plain tokens. See [Agents](agents.md#mcp-servers). |
| Registry | `managedId: <name>` | The server needs a credential, several factories use it, or it uses OAuth. |
| Built-in | `managedId: builtin/<provider>` | Agents need tools for the factory's own Linear, Jira, Slack or Microsoft Teams integration. |

## The MCP server registry

The registry holds the MCP servers of a team. An editor registers a server once. Agents in each factory of the team then reference it by name:

```yaml
# agents/triage.md (frontmatter extract)
mcpServers:
  errors:
    managedId: sentry
```

When a run starts, Open Factory resolves `managedId` to the registered server and writes it into the harness configuration, as for an inline server.

### Register a server

Use the dashboard (**MCP servers** > **Register server**), the CLI or the [REST API](#rest-api). Store credentials as [secrets](secrets.md) first:

```bash
openfactory secret set SENTRY_TOKEN
openfactory mcp-registry add sentry --url https://mcp.sentry.example/mcp \
  --header 'Authorization=Bearer {{SENTRY_TOKEN}}'

# A server that only the sandbox can reach:
openfactory mcp-registry add internal-docs --url http://10.0.0.5:8080/mcp --direct

# A stdio server that starts in the sandbox:
openfactory mcp-registry add docs --command npx --arg -y --arg @acme/docs-mcp
```

| Field | Rules |
|---|---|
| `name` | 1 to 64 letters, digits, `.`, `_` or `-`. Unique in the team. Names that start with `builtin` are reserved. Agents use it as `managedId`. |
| `url` | An absolute `http` or `https` URL: streamable HTTP, or legacy SSE when the path ends in `/sse`. Set exactly one of `url` and `command`. |
| `headers` | Only with `url`. |
| `command`, `args`, `env` | A command that starts in the sandbox and speaks MCP over stdio. `args` and `env` only with `command`. |
| `connection` | Only with `url`. `proxy` (default) or `direct`. See [Connection modes](#connection-modes). |
| `oauth` | Only with `url`. Needs `connection: proxy`. See [OAuth](#oauth). |

- Reference secrets with `{{SECRET_NAME}}` in `headers` and `env`. The API rejects a literal value for a header or variable whose name looks like a credential, for example `Authorization`, `X-Api-Key` or `GITHUB_TOKEN`.
- An agent can add its own `headers` or `env`, layered over the registered values. It cannot replace the `Authorization` header of a proxied or built-in server.

### Validation

When you save a managed definition (dashboard, `PUT /api/v1/factories/{uid}/definition` or factory creation), each `managedId` must name a registered server of the team.

Git-backed definitions are not checked when they sync. A run whose agent references an unknown server fails with `no MCP server "<name>" is registered for this team`.

## Connection modes

| `connection` | The harness connects to | Headers and OAuth tokens |
|---|---|---|
| `proxy` (default) | The control-plane proxy | Stay in the control plane. See [Proxied servers](#proxied-servers). |
| `direct` | The server URL, from the sandbox | Are written into the harness configuration in the sandbox. The agent can read them. |

- A new `url` server without `connection` gets `proxy`.
- The control plane must reach a proxied server. Use `direct` only for a server that only the sandbox can reach, for example a private endpoint next to a remote worker.
- A legacy SSE server cannot use the proxy. Registered without a mode, it gets `direct` and the response has a warning in `warnings` (the CLI prints it). `connection: proxy` with an SSE URL is an error.
- An OAuth server always uses `proxy`. `connection: direct` with `oauth` is an error.
- A `command` server runs in the sandbox and has no connection mode.
- A server registered before the `connection` field keeps its mode: `proxy: false` reads as `direct`.

## Proxied servers

The run's harness connects to `${OF_PUBLIC_URL}/mcp/proxy/{runId}/{serverId}` with the run's bearer token. The control plane then:

1. Checks that the token is valid for the run, and that the run's agent declares the server.
2. Expands the `{{SECRET}}` header placeholders.
3. Adds the OAuth access token, if the server uses OAuth.
4. Forwards the request and streams the response back.

Secrets and tokens never enter the sandbox, the harness configuration or the transcript.

- The proxy forwards only these request headers: `Accept`, `Content-Type`, `Mcp-Session-Id`, `Mcp-Protocol-Version`, `Last-Event-ID`, `User-Agent`.
- It removes `Set-Cookie` from responses.
- When the upstream server returns 401 or 403, the proxy removes the `WWW-Authenticate` challenge, so the agent's MCP client does not try to sign in.

Requirements:

- `OF_PUBLIC_URL` is set and the sandbox can reach it (the orchestration server needs this too).
- The control plane can reach the upstream URL.
- The server uses streamable HTTP, not legacy SSE.
- With a restricted [network policy](self-hosting/network-policy.md), the run needs only the control plane host.

## OAuth

A run cannot complete a browser sign-in, so an editor connects an OAuth server once. Runs then share that connection.

### Connect a server

1. Register the server with OAuth: **The server uses OAuth** in the dashboard, or `--oauth` with the CLI.
2. On the server's page, select **Connect**. With the CLI, run `openfactory mcp-registry connect <name>` and open the URL in a browser where you are signed in to the dashboard.
3. Approve the access at the provider. The provider sends you back to the server's page, which shows **OAuth: connected**.

Open Factory follows the MCP authorization specification:

- It finds the authorization server from the server's protected resource metadata, or uses the server's origin.
- It reads the authorization server metadata, or uses `/authorize`, `/token` and `/register` on that origin.
- Without a client ID, it uses dynamic client registration.
- It uses the authorization code flow with PKCE (`S256`) and sends the server URL as `resource`.
- It requests the scopes you set, or else the scopes the server advertises.

The redirect URI is `${OF_PUBLIC_URL}/api/v1/mcp-servers/oauth/callback`. Most providers accept `http` only for loopback addresses, so `OF_PUBLIC_URL` must use `https`. The [quick install](self-hosting/kubernetes.md#quick-install) serves plain `http`: add TLS before you connect OAuth servers.

If the provider has no dynamic client registration, register a client there with this redirect URI. Then set **Client ID** and, for a confidential client, **Client secret (secret name)**.

| OAuth field | Meaning |
|---|---|
| `scopes` | Scopes to request. |
| `client_id` | A client registered at the provider. Empty means dynamic client registration. |
| `client_secret_name` | A team secret with the client secret. Requires `client_id`. |
| `authorization_endpoint`, `token_endpoint` | Overrides for the discovered endpoints. `https` only, except on loopback hosts. |

### Tokens

- Tokens are encrypted with a key derived from `OF_SECRET_KEY` and stored in the database. The API and dashboard never show them.
- The control plane refreshes the access token when it expires in less than one minute.
- If the refresh fails, or the server rejects the token, the status is **error** with the reason. An editor must reconnect.
- A change to the URL, OAuth client, endpoints or scopes deletes the token and sets **not connected**.
- **Disconnect** and deleting the server delete the token.
- If you change `OF_SECRET_KEY`, reconnect each OAuth server.

The connection belongs to the team. Each run that declares the server uses the access that the connecting user approved, so connect with an account that has only the access the agents need.

A run whose agent uses an OAuth server that is not connected fails at start with `MCP server "<name>" is not connected`.

## Built-in integration servers

The control plane serves MCP tools for a factory's integrations. They use the integration's credentials, so they act as the integration's app or bot, not as the requester.

```yaml
mcpServers:
  linear:
    managedId: builtin/linear
  slack:
    managedId: builtin/slack
    allowWrite: true
```

A run can use `builtin/<provider>` only when:

- The agent's `mcpServers` has an entry with that `managedId`.
- `factory.yaml` `integrations` attaches the integration.
- The team has an integration record for the provider. If it has several, set `config.factory_id` on the one the factory uses.

Read tools are always available. Write tools need `allowWrite: true` (valid only with a built-in `managedId`).

| Server | Read tools | Write tools |
|---|---|---|
| `builtin/linear` | `linear_get_issue`, `linear_search_issues` | `linear_create_comment` |
| `builtin/jira` | `jira_get_issue`, `jira_search` (JQL) | `jira_add_comment` |
| `builtin/slack` | `slack_read_thread`, `slack_channel_history` | `slack_post_message` |
| `builtin/teams` | `teams_list_channels` | `teams_post_message` |

The Teams server cannot read messages: the Bot Framework gives a bot no channel history.

- `teams_list_channels` returns the channels that the integration record selects with `channel_ids`.
- `teams_post_message` posts as the bot, only in the selected channels. With no selection, it posts only where someone mentioned the bot.
- The Teams server needs no secret; it uses the app credentials of the Teams integration.

List results return at most 50 items. The server log records each tool call with the run, factory and tool name. The built-in servers are served at `${OF_PUBLIC_URL}/mcp/integrations/{provider}/{runId}` and authenticate with the run's bearer token.

## REST API

| Method and path | Role | Purpose |
|---|---|---|
| `GET /api/v1/mcp-servers` | user | List registered servers, and built-in servers with `configured` (the team has the integration). |
| `POST /api/v1/mcp-servers` | editor | Register a server. |
| `GET /api/v1/mcp-servers/{id}` | user | Get a server by ID or name. |
| `PATCH /api/v1/mcp-servers/{id}` | editor | Change a server. Absent fields do not change. An empty string clears `url` or `command`. `"oauth": null` removes OAuth. A `command` server that becomes a `url` server gets the default mode. |
| `DELETE /api/v1/mcp-servers/{id}` | editor | Delete a server and its token. |
| `POST /api/v1/mcp-servers/{id}/oauth/connect` | editor | Start the OAuth flow. Returns `authorization_url`. |
| `POST /api/v1/mcp-servers/{id}/oauth/disconnect` | editor | Delete the token. |
| `GET /api/v1/mcp-servers/oauth/callback` | editor | The provider's redirect target. Needs the browser session of the user who started the flow. |

The Go SDK has the same operations in `client.MCPServers`. See [REST API and Go SDK](api.md).

## Security notes

- The control plane connects to the URLs that editors register: the MCP server, its metadata and the OAuth endpoints. Give the editor role only to people you trust with outbound requests from the server's network.
- An authorization state is valid once, for 10 minutes, and only for the user who started it.
- Built-in write tools post as your integration's bot. Enable `allowWrite` only for agents that must post.

## Related pages

- [Agents](agents.md#mcp-servers)
- [Definition reference](definition-reference.md#mcp-server-object)
- [Integrations](integrations/README.md)
