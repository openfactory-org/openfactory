# Security

This page describes the security model of a self-hosted deployment: authentication, roles, secrets, sandbox isolation and data boundaries.

## Trust boundaries

| Component | Trust level | Notes |
|---|---|---|
| Control plane (`openfactory serve`) | Trusted | Holds the database and the secret key. |
| Database | Trusted | Stores secrets only as ciphertext, and API keys only as SHA-256 hashes. |
| Remote worker | Trusted by the team | Authenticates with a team API key. Runs sandboxes for that team. |
| Plugin | Trusted | Runs as a child process of the server, with a minimal environment: `PATH`, `HOME`, `TMPDIR`, `LANG`, `TZ`, the variables in `OF_PLUGIN_ENV_ALLOW`, and its own entry of `OF_PLUGIN_ENV`. It does not get `OF_SECRET_KEY` or the database URL. A sandbox backend plugin controls the sandboxes that it starts, so install only plugins that you trust. |
| Sandbox | Untrusted | Runs agent commands, which can follow instructions from issues, pull requests and chat messages. |
| Harness CLIs (`claude`, `codex`) | Untrusted | Run inside the sandbox. |

A sandbox can read every secret and variable that its run receives, including the model credential (see [Model credentials](#model-credentials)). Give agents only the secrets that they need. [Redaction](#redaction) hides known secret values in run output, but it is only a second barrier.

## Authentication

### API keys

The REST API, the Factory MCP server and remote workers authenticate with API keys.

- Format: `of_<prefix>_<secret>` for a user, `ofa_<prefix>_<secret>` for a service account. The prefix has 8 lowercase letters or digits. The secret has 40 letters or digits.
- The server stores only the prefix and the SHA-256 hash of the key, and compares hashes in constant time. The full key is shown once, at creation.
- A request has the role of the key's owner. A user key never authenticates a service account, and the reverse.
- A service account is a non-person principal, for a CI job or a bot. Only an admin creates, changes or deletes one. A delete also deletes its keys. See [Service accounts](api.md#service-accounts).
- The server records the last use of a key, at most once per minute.

To create, list and revoke keys, see [Create and revoke keys](api.md#create-and-revoke-keys). An admin can create or revoke keys of other users.

### Factory MCP sign-in

An MCP client can connect to the Factory MCP server with only its URL and sign in through the browser. The server is an OAuth 2.1 authorization server for this purpose. Browser sign-in needs `OF_PUBLIC_URL`.

- A `401` from `/mcp` has a `WWW-Authenticate` header with `resource_metadata`. The client reads `/.well-known/oauth-protected-resource/mcp`, then `/.well-known/oauth-authorization-server`.
- The client registers at `/oauth/register` (dynamic client registration). Only public clients are allowed. A redirect URI must use `https`, an `http` loopback host (`localhost`, `127.0.0.1` or `[::1]`), or a private-use scheme of a native app.
- `/oauth/authorize` uses the dashboard sign-in (API key or OIDC). The user then allows or denies the named client. PKCE with `S256` is required.
- The access token (prefix `ofm_`) is valid for 1 hour. The refresh token (prefix `ofr_`) is valid for 30 days and changes at each use. A used refresh token does not work again.
- The server stores only SHA-256 hashes of codes and tokens. A token works only on the Factory MCP server, not on the REST API.
- A token has the current role of its user. It stops working when the user leaves the team or becomes a service account.
- Users see and end their sign-ins on the **MCP servers** page of the dashboard (**End sign-in**). Admins see and end every sign-in of the team. The API is `GET /api/v1/mcp-sign-ins` (`scope=team` for admins) and `DELETE /api/v1/mcp-sign-ins/{id}`. A client can revoke its own token at `/oauth/revoke`.
- Sign-ins, denials and revocations are audit events (`mcp.signin.authorize`, `mcp.signin.revoke`).

API keys also work on `/mcp`. Use them for clients without a person, such as CI.

### Bootstrap the first admin

A new server has no teams and no users. One unauthenticated request to `POST /api/v1/bootstrap` (or `openfactory bootstrap`) creates the first team, its admin and an API key. See [Get the first key](api.md#get-the-first-key). After that, the endpoint fails with `409` and the code `conflict`. Run the bootstrap immediately after the first start, before you expose the server.

### Dashboard sign-in

Users sign in at `/login` with their own API key.

- The session is a signed `of_session` cookie, valid for 7 days. Its key derives from the signing key (`OF_SIGNING_KEY`, or `OF_SECRET_KEY` when that is not set).
- If `OF_PUBLIC_URL` starts with `https://`, cookies have the `Secure` attribute.
- A cookie-authenticated request that changes data must have the header `X-OF-CSRF: 1` or an `Origin` header of the server. The dashboard sends the header.
- Requests with an `Authorization: Bearer` header are also accepted.

### OIDC single sign-on

OIDC login is part of the free core. The login page shows a single sign-on button, and identity-provider groups map to roles. See [OIDC login](self-hosting/oidc.md).

### Development mode

`OF_DEV=true` creates an admin and prints its API key once, generates a secret key in the data directory, and selects the `local` sandbox backend, which runs agents on the host without isolation. See [Development mode](self-hosting/configuration.md#development-mode).

> **Warning:** Never set `OF_DEV=true` on a server that other people or networks can reach.

### Session tokens of runners

The runner in each sandbox calls the control plane with a session token (prefix `ofs_`). Its key derives from the signing key (sub-key `openfactory/runner-token-key`). The token cannot call the REST API. The runner uses it for:

- the runner API;
- the orchestration MCP server at `${OF_PUBLIC_URL}/mcp/run/{runId}`;
- the MCP proxy (`/mcp/proxy/{runId}/{serverId}`) and the built-in integration servers (`/mcp/integrations/{provider}/{runId}`). These two also check that the run's agent declares the server. See [MCP servers](mcp-servers.md).

Limits:

- The token is valid only for its run and the active session. After the session ends, the server answers `401`. When a newer session of the run exists, it answers `409`.
- A bad signature, an expired token, or a token of another run or team gets `401`. The run paths accept no other token.
- The token lasts 1 hour. A runner with a stale token asks for a new one.
- There is no API or command to revoke one token. A token ends with its session, or when a newer session starts. A change of the signing key makes every token invalid, unless `OF_SIGNING_KEY_PREVIOUS` holds the old key. See [Key rotation](self-hosting/configuration.md#key-rotation).

### MCP server credentials

A registered `url` MCP server uses the control-plane proxy by default (`connection: proxy`). The headers of a [proxied server](mcp-servers.md#proxied-servers) and the OAuth tokens of a connected server stay in the control plane. They are not written to the sandbox, the harness configuration or the transcript.

A server with `connection: direct` sends its expanded headers to the sandbox, where the agent can read them. Use `direct` only for a server that the control plane cannot reach. Inline `url` servers in a definition are always direct, so register each server that needs a credential.

OAuth tokens are encrypted like secrets, with a data key that `OF_SECRET_KEY` wraps. After a [key rotation](self-hosting/configuration.md#key-rotation), no server must connect again.

### Run principals

Each run records its `creator` and the principal that it acts as (`run_as`): a user, a service account or the factory. See [Run principals](definition-reference.md#run-principals). The cloud identity token of a run has the claims `run_as` and `creator`. The session token has the `run_as` principal as its subject.

With `credentialStrategy: CREATOR`, a run that acts as a person uses that person's forge token. If the person has no token, the run fails with `external_authentication_required`. It never falls back to the factory's credential.

The built-in tracker and chat tools (`builtin/linear`, `builtin/jira`, `builtin/slack`, `builtin/teams`) use the credential of the integration record (for Teams, the bot credentials), not the credential of the `run_as` principal. They do not check that principal's permissions at the provider. So each member who can start a run of an agent with a built-in server and `allowWrite` can read and write what the bot can. Give the bot only the access that every such member may have.

### Worker authentication

Remote workers connect to `/api/v1/workers/connect` with a team API key whose owner has the team-wide `admin` role. The worker registers under the team of the key. Its API key is not passed to sandboxes. See [Workers](self-hosting/workers.md#worker-api-key).

## Roles and permissions

The roles are ordered: `user` < `editor` < `admin`. A higher role can do everything that a lower role can do.

Each user has one team-wide role, and can also have a role for one factory. For a request about a factory, the server uses the factory role if one exists, and else the team-wide role. A request is about a factory when its path names the factory, or a work item, inbox item, webhook, benchmark run or run of the factory.

| Action | user | editor | admin |
|---|---|---|---|
| View factories, definitions, agents, automations, work items, runs, inbox, metrics, workers, machines, secret metadata | Yes | Yes | Yes |
| Start runs and send sandboxes to a factory | Yes | Yes | Yes |
| Send messages to work items, cancel or complete them, resolve inbox items | Yes | Yes | Yes |
| Validate a definition, test webhook filters | Yes | Yes | Yes |
| Manage your own API keys and personal forge tokens | Yes | Yes | Yes |
| Update a factory, its definition, agents, automations and schedules; restore a definition version | No | Yes | Yes |
| Attach team integrations to a factory | No | Yes | Yes |
| Change a work item's stage, score a run, start or cancel benchmarks, start Self-improvement, decide definition proposals | No | Yes | Yes |
| Create, update and delete webhooks; rotate webhook secrets | No | Yes | Yes |
| Create a factory (team-wide role only) | No | Yes | Yes |
| Create, update and delete secrets, environments and MCP servers (team-wide role only) | No | Yes | Yes |
| Delete a factory | No | No | Yes |
| Create and update users, their roles and their factory roles (team-wide role only) | No | No | Yes |
| Create, update and delete integrations (team-wide role only) | No | No | Yes |
| Create or revoke API keys of other users (team-wide role only) | No | No | Yes |
| Manage service accounts and their keys (team-wide role only) | No | No | Yes |

"Team-wide role only" means that a factory role does not count, because these actions create or change team resources. An editor of one factory can still attach existing team resources to that factory: an integration on the factory's Integrations page, and a secret, environment or MCP server by name in its definition.

The `user` role is not read-only: a user can start runs, send messages, cancel work and resolve inbox items. The core has no read-only role.

The dashboard and the API use the same minimum role for each action. The dashboard hides or disables an action that the API would refuse. A request with too low a role fails with `403` (`not_authorized`). A request without a valid key fails with `401` (`authentication_required`).

Roles do not give access to repositories or external services: a run's access depends on its forge credentials, secrets and MCP servers. Roles also do not control merges: your forge's branch protection rules do.

An admin creates a user with `POST /api/v1/users` (see [Create and revoke keys](api.md#create-and-revoke-keys)). Set or remove factory roles with `PATCH /api/v1/users/{id}`. A role sets the override, and `null` removes it:

```bash
curl -sS -X PATCH "${OF_PUBLIC_URL}/api/v1/users/usr_123" \
  -H "Authorization: Bearer $OF_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"factory_roles": {"fac_payments": "editor", "fac_old": null}}'
```

`GET /api/v1/me` returns the team-wide `role` and the `factory_roles` of the caller.

### Approvals and follow-ups

Roles do not decide who can approve. The foreman's instructions decide when it asks for an approval. By default, every team member who can sign in can resolve an approval, including an OIDC user with the default role.

- **`approvers`** in `factory.yaml` limits approvals to a list of users (by email) and service accounts (by name). The apply fails if an entry is not a user or service account of the team. The list applies to the dashboard, the API, chat buttons, and replies in chat and forge threads. Other members can still answer questions.
- **`restrictFollowupsToStarter: true`** limits trusted messages to the user who started the work item. Another member gets `403` through the dashboard, the API or Factory MCP, and a message from them in a thread goes to the foreman as untrusted input. They can still resolve inbox items. A work item without a creator trusts no event text; a member then answers in the dashboard or through the API.
- Each inbox item records who resolved it (`resolved_by`) and through which tool (`resolved_via`: `dashboard`, `api`, `mcp`, `chat` or `forge`).

If approvals must stay with a small group, set `approvers`, limit who can sign in (`OF_OIDC_ALLOWED_DOMAINS`, or the application assignment of your identity provider), and protect merges with branch protection rules.

### Sender trust

Text from a chat, forge or tracker can come from an attacker: on a public repository anyone can comment, and in a Slack channel every member can reply. The engine checks the sender of each event before the event continues a work item, and sets `Actor.Trust`:

| Trust | Sender |
|---|---|
| `member` | An account linked to an active user of the factory's team. Links come from [account linking](integrations/README.md#account-linking). |
| `external` | A person or bot without such a link, whatever its name. A custom webhook delivery is also `external`: its authentication proves the sending system, not a team member. |
| `platform` | A schedule, or the control plane itself. |

For a message that continues a work item:

| Sender | The text | Can resolve an open request |
|---|---|---|
| `member`, and no `approvers` list or in `approvers` | Goes to the foreman as trusted input. | Approvals and questions. |
| `member`, not in `approvers` | Goes to the foreman as trusted input. | Questions only. |
| `external`, or a bot | Goes to the foreman in a section marked as untrusted. The text cannot approve, answer an open request or change the agent's instructions. | Nothing. A reply such as "lgtm" from an unlinked account never resolves an approval. |

- **`requireLinkedActor: true`** drops each forge, chat or tracker event whose sender is `external`. The default is `false`. It does not apply to custom webhooks, schedules, the API or MCP.
- **Mentions.** Only a linked team member can start work with a mention. On GitHub, an unlinked account gets a one-time notice about linking.
- **Automations.** A matching automation does not need a link. The run acts as the agent, and the event text is not trusted.
- **Slack buttons.** The person who presses a button must have a linked account, and `approvers` applies to approval buttons.
- **Timeline.** Each message writes a `sender_checked` entry on the work item timeline: the provider, the handle, the matched user, the trust, how the engine identified the sender, and the result (`approval_approved`, `approval_rejected`, `answer`, `delivered` or `delivered_untrusted`).

## Agents never merge

An agent opens a pull request, and a person merges it. The platform enforces this where it can, independent of the prompt.

- The control plane reads each MCP request that a run sends through the MCP proxy (`/mcp/proxy/...`) or a built-in integration server (`/mcp/integrations/...`). It refuses each `tools/call` of a merge tool. The agent gets an error result that explains the rule, and the request does not reach the server.
- A merge tool is a tool whose name merges a pull or merge request, or makes the forge merge later, for example `merge_pull_request`, `merge_merge_request`, `accept_merge_request`, `complete_pull_request`, `enable_auto_merge` and `add_to_merge_queue`. Read-only names such as `get_merge_request` are not refused.
- Each merge call is an audit event with the action `agent.merge`. A refused call has the outcome `failure`.
- `allowAgentMerge: true` in `factory.yaml` allows merges by the agents of that factory. Each merge call is still an audit event, with the outcome `success`.

Limits:

- The guard sees only MCP traffic through the control plane. It does not see calls to a `direct` server or an inline `url` server. Register forge MCP servers and keep `connection: proxy`.
- The guard checks tool names. A generic tool that sends any API request, or that changes a pull request through an argument, is not refused.
- A run has a forge credential for git push. On most forges, that credential can also merge through the forge API:

| Forge | Can the run's credential merge? | What to do |
|---|---|---|
| GitHub | Yes. A merge and a push both need `contents: write`, and an installation token cannot have one without the other. | Protect the default branch with a rule that needs an approving review by a person. Do not add the app to the bypass list. |
| GitLab | Only if the token's role can merge into the branch. | Use a project access token with the `Developer` role, and set **Allowed to merge** of protected branches to `Maintainers`. |
| Azure DevOps | Yes. A token that can push can also complete a pull request. | Add a branch policy with a minimum number of reviewers, and do not let the token's identity bypass policies. |
| Generic git | No API, but a push to the default branch is a merge. | Protect the branch on the git server. |

Without these branch rules, the platform cannot stop a merge that an agent makes outside MCP.

## Spend limits

A spend limit stops model spend before a loop of runs or a long run uses many tokens.

- A factory sets `spendLimit.daily` and `spendLimit.monthly` in `factory.yaml`, in US dollars. See [Definition reference](definition-reference.md#factoryyaml).
- `OF_TEAM_SPEND_LIMIT_DAILY` and `OF_TEAM_SPEND_LIMIT_MONTHLY` set a limit for each team. See [Configuration](self-hosting/configuration.md#spend-limits).
- Days and months are UTC calendar periods. The spend of a period is the sum of `usage.cost_usd` of the runs created in it. A run that continues into the next period keeps its cost in the period of its creation.
- At 80 percent of a limit, each team admin gets an inbox item of kind `spend_limit`, once per period.
- When a limit is reached:
  - A new run, or a run with a new message, waits in `queued` with `queued_reason: spend_limit` until the next period. The queue timeout does not apply.
  - At a turn end over the limit, the control plane stops the whole run tree. Each run goes to `error` with the failure code `spend_limit_exceeded`. The same check runs before a new turn of a live session.
- The factory overview in the dashboard shows the spend and each limit. The API is `GET /api/v1/factories/{uid}/spend`.

The cost of a run is the estimate that its harness reports, not an invoice. A turn in progress can pass the limit before the check at its end.

## Secrets at rest

The secret store uses AES-256-GCM envelope encryption.

- Each secret value is encrypted with a new random 256-bit data key, and the data key is encrypted with the master key `OF_SECRET_KEY`.
- Both layers use the secret's team, factory scope, name and type as additional authenticated data, so a ciphertext moved to another record fails to decrypt.
- The database sees only ciphertext. Secret values are never logged, and the API never returns them.

## Secrets in runs

- Secrets reach the sandbox as environment variables and files. The runner fetches them from the control plane after the sandbox starts, so they are not in command arguments, Pod specs or Kubernetes audit logs. The Pod spec holds only the run ID, the session number, the control URL and a bootstrap token for the session.
- Repository clone URLs with credentials go through the environment, not through `git clone` arguments.
- Setup errors are scrubbed: known secret values and credentials in clone URLs become `***`.
- Names that start with `OF_` are reserved for platform variables.

See [Secrets](secrets.md) for secret types and templates.

### Redaction

Open Factory replaces the secret values of a run with `***` in its output (code: `internal/redact`). The redactor knows the run's secrets, the harness credentials, the header and variable values of MCP servers, the session token and the forge tokens. For each value it matches:

- the raw value and the JSON-escaped value;
- the standard and URL-safe base64 forms, with and without padding;
- the URL-encoded forms.

The control plane ignores values with fewer than 8 characters, because they would match too much ordinary text. The runner also redacts shorter values, but only in raw and JSON-escaped form.

The runner redacts each event before it leaves the sandbox. Text that an agent posts with `post_update`, `ask_human` and `request_approval` is redacted before it reaches a person or a tracker.

Limits: a secret that an agent encodes another way (for example hex) or splits into parts passes. Redaction does not change artifact files or snapshots. Narrow permissions and rotation stay necessary.

## Model credentials

The control plane makes no model calls and holds no model key. Each harness CLI calls the model provider from the sandbox, with the factory's model credential (`harness.auth`) in its environment. So an agent command can read the credential.

- Use a model key for each factory or team, not a shared personal key.
- Set spend and usage limits at the provider.
- Rotate the key when a run might have exposed it.
- With `auth.source: workerEnvironment`, the credential comes from the self-hosted worker. The sandbox can read it too.

## Sandbox isolation

| Backend | Isolation |
|---|---|
| `local` | Development and tests only. A separate workspace directory, but processes share the host kernel, file system and network. Sandboxes get only `HOME`, `TMPDIR` and `PATH` from the host, plus run variables. |
| `kubernetes` | A separate Pod per run. Use Pod Security admission, a restricted sandbox service account and NetworkPolicy. |

For agents that process untrusted input (public issues, external pull requests, chat from many users), use `kubernetes` and a restricted `networkPolicy`. The backend then creates a NetworkPolicy and a proxy sidecar for each sandbox, and the cluster network plugin must enforce NetworkPolicy. Set `OF_K8S_NETWORK_POLICY_ENFORCEMENT=require` to fail restricted runs on a cluster that does not enforce policies, and consider the optional egress lock. See [Network policy](self-hosting/network-policy.md#kubernetes).

## Data boundaries

All data stays in your infrastructure, except data that you send to external services.

| Data | Where it goes |
|---|---|
| Definitions, work items, runs, run messages (the transcript), inbox items, secrets | Your database. |
| Artifacts, raw harness transcripts, prompt attachments | Your [object storage](self-hosting/object-storage.md) (S3, GCS, Azure Blob or local disk). Clients read them through short-lived presigned URLs. |
| Repository checkouts, build output | The sandbox (a Pod in your cluster, or the host with the `local` backend). It is removed when the run ends. |
| LLM prompts and responses | The model provider of the harness, called from the sandbox. A scorer's judge is also a run. The control plane does not call a model provider. |
| Replies and status updates | The forge, chat or tracker integrations that you configure. |

Code can appear in transcripts and LLM prompts.

## Inbound requests

- Forge and chat integrations verify the provider's signature or token. See [Integrations](integrations/README.md).
- Custom webhooks support token, URL token and signature authentication. See [Webhooks](webhooks.md).
- The server listens on plain HTTP at `OF_LISTEN`. Put TLS in front of it.

## Hardening checklist

1. Set a stable `OF_SECRET_KEY` from a secret manager.
2. Serve the control plane only over HTTPS.
3. Bootstrap the first admin at once. Give other users the lowest role that they need.
4. Use a dedicated user and key for each remote worker. Revoke unused keys.
5. Use the `kubernetes` backend outside development, never `local`.
6. Use restricted network policies, and enforce them at the network level. On Kubernetes, only a NetworkPolicy object or your CNI enforces them.
7. Scope secrets to the factories that need them.
8. Restrict access to the server's Kubernetes API credentials. The Helm chart grants access only to Jobs, Pods, Pod logs, events and NetworkPolicies in the sandbox namespace.
9. Use OIDC single sign-on, so that access follows your identity provider.
10. Install only plugins that you trust. A plugin does not get the server's environment, but a sandbox backend plugin gets the bootstrap token of each session that it starts.
11. Give sign-in access only to people who may approve agent requests, or set `approvers`. Every role can resolve inbox items.
12. Configure the sign-in of each provider on its integration record (`oauth_client_id`), so that each account link has a proof from the provider. See [Account linking](integrations/README.md#account-linking).
13. Give each integration token access only to the repositories and projects that the factories need. With a GitHub App, the server limits each installation token to the run's repositories. For a static token, create a record for each factory with its own limited token. See [Credential isolation between factories](integrations/README.md#credential-isolation-between-factories).
14. Protect the default branch of each repository with a required review by a person, and keep `allowAgentMerge` off. See [Agents never merge](#agents-never-merge).
15. Set a spend limit for each factory, or for each team. See [Spend limits](#spend-limits).

## Related pages

- [Self-hosting overview](self-hosting/README.md)
- [Configuration](self-hosting/configuration.md)
- [Secrets](secrets.md)
- [Network policy](self-hosting/network-policy.md)
