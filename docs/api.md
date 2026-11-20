# REST API and Go SDK

Use the REST API to send work to factories, start and steer runs, and resolve inbox items from scripts, CI jobs and services. The dashboard, the [Factory MCP server](factory-mcp.md) and the Go SDK use the same API keys, roles and error format.

## Base URL and OpenAPI document

All endpoints are under `/api/v1`. In the examples, `OF_PUBLIC_URL` is the external URL of your server (default `http://localhost:8080`) and `OF_API_KEY` is an API key.

```bash
export OF_PUBLIC_URL=https://factories.example.com
export OF_API_KEY=of_abcd1234_...
curl -s "$OF_PUBLIC_URL/api/v1/me" -H "Authorization: Bearer $OF_API_KEY"
```

The full schema is an OpenAPI 3.1 document, served at `GET /api/v1/openapi.json` (no authentication). The source is [`internal/api/openapi.json`](../internal/api/openapi.json). See [OpenAPI document](#openapi-document).

### Path style

- Collections have plural names at the top level: `/factories`, `/runs`, `/work-items`, `/webhooks`.
- A sub-resource is below its parent: `/factories/{uid}/webhooks`.
- An action is a `POST` on a sub-path: `/runs/{runId}/cancel`.

Bodies are JSON. The maximum request body is 8 MiB. Each response has an `X-Request-Id` header. If you send `X-Request-Id`, the server uses your value.

## Authentication

Send a bearer API key with each request:

```
Authorization: Bearer of_<prefix>_<secret>
```

These endpoints need no key: `GET /openapi.json`, `POST /bootstrap`, `GET /factory-files/schemas[/{version}[/{document}]]`, `GET /login-providers` and `GET /version`.

The server shows a key one time, when you create it. If you lose a key, delete it and create a new one. For the key format and storage, see [API keys](security.md#api-keys).

### Get the first key

A new server has no team and no users. Bootstrap it one time to create the first team, its admin user and an API key:

```bash
curl -s -X POST "$OF_PUBLIC_URL/api/v1/bootstrap" \
  -H "Content-Type: application/json" \
  -d '{"team_name": "Acme", "admin_email": "admin@acme.dev", "admin_name": "Ada", "key_name": "laptop"}'
```

The response has `team`, `user`, `api_key` (the stored record) and `key` (the plain-text key). `key_name` is optional (default `bootstrap`). When a team exists, the endpoint returns `409 conflict`. `openfactory bootstrap` does the same and prints an `export OF_SERVER=... OF_API_KEY=...` line.

### Create and revoke keys

```bash
curl -s -X POST "$OF_PUBLIC_URL/api/v1/api-keys" \
  -H "Authorization: Bearer $OF_API_KEY" -H "Content-Type: application/json" \
  -d '{"name": "ci"}'
```

- Any role can create a key for itself. `name` is required, 1 to 100 characters.
- An admin can create a key for another user with `"user_id": "usr_..."`.
- `GET /api-keys` lists the team's keys. `DELETE /api-keys/{id}` revokes one.
- To add a user, an admin calls `POST /users` with `email`, `name`, `role` and, optionally, `"create_api_key": true`.

A key acts as its owner, in the owner's team, with the owner's role. The owner is a user (prefix `of_`) or a [service account](#service-accounts) (prefix `ofa_`).

### Service accounts

A service account is a principal that is not a person, for example a CI job or a bot. It has a role and API keys, but no email and no sign-in. Only an admin can manage service accounts.

```bash
# The response shows the first key one time.
curl -s -X POST "$OF_PUBLIC_URL/api/v1/service-accounts" \
  -H "Authorization: Bearer $OF_API_KEY" -H "Content-Type: application/json" \
  -d '{"name": "ci", "role": "user", "create_api_key": true}'
```

| Endpoint | Purpose |
|---|---|
| `GET\|POST /service-accounts/{id}/keys` | List keys, or add one (`{"name": "deploy"}`). |
| `DELETE /service-accounts/{id}/keys/{keyId}` | Revoke a key. |
| `PATCH /service-accounts/{id}` | Change the role, for example `{"role": "editor"}`. |
| `DELETE /service-accounts/{id}` | Delete the account and its keys. |

- `{id}` is the ID (`sa_...`) or the name.
- `name` is 1 to 64 characters, without spaces, `/` or `:`, and unique in the team. An automation uses it with `runAs: serviceAccount:<name>`.
- `role` is `user` (default), `editor` or `admin`. `factory_roles` works as for users.
- A run that a service account key starts has the service account as `creator` and `run_as`. With `credentialStrategy: CREATOR`, such a run uses the factory credential.
- `GET /users` does not list service accounts. `POST /api-keys` cannot create a key for a service account, and a service account cannot create its own keys.

## Roles

A higher role includes the permissions of the lower roles.

| Role | Can do |
|---|---|
| `user` | Read everything in the team. Send work, start and steer runs, message and cancel work items, resolve inbox items, create own API keys. |
| `editor` | Create and update factories, put definitions, set a work item's stage, manage webhooks, secrets, environments and MCP servers, start benchmarks. |
| `admin` | Delete factories, manage users, service accounts and integrations, create API keys for other users. |

- Each user has one team-wide role, and can also have a role for one factory (`factory_roles`). A request is about a factory when its path has a factory, work item, inbox item, webhook, benchmark run or run ID. For such a request, the factory role counts if it exists.
- Only the team-wide role counts to create a factory, and to create or change team resources: secrets, environments, MCP servers, integrations, users and other users' API keys.
- An admin sets a factory role with `PATCH /users/{id}` and `{"factory_roles": {"fac_123": "editor"}}`. `null` removes it.
- `x-required-role` on each operation in the OpenAPI document is the minimum role. The dashboard uses the same minimums.
- Roles do not decide who can approve a spec or merge a pull request. See [Approvals and follow-ups](security.md#approvals-and-follow-ups).

A role that is too low gets `403 not_authorized`. Resources of other teams give `404 resource_not_found`. See [Security](security.md).

## Send work to a factory

`POST /api/v1/factories/{uid}/runs` creates a work item and starts the foreman (or another agent). It is the API form of a message to the factory.

```bash
curl -s -X POST "$OF_PUBLIC_URL/api/v1/factories/fac_123/runs" \
  -H "Authorization: Bearer $OF_API_KEY" -H "Content-Type: application/json" \
  -d '{
    "prompt": "Investigate and fix the flaky payment webhook retry test",
    "title": "Fix flaky payment webhook retry test",
    "ticket_ref": "linear:PAY-123",
    "metadata": {"ci.job": "4512"}
  }'
```

| Field | Meaning |
|---|---|
| `prompt` | Required. The request. |
| `title` | Work item title. Default: the first line of `prompt`, cut at 80 characters. |
| `ticket_ref` | `<source>:<id>`, for example `linear:PAY-123`. Links the work item to that ticket. |
| `ticket_url` | Link to the ticket. |
| `source` | A full source reference: `provider`, `kind`, `url`, `repo`, `number`, `channel`, `thread_id`, `external_id`, `title`. |
| `agent` | Agent name. Default: the foreman. |
| `metadata` | See [Run metadata](#run-metadata). |
| `overrides` | Execution overrides: `harness`, `model`, `machine`, `environment_id`, `secrets`, `mcp_servers`, `worker_host`, `idle_timeout_minutes`. |

The response (`201`) is a run object plus `work_item_url` and `work_item_id`. To find a factory UID, call `GET /api/v1/factories?search=payments` (matches name or alias).

## Start a run of an agent

`POST /api/v1/runs` starts a run of one agent in a new work item. Name the agent with `agent_identity_uid`, or with `factory_id` and `agent_name`.

```bash
curl -s -X POST "$OF_PUBLIC_URL/api/v1/runs" \
  -H "Authorization: Bearer $OF_API_KEY" -H "Content-Type: application/json" \
  -d '{
    "prompt": "Review the latest changes and report missing tests. Do not modify files.",
    "factory_id": "fac_123",
    "agent_name": "review",
    "config": {"environment_id": "env_123", "idle_timeout_minutes": 10}
  }'
```

| Field | Meaning |
|---|---|
| `prompt` | Required. The work to do. |
| `title` | Work item title. |
| `agent_identity_uid` | The UID of an agent. |
| `factory_id`, `agent_name` | A factory UID or alias, and an agent of that factory. |
| `metadata` | See [Run metadata](#run-metadata). |
| `config.harness` | Overrides the agent's harness. `auth` is `source: managedSecret` with `secretName`, or `source: workerEnvironment` (needs a self-hosted `worker_host`). |
| `config.model_id` | Overrides the model, when you do not give `harness`. |
| `config.environment_id` | An environment of your team. |
| `config.machine` | A machine name. |
| `config.worker_host` | `server` (default) or a self-hosted worker ID. |
| `config.mcp_servers` | Extra MCP servers. Each sets exactly one of `managedId`, `command` or `url`. `allowWrite` is valid only with a built-in `managedId`. See [MCP servers](mcp-servers.md). |
| `config.secrets` | Extra secret names to inject. |
| `config.idle_timeout_minutes` | 0 to 60. |

See [Harnesses](harnesses.md), [Machines and environments](machines-and-environments.md) and [Secrets](secrets.md).

## Monitor a run

### Run states

`state` is the API state. `phase` is the internal state.

| `state` | `phase` | Meaning |
|---|---|---|
| `QUEUED` | `queued` | Waits for a worker slot. |
| `PENDING` | `pending`, `starting` | A backend prepares the sandbox. |
| `CLAIMED` | `claimed` | The runner does its setup. |
| `RUNNING` | `running` | The agent works. |
| `BLOCKED` | `blocked` | The last turn ended with an open request. See `blocked_reason`. |
| `SUCCEEDED` | `succeeded`, `idle` | Finished. A new message can continue it. |
| `FAILED` | `failed` | Terminal. Stopped with an error (see `status_message`). |
| `ERROR` | `error` | Terminal. The platform ended the run. |
| `CANCELLED` | `cancelled` | Terminal. A person or the system cancelled it. |

### Run object

| Field | Meaning |
|---|---|
| `run_id`, `title`, `factory_id`, `work_item_id`, `agent_name`, `metadata`, `result` | Identity and result. |
| `url` | The run in the dashboard. |
| `created_at`, `started_at`, `finished_at`, `updated_at` | Times. |
| `blocked_reason` | `human_request` or `harness_needs_input`, when `BLOCKED`. |
| `sandbox_running` | `true` when the newest session is assigned or active. Always `false` without a runner store. |
| `status_message` | `message`, `failure_code`, `retryable` (the platform retries this failure code) and `detail`. Absent when there is nothing to report. |
| `error` | The error text (the old string form of `status_message`). |
| `root_run_id`, `parent_run_id`, `depth` | Place in the run tree (at most 10 levels up). |
| `creator`, `run_as` | Principals, each `{"type", "id", "name"}`; `type` is `user`, `service_account` or `factory`. No `creator` for a schedule, webhook, scorer or benchmark. See [Run principals](definition-reference.md#run-principals). |
| `active_child_runs` | Runs under this run that are not finished (`idle` and `blocked` count as not finished). Reads at most 1000 runs and 10 levels. |
| `session` | Number of the newest session. `0` without a session or runner store. |
| `usage` | `input_tokens`, `output_tokens`, `cost_usd`, `duration_ms`, `coverage` (`known`). |
| `judge_usage` | Usage of the judge runs of the run's scores, added to its cost. `GET /runs/{runId}` returns it when the run has scores. |
| `source`, `source_ref` | How the run started (below), and the ID of its automation, schedule, webhook, parent run, scorer or benchmark. |

| `source` | The run started from |
|---|---|
| `API`, `CLI`, `DASHBOARD`, `MCP` | An API key client, the `openfactory` CLI, the dashboard, the Factory MCP server. |
| `AUTOMATION` | An automation with a forge, chat or tracker trigger, or a manual run of an automation. |
| `SCHEDULE` | An automation with a schedule trigger. |
| `WEBHOOK` | An automation that a custom webhook started. |
| `MENTION` | A mention of the factory, or a new event on a tracked issue or pull request. |
| `ORCHESTRATION` | A parent run (`run_agents`). |
| `SCORER`, `BENCHMARK` | A scorer judge run, or a benchmark trial or judge run. |
| `SELF_IMPROVEMENT` | A self-improvement run. |

A run from an older version has no stored source. The API works it out from old data, so an old schedule or webhook run shows `AUTOMATION`.

### Get and list runs

```bash
curl -s "$OF_PUBLIC_URL/api/v1/runs/run_123" -H "Authorization: Bearer $OF_API_KEY"
curl -s "$OF_PUBLIC_URL/api/v1/runs?factory_id=fac_123&state=FAILED,CANCELLED&since=7d" \
  -H "Authorization: Bearer $OF_API_KEY"
```

Filters: `factory_id`, `work_item_id`, `parent_run_id`, `agent_name`, `name`, `creator` (an ID or `me`), `source`, `model`, `state`, `root_run_id`, `ancestor_run_id`, `since`.

- `state`, `source` and `model` take comma-separated or repeated values. `SUCCEEDED` includes the `idle` phase.
- `since` is an RFC 3339 time, a date (`2026-11-01`) or a duration (`24h`, `7d`, `2w`).
- `root_run_id` returns the named run and each run under it. `ancestor_run_id` returns the same set without the named run. You cannot use both. Each reads at most 1000 runs and 10 levels. An unknown run gives `404 resource_not_found`.

### Read the transcript

`GET /api/v1/runs/{runId}/messages` returns the messages in order. Each has a `seq` and a `role` (`user`, `assistant`, `tool_call`, `tool_result`, `system`, `log`, `artifact`). For runs from older versions, the server also reads older messages from object storage.

| Query | Meaning |
|---|---|
| `after` | Only messages with a higher `seq`. |
| `limit` | Default 200, maximum 1000. If there are more, the response has `has_more: true` and `next_after`. |
| `role` | Only these roles (comma-separated). |

`GET /runs/{runId}/transcript` returns the same messages as JSON Lines (`application/x-ndjson`), to download a whole run. The runner replaces secret values with `***` before the server stores messages.

### Run details

These endpoints follow the access rules of `GET /runs/{runId}`.

| Endpoint | Result |
|---|---|
| `GET /runs/{runId}/conversation` | The normalized conversation in steps, messages and blocks. `scope=final_message` returns only the last assistant message. |
| `GET /runs/{runId}/timeline` | Events in time order: `event_id`, `run_id`, `session`, `type`, `at`, `data`. |
| `GET /runs/{runId}/diagnostics` | `code`, `message`, `data`, `at`, `session`. |
| `GET /runs/{runId}/sessions` | `number`, `state`, `assigned_at`, `started_at`, `ready_at`, `ended_at`, `end_reason`, `sandbox_reason`, `usage`, `snapshot`. |
| `GET /runs/{runId}/inputs` | The message queue: `message_id`, `seq`, `kind`, `sender`, `state`, `content`, `data`, `attachments`, `untrusted`, `dedupe_key`. |

- `timeline`, `diagnostics`, `sessions` and `inputs` need the runner store. Without it they return `501 feature_not_available`.
- Timeline types: `run.created`, `session.started`, `setup.step`, `snapshot.restored`, `session.ready`, `turn.started`, `turn.ended`, `snapshot.saved`, `session.ended`. A finished run ends with `run.succeeded`, `run.failed`, `run.error` or `run.cancelled`.
- A session's `snapshot` has the flags `workspace` and `harness_state`: `true` when the session saved that part.
- An input's `state` is `pending`, `delivered` or `consumed`. Filter with `state`; `after` returns inputs with a higher `seq`.

### Stream events

`GET /api/v1/runs/{runId}/events` is a Server-Sent Events stream:

| Event | Data | When |
|---|---|---|
| `run` | Run object | First, then at each state change. |
| `message` | Message (`id:` is its `seq`) | At each new transcript message. |
| `done` | Run object | The run is terminal. The stream ends. |
| `error` | `{"message": "..."}` | The server cannot read the transcript. |

The server sends a `: ping` comment every 15 seconds. To resume, send `Last-Event-ID` with the last `seq`, or `?after=<seq>`. `?stop_on_idle=true` ends the stream when the run goes idle.

```bash
curl -N "$OF_PUBLIC_URL/api/v1/runs/run_123/events?stop_on_idle=true" \
  -H "Authorization: Bearer $OF_API_KEY"
```

`GET /stream` is the team-wide live feed of the dashboard: `ready`, `run`, `message`, `inbox`, `workitem` and `definition` events.

## Follow-ups and cancellation

A message continues the same agent session:

```bash
curl -s -X POST "$OF_PUBLIC_URL/api/v1/runs/run_123/messages" \
  -H "Authorization: Bearer $OF_API_KEY" -H "Content-Type: application/json" \
  -d '{"message": "Also add a regression test for the retry backoff"}'
```

- `message` is required. `attachments` is an optional list of attachment IDs (upload with `POST /attachments`, at most 25 MiB).
- The response is `202` with `run_id`, `message_id` (the queue entry and stored message) and `outcome`: `live_session`, `queued` (waits for a session) or `new_session`.
- The first prompt is the first queue entry. A message that arrives before the first turn comes after it, and the first turn gets both.
- A message to a cancelled run continues it in a new session.
- `POST /work-items/{id}/messages` sends to the foreman and answers with `work_item_id`, `run_id`, `message_id` and `outcome`. `POST /runs/{runId}/followups` does the same and answers with the run object.

`POST /api/v1/runs/{runId}/cancel` stops a run. The response is `200` with `run_id`.

- The optional body has `cascade` (default `true`: also cancel the runs under it).
- A cancel is accepted in each state. A finished run keeps its state, and its live session ends.

`POST /runs/{runId}/interrupt` stops the active turn and keeps the session.

## Work items and the inbox

A work item is one request from intake to completion.

| Endpoint | Body or query | Role |
|---|---|---|
| `GET /factories/{uid}/work-items` | Query: `stage`, `creator` (`me` or a user ID), `search`, `since`, `limit`, `cursor` | user |
| `GET /work-items/{id}` | Returns the work item with `timeline`, `runs` and `inbox` | user |
| `POST /work-items/{id}/messages` | `{"message": "..."}` to the foreman | user |
| `POST /work-items/{id}/cancel` | `{"reason": "..."}` | user |
| `POST /work-items/{id}/complete` | `{"note": "..."}` | user |
| `POST /work-items/{id}/stage` | `{"stage": "...", "note": "..."}` | editor |

Stages: `intake`, `triage`, `planning`, `awaiting_approval`, `building`, `reviewing`, `handoff`, `complete`, `cancelled`.

### Inbox items

An inbox item is a notice about one run for the users in `recipients`. The default recipient is the creator of the work item. An item without recipients is for the factory's editors and admins.

List items with `GET /inbox` (filter with `factory_id`) or `GET /factories/{uid}/inbox`:

- By default you get the items where you are a recipient, plus items without recipients if you are a factory editor or admin.
- `scope=team` lists all items. It needs editor or admin; a user gets `403 not_authorized`.
- Other filters: `include_resolved`, `unread`, `kind` (comma-separated), `search`.

| Kind | What it is |
|---|---|
| `question` | The agent asks something (`ask_human`). |
| `approval` | The agent asks for a decision (`request_approval`). |
| `handoff` | A result or pull request to review. Closes when a review is submitted at the forge or the pull request merges. |
| `review` | A proposed definition change, or a self-improvement pull request. |
| `failure` | A root run failed. |
| `blocked` | A root run stopped and waits for input. |
| `answered` | Information only. |
| `spend_limit` | 80 percent of a spend limit is used. Goes to each admin, has no run. |

The platform creates a `failure` or `blocked` item only when the work item has no other open item for the same people.

`is_read` is your read state. `POST /inbox/{id}/read` marks an item read; `{"read": false}` marks it unread.

A question item has `questions`, each with `question_id`, `text`, `type` (`single_select`, `multi_select` or `text`), `options` and an optional `recommended_option`.

### Resolve an item

```bash
curl -s -X POST "$OF_PUBLIC_URL/api/v1/inbox/inbox_123/resolve" \
  -H "Authorization: Bearer $OF_API_KEY" -H "Content-Type: application/json" \
  -d '{"resolution": "approve"}'
```

| Kind | `resolution` |
|---|---|
| `approval` | Required. If the item has `options`, one of them (case-insensitive). |
| `question`, `blocked` | Required. Your answer. |
| `review` of a definition change | Required. `approve` applies the version. `reject` or `reject: <reason>` rejects it and sends the reason to the agent. Needs `editor`. See [Review](measure-and-improve.md#review). |
| `handoff`, `failure`, `review` of a pull request, `answered`, `spend_limit` | Optional. Default `resolved`. |

A question item also accepts `answers`, one per question, in place of `resolution`. The API checks each choice against the options; a `single_select` question takes one choice:

```bash
curl -s -X POST "$OF_PUBLIC_URL/api/v1/inbox/inbox_123/resolve" \
  -H "Authorization: Bearer $OF_API_KEY" -H "Content-Type: application/json" \
  -d '{"answers": [{"question_id": "q1", "selected": ["Postgres"]}]}'
```

- The engine sends the resolution to the waiting agent as a user message. An `answered` item sends nothing.
- Resolving a resolved item returns `409 conflict`.
- The item records `resolved_by` and `resolved_via` (`api`, `dashboard` or `chat`).
- If `factory.yaml` has `approvers`, only a listed user can resolve an `approval`.
- If `factory.yaml` sets `restrictFollowupsToStarter: true`, only the creator of the work item can post messages or follow-ups to its runs.
- Other users get `403 not_authorized` in both cases.

`POST /inbox/{id}/unresolve` undoes a resolve within 10 minutes, while the run has not read the answer. The agent gets a message that withdraws the answer. Otherwise, and for items the platform closed (for example a handoff after a merge), the API returns `409 conflict`. See [Dashboard and inbox](dashboard.md#inbox).

## Run metadata

`metadata` attaches your own key-value pairs to a run, for example `{"ci.job": "4512"}`. Each pair goes to the sandbox as an environment variable (`ci.job` becomes `OF_METADATA_CI_JOB`), and child runs inherit it. At most 20 keys; keys are 1 to 64 characters from `A-Z a-z 0-9 . _ -`; values are at most 256 bytes. See [Run metadata](agents.md#run-metadata) for all rules.

## Pagination

List endpoints use cursor pagination with one response form:

```json
{"items": [...], "page_info": {"has_next_page": true, "next_cursor": "MTczMDk..."}}
```

- `limit`: default 50, maximum 200.
- Send `next_cursor` as `cursor` to get the next page. When `has_next_page` is `false`, there is no `next_cursor`.
- `sort_by` is `created_at` (the only value). `sort_order` is `desc` (default) or `asc`. A cursor works only with the sort that returned it.
- Cursors are opaque. Do not build them.
- A run list filtered by `source` or `model` reads at most 20 store pages per request. A page can then be shorter than `limit` with `has_next_page: true`.

The transcript uses `after` and `next_after` instead.

## Errors

Every error is an RFC 7807 problem document (`application/problem+json`):

```json
{
  "type": "/errors/resource-not-found",
  "title": "The referenced resource does not exist.",
  "status": 404,
  "detail": "factory fac_missing not found",
  "instance": "/api/v1/factories/fac_missing",
  "code": "resource_not_found",
  "error": {"code": "resource_not_found", "message": "The referenced resource does not exist. (factory fac_missing not found)"},
  "retryable": false,
  "request_id": "4f1c..."
}
```

Use `code` in program logic; do not parse messages. `request_id` equals the `X-Request-Id` header; give it to your administrator to find the request in the logs.

Some errors add `reason` (a second code), `provider`, `auth_url` and `setup_url`. An invalid definition adds an `errors` list with `file`, `line`, `field` and `message`.

| `code` | Status | Retry | Meaning |
|---|---|---|---|
| `invalid_request` | 400 | No | A parameter is missing or not valid. Read `detail`. |
| `authentication_required` | 401 | No | The key is missing, not valid or deleted. |
| `external_authentication_required` | 401 | No | The run needs your account at `provider`, and you did not link it. `auth_url` is where you store a personal token. `reason` is `no_linked_account`. |
| `not_authorized` | 403 | No | Your role is too low. |
| `resource_not_found` | 404 | No | The resource does not exist, or belongs to another team. |
| `conflict` | 409 | No | The resource changed, a resource with the same name exists, or its state blocks the request. Read `detail`, read the resource again and change the request before you send it again. |
| `operation_not_supported` | 422 | No | Not possible for this resource or state, for example a definition change on a git-backed factory. |
| `integration_not_configured` | 424 | No | The integration setup is not complete. |
| `integration_disabled` | 424 | No | The integration is disabled. |
| `internal_error` | 500 | Yes | An unexpected server error, logged with the request ID. |
| `feature_not_available` | 501 | No | The feature is not configured on the server, for example metrics or benchmarks. |
| `resource_unavailable` | 503 | Yes | A required resource is temporarily not available, or the request timed out. |

## Endpoint reference

Paths are relative to `/api/v1`. The OpenAPI document has the full schemas.

| Area | Endpoints |
|---|---|
| Discovery | `GET /openapi.json`, `POST /bootstrap`, `GET /factory-files/schemas[/{version}[/{document}]]`, `GET /login-providers`, `GET /version`, `GET /stream` |
| Identity | `GET /me`, `GET /me/forge-tokens`, `PUT\|DELETE /me/forge-tokens/{forge}`, `GET /teams`, `GET /teams/{id}`, `GET\|POST /users`, `GET\|PATCH /users/{id}`, `GET\|POST /api-keys`, `DELETE /api-keys/{id}`, `GET /mcp-sign-ins`, `DELETE /mcp-sign-ins/{id}` (see [Factory MCP](factory-mcp.md#sign-in-with-the-browser)), `GET\|POST /service-accounts`, `GET\|PATCH\|DELETE /service-accounts/{id}`, `GET\|POST /service-accounts/{id}/keys`, `DELETE /service-accounts/{id}/keys/{keyId}` |
| Factories | `GET\|POST /factories`, `GET\|PATCH\|DELETE /factories/{uid}`, `POST /factories/{uid}/runs`, `GET /factories/{uid}/settings`, `GET /factories/{uid}/agents`, `GET /factories/{uid}/agents/{name}`, `GET\|PUT /factories/{uid}/agents/{name}/spec`, `GET /factories/{uid}/automations`, `GET\|PUT\|DELETE /factories/{uid}/automations/{name}`, `POST /factories/{uid}/automations/{name}/validate`, `GET\|PUT /factories/{uid}/integrations`, `GET /factories/{uid}/spend` (see [Spend limits](security.md#spend-limits)), `GET /factories/{uid}/schedules`, `POST /factories/{uid}/schedules/{id}/pause\|resume`, `POST /automations/{id}/run` (see [Pause, resume and run now](automations.md#pause-resume-and-run-now)), `POST /factories/{uid}/test-filter`, `POST /factories/{uid}/self-improvement` |
| Definitions | `GET\|PUT /factories/{uid}/definition`, `POST /factories/{uid}/definition/validate`, `GET /factories/{uid}/definition/versions`, `GET /factories/{uid}/definition/versions/{versionId}`, `POST /factories/{uid}/definition/versions/{versionId}/restore`, `GET /factories/{uid}/definition/proposals`, `POST /factories/{uid}/definition/proposals/{id}/approve\|reject` (see [Review](measure-and-improve.md#review)) |
| Work items | `GET /factories/{uid}/work-items`, `GET /work-items/{id}`, `POST /work-items/{id}/messages\|cancel\|complete\|stage` |
| Inbox | `GET /inbox`, `GET /factories/{uid}/inbox`, `GET /inbox/{id}`, `POST /inbox/{id}/resolve\|read\|unresolve` |
| Agent runs | `GET\|POST /runs`, `GET /runs/{runId}`, `GET\|POST /runs/{runId}/messages`, `GET /runs/{runId}/transcript`, `GET /runs/{runId}/inputs\|events\|conversation\|timeline\|diagnostics\|sessions`, `POST /runs/{runId}/followups\|cancel\|interrupt`, `POST /attachments`, `GET\|POST /runs/{runId}/attachments`, `GET /runs/{runId}/attachments/{name}`, `GET /runs/{runId}/artifacts`, `GET /runs/{runId}/artifacts/{name}` |
| Webhooks | `GET\|POST /factories/{uid}/webhooks`, `GET\|PATCH\|DELETE /webhooks/{id}`, `POST /webhooks/{id}/rotate-secret`, `GET /webhooks/{id}/deliveries`, `GET /webhooks/{id}/deliveries/{deliveryId}`, `POST /webhooks/{id}/test-filter` |
| Measure | `GET /factories/{uid}/scorers`, `GET /factories/{uid}/scores`, `POST /runs/{runId}/score`, `GET /factories/{uid}/benchmarks`, `GET\|POST /factories/{uid}/benchmarks/{suite}/runs`, `POST /factories/{uid}/benchmarks/{suite}/tasks` (a task from a finished run, with `from_run_id`), `GET /factories/{uid}/benchmark-runs`, `GET /benchmark-runs/{id}`, `POST /benchmark-runs/{id}/cancel`, `GET /factories/{uid}/metrics` |
| Platform | `GET\|POST /secrets`, `GET\|PUT\|DELETE /secrets/{name}`, `GET\|POST /environments`, `GET\|PATCH\|DELETE /environments/{id}`, `GET /machines`, `GET /workers`, `GET /workers/{id}`, `GET /outbox/dead`, `POST /outbox/{id}/redrive` (admin: dead-lettered events), `GET\|POST /integrations`, `GET\|PATCH\|DELETE /integrations/{id}`, `GET /networking/policies`, `GET\|POST /mcp-servers`, `GET\|PATCH\|DELETE /mcp-servers/{id}`, `POST /mcp-servers/{id}/oauth/connect\|disconnect`, `GET /mcp-servers/oauth/callback` (see [MCP servers](mcp-servers.md#rest-api)) |

### Personal forge tokens

Runs with [`credentialStrategy`](definition-reference.md#credentialstrategy) `CREATOR` use the personal forge token of the person the run acts as (`run_as`). Without that token, the run does not fall back to the team credential: an API request gets `401`, and a run from an automation or a mention fails with `external_authentication_required`. Store your token with an API key of your user:

```bash
curl -sS -X PUT "${OF_PUBLIC_URL}/api/v1/me/forge-tokens/GITHUB" \
  -H "Authorization: Bearer $OF_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"token": "<personal access token>"}'
```

- `{forge}` is `GITHUB`, `GITLAB` or `AZURE_DEVOPS`. The response is `204`.
- `GET /me/forge-tokens` returns `{"forges": [...]}`: the forges with a stored token, never the tokens.
- `DELETE /me/forge-tokens/{forge}` removes a token.
- The token needs repository read and write access, for example a fine-grained GitHub token with **Contents** and **Pull requests** permissions.

### Create a factory from files

To create a managed factory from a local definition tree, send the files in `files` (path to content):

```bash
curl -s -X POST "$OF_PUBLIC_URL/api/v1/factories" \
  -H "Authorization: Bearer $OF_API_KEY" -H "Content-Type: application/json" \
  -d '{"name": "payments", "files": {"factory.yaml": "...", "agents/foreman/agent.md": "..."}}'
```

- To change it, call `PUT /factories/{uid}/definition` with `files`, an optional `message` and an optional `base_version_id`. If `base_version_id` is not the active version, the API returns `409 conflict`.
- A factory with a `git` source rejects this call. Merge the change to its production branch instead.
- `POST /factories/{uid}/definition/validate` checks files without saving them.

## Command-line client

The `openfactory` binary has a client for common API calls. Global flags:

| Flag | Environment variable | Default |
|---|---|---|
| `--server` | `OF_SERVER` | `http://localhost:8080` |
| `--api-key` | `OF_API_KEY` | None |
| `-o`, `--output` | | `text`. `json` prints the raw API response. |

A `<factory>` argument accepts the UID, alias or name. A `<prompt>` of `-` reads stdin. `-f` streams the transcript.

| Command | Subcommands |
|---|---|
| `bootstrap --email EMAIL` | Bootstraps a new server and prints the first key. |
| `factory` | `list`, `get <factory>`, `create` (default agents, the tree in `--from DIR`, or `--source git`), `apply <dir>` (to `--factory`, else the factory with the tree's alias or name, else a new one), `run <factory> <prompt>`. |
| `run` | `list`, `get <run>`, `logs <run> [-f]`, `followup <run> <message> [-f]`, `cancel <run>`. |
| `work-item` | `list <factory>`, `get <id>`, `send <factory> <prompt>`, `complete <id>`. |
| `inbox` | `list` (`--all` adds resolved items, `--team` all people), `resolve <id> <resolution>`, `unresolve <id>`. |
| `secret` | `set <name> [value]` (stdin when the value is missing or `-`), `list`, `delete <name>`. |
| `mcp-registry` | `list`, `get`, `add`, `delete`, `connect` (prints the OAuth URL), `disconnect`. See [Register a server](mcp-servers.md#register-a-server). |
| `webhook` | `list <factory>`, `create <factory> --name ...`, `deliveries <webhook-id>`. |
| `benchmark` | `run <factory> <suite>`, `list <factory>`. |
| `worker list` | Lists registered remote workers. |

Run `openfactory <command> --help` for all flags.

## Clients

### Go SDK

The CLI uses the Go SDK, `github.com/openfactory-org/openfactory/pkg/sdk` ([source](../pkg/sdk)).

- It retries with exponential backoff and obeys `Retry-After`.
- It retries `429` and `503` for each method, because the server did not process the request.
- It retries `500`, `502`, `504` and connection errors only for methods other than `POST` and `PATCH`, so it does not create an object two times.
- It gets the next pages for you and streams run events with automatic resume.

`sdk.NewClientFromEnv()` reads the base URL from `OF_API_URL`, then `OF_PUBLIC_URL` (default `http://localhost:8080`), and the key from `OF_API_KEY`.

```bash
go get github.com/openfactory-org/openfactory/pkg/sdk
```

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/openfactory-org/openfactory/pkg/sdk"
)

func main() {
	ctx := context.Background()
	client := sdk.NewClient("https://factories.example.com", os.Getenv("OF_API_KEY"))

	// Find the factory. All() fetches the next pages when necessary.
	var uid string
	for f, err := range client.Factories.List(ctx, &sdk.FactoryListOptions{Search: "payments"}).All(ctx) {
		if err != nil {
			log.Fatal(err)
		}
		uid = f.UID
		break
	}

	run, err := client.Factories.Dispatch(ctx, uid, sdk.DispatchParams{
		Prompt:    "Fix the flaky payment webhook retry test",
		TicketRef: "linear:PAY-123",
	})
	if err != nil {
		log.Fatal(sdk.ErrorCode(err), err)
	}

	// Stream the transcript until the run is terminal.
	st, err := client.Runs.Stream(ctx, run.RunID, nil)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()
	for st.Next() {
		ev := st.Event()
		if ev.Type == "message" {
			fmt.Println(ev.Message.Role, ev.Message.Content)
		}
	}
	if err := st.Err(); err != nil {
		log.Fatal(err)
	}
}
```

- Options: `WithHTTPClient`, `WithMaxRetries` (default 3), `WithBackoff` (default 500 ms to 8 s), `WithUserAgent`.
- `client.Runs.Wait(ctx, runID, interval, untilIdle)` polls until the run is finished. With `untilIdle`, it also returns at `BLOCKED`.
- Error helpers: `sdk.ErrorCode`, `sdk.IsNotFound`, `sdk.IsConflict`, `sdk.IsUnauthorized`, `sdk.IsForbidden`, `sdk.IsInvalid`.
- The Go documentation of `pkg/sdk` lists all services and methods.

### OpenAPI document

The OpenAPI 3.1 document is the contract for clients in other languages and for the dashboard's TypeScript types (`make web-check-api`).

- [`internal/api/openapi.json`](../internal/api/openapi.json) is written by hand. `TestOpenAPICoversRoutes` makes sure that it has each server route.
- The binary embeds it and serves it at `GET /api/v1/openapi.json`. If `OF_PUBLIC_URL` is set, the `servers` field is `OF_PUBLIC_URL/api/v1`.
- `make openapi-check` compares it with `api/openapi.v1.baseline.json` to find breaking changes.

### Make a client in another language

There is no SDK for other languages. Generate a client, for example with [OpenAPI Generator](https://openapi-generator.tech):

1. Download the document:

   ```bash
   curl -s "$OF_PUBLIC_URL/api/v1/openapi.json" -o openapi.json
   ```

2. Generate the client (Python here; use `-g typescript-fetch` for TypeScript, `openapi-generator-cli list` for all):

   ```bash
   docker run --rm -v "$PWD:/local" openapitools/openapi-generator-cli generate \
     -i /local/openapi.json -g python -o /local/openfactory-client
   ```

3. Send `Authorization: Bearer $OF_API_KEY` with each request.

A generated client does not retry or resume streams. If you need this, retry `429` and `503` with `Retry-After` (never a failed `POST`), and resume the run stream with `Last-Event-ID` (see [Stream events](#stream-events)). Generate the client again when you upgrade the server: before `v1.0.0`, the API can change.
