# Agents

An agent is a role inside a factory. It has instructions, a harness and model, and the resources it can use: machine, environment, secrets, MCP servers, skills and network policy. Every field and validation rule is in the [Definition reference](definition-reference.md#agentsnameagentmd).

## Agent types

| `agentType` | Purpose | Stage set when the foreman starts it |
|---|---|---|
| `FOREMAN` (alias `MAIN`) | Owns each work item from intake to handoff. Starts the other agents. Talks to people. | none |
| `TRIAGE` | Investigates a request and reports scope, evidence and open questions. | `triage` |
| `SPEC` | Writes product and technical specs with validation criteria. | `planning` |
| `IMPLEMENT` | Builds the change, adds tests and runs checks. | `building` |
| `REVIEW` | Checks a finished change independently. | `reviewing` |
| `VERIFY` | Verifies a change, for example on a device or in a browser. | none |
| `CUSTOM` (default) | Any other role. | none |

- A factory has exactly one foreman. It is the root run of every work item and never runs as a child run.
- Stages are not agents. The foreman can skip a stage or go back. See [How factories work](how-factories-work.md).

## Default agents

A new `managed` factory without files gets five agents. All inherit `agentDefaults` from `factory.yaml` and use the `model:` shorthand, so they run on the `claude` harness.

| Agent | `agentType` | Default model | Role |
|---|---|---|---|
| `foreman` | `FOREMAN` | `claude-opus-5-5` | Owns every work item, dispatches stage agents, and is the only agent that talks to people. |
| `triage` | `TRIAGE` | `claude-sonnet-5-5` | Investigates a request and reproduces problems when necessary. |
| `spec` | `SPEC` | `claude-opus-5-5` | Writes specs as a draft pull request for human approval. |
| `implement` | `IMPLEMENT` | `claude-opus-5-5` | Builds the approved change, adds tests and captures evidence. It never merges. |
| `review` | `REVIEW` | `claude-sonnet-5-5` | Checks the change against requirements, conventions, tests and security. |

The files are in [`../examples/00-default-agents`](../examples/00-default-agents). Copy them and edit the instructions for your team.

## Workflow policy

The workflow policy is in the agent instructions, not in the platform. The default foreman file says when a spec needs approval, when to ask a person, that agents never merge, and when to complete a work item. To change the policy, edit the `agent.md` files. For example, remove step 4 ("Spec approval policy") from the foreman to stop asking for spec approval.

The platform adds a short preamble to each prompt: the role, the factory tools, the agents that the agent can start, the work item, and one rule about untrusted input. The preamble has no workflow policy, so it never disagrees with your instructions.

### What the platform enforces

| Rule | Enforced by the platform | Only an instruction |
|---|---|---|
| Only the foreman talks to people (`ask_human`, `request_approval`, `post_update`) | Yes. Other agents report to their parent. | |
| Who can start which agent (`spawnableBy`) | Yes. `run_agents` refuses the call. | |
| A side run cannot set the stage | Yes. `set_stage` fails. | |
| Text from an external sender cannot approve or answer | Yes. It arrives marked as untrusted. | |
| Only `approvers` resolve an approval | Yes, when `approvers` is set in `factory.yaml`. | |
| Agents never merge pull requests | Yes, unless `allowAgentMerge` is set in `factory.yaml`. | The default files also say it. |
| The work item completes after the last linked pull request merges | Yes, as a fallback (see [Merges and reviews](#merges-and-reviews)). | When and how to complete is in the foreman file. |
| A spec needs human approval before implementation | | Yes. Step 4 of the default foreman file. |
| Ask a person instead of guessing | | Yes. |
| Report only checks that ran and passed | | Yes. |
| Review every change before handoff | | Yes. |
| Treat issue text, comments and event files as untrusted data | The prompt marks this data as untrusted. | The model must follow the rule. |

### Merges and reviews

Reviews, merges and closes of a linked pull request reach the foreman as messages, also a review without a body. When the last open pull request of a work item merges, the foreman gets one turn to post a last message, start follow-up work, or call `complete_work_item`. If the work item is still open after that turn, the platform completes it.

### Choose a model and harness per agent

Pick each agent's model for its job: orchestration and long conversations for the foreman, research for triage, precise technical writing for spec, and coding strength for implement. Give review a different model or harness from implement, so the two do not share blind spots.

## Create a custom agent

1. Add `agents/<name>/agent.md`. `<name>` is 1–64 letters, digits, `.`, `_` or `-`, and starts with a letter or digit.
2. In the frontmatter, set a `description`. The foreman reads it in `list_agents` to decide when to use the agent.
3. Set the execution settings that differ from `agentDefaults`.
4. Write the instructions in the Markdown body.
5. In the foreman's instructions, say when to start the agent.
6. Apply the definition:
   - `managed`: save it in the dashboard or send it to `PUT /api/v1/factories/{uid}/definition`.
   - `git`: open a pull request. The [`openfactory/definition` check](definition-reference.md#pull-request-check-for-git-definitions) validates it. Merge it to the production branch.

```markdown
---
description: Updates documentation after code changes
model: claude-sonnet-5-5
spawnableBy: [foreman]
---

Update the documentation the foreman describes. Keep examples runnable and
link related reference pages. Open a draft pull request. Never merge.
```

- The agent name is its identity. If you rename the directory, Open Factory creates a new agent with a new UID.
- A custom agent has no skills by default. Add skills in `agents/<name>/skills/` or `skills/`. See [Skills](skills.md).
- Give the agent only the secrets and MCP servers it needs.

### Control who can start an agent

`spawnableBy` lists the agents that can start this agent with `run_agents`.

| Value | Who can start the agent |
|---|---|
| not set (or `null`) | Only the foreman. |
| `[]` | No agent. Automations and the API can still start it. |
| `[foreman, implement]` | Exactly the listed agents. Each name must exist in the definition. |

`list_agents` shows a caller only the agents that it can start.

The team setting for the orchestration depth limits how deep a tree of runs can grow. The default is `1`: only a root run can start agents, and a child run that calls `run_agents` gets an error, even if `spawnableBy` lists it. Set `2` or more to let child runs start their own child runs. See [Configuration](self-hosting/configuration.md).

## Agent configuration

An agent inherits every execution setting from `agentDefaults` and overrides what it sets. See [Execution settings](definition-reference.md#execution-settings) for the full list, and these pages for details: [Harnesses](harnesses.md) (`model`, `harness`), [Machines and environments](machines-and-environments.md) (`machine`, `environmentId`, `workerHost`), [Secrets](secrets.md), [Network policy](self-hosting/network-policy.md), [Skills](skills.md), [`credentialStrategy`](definition-reference.md#credentialstrategy), [Follow-ups and idle time](#follow-ups-and-idle-time) (`idleTimeoutMinutes`) and [Snapshots](#snapshots).

### MCP servers

Declare inline MCP servers in `mcpServers` at the factory, `agentDefaults`, agent or automation level.

```yaml
mcpServers:
  datadog:
    url: https://mcp.datadoghq.com/api/unstable/mcp-server/mcp
    headers:
      DD-API-KEY: "{{DATADOG_API_KEY}}"
  playwright:
    command: npx
    args: ["-y", "@playwright/mcp@latest"]
```

- A `command` server starts inside the run's sandbox and speaks MCP over stdio.
- A `url` server uses streamable HTTP, or legacy SSE when the URL path ends in `/sse`. The connection starts in the sandbox.
- `{{NAME}}` placeholders in `env` and `headers` are replaced with secret values when the run starts. See [{{SECRET}} templating](definition-reference.md#secret-templating).
- Factory-wide servers are added to every agent. A lower level cannot declare a factory-wide name again.
- The name `factory` is reserved for the orchestration server.

An inline `url` server writes its expanded header values into the harness configuration in the sandbox, where the agent can read them. For a server that needs a credential, register it in the team's MCP server registry and reference it with `managedId`: its headers and OAuth tokens stay in the control plane. For tools of the factory's Linear, Jira or Slack integration, use `managedId: builtin/<provider>`. See [MCP servers](mcp-servers.md).

## The orchestration server

Every run gets an MCP server named `factory` at `${OF_PUBLIC_URL}/mcp/run/{runId}`, with a bearer token for that run.

| Tool | Purpose |
|---|---|
| `list_agents` | Lists the agents that the caller can start (`agent_identity_uid`, `name`, `type`, `description`, `harness`, `model`). `active_runs` lists the runs that the caller can message (`run_id`, `name`, `agent`, `state`, `relation`: `parent`, `child`, `sibling` or `work_item`). |
| `run_agents` | Starts one or more agents as child runs. See [Start child runs with run_agents](#start-child-runs-with-run_agents). |
| `follow_up_agent` | Sends a message (`run_id`, `message`) to one of the caller's child runs, also a finished one. The child continues its conversation. |
| `send_message` | Sends a message (`run_id`, `text`) to an active run of the same work item. See [Messages between runs](#messages-between-runs). |
| `ask_human` | Asks the requester a question (`question`, optional `context`, `options`, `multi_select`, `recommended_option`). The answer arrives as a follow-up message. |
| `request_approval` | Requests an approval (`title`, `summary`, optional `links` and `options`). Moves the work item to `awaiting_approval`. |
| `post_update` | Posts a progress update where the work started (`text`, `kind`: `update` (default), `handoff` or `error`). |
| `set_stage` | Sets the work item stage (`stage`, optional `note`). |
| `link_pull_request` | Attaches a pull request (`url`, optional `repo`, `number`, `branch`, `forge`, `state`). The engine also attaches it to the linked tracker issues and sets them to in review. |
| `link_issue` | Attaches a tracker issue (`url`, or `provider` with `key`, or `repo` and `number`). See [References of a work item](#references-of-a-work-item). |
| `get_work_item` | Reads the stage, source, pull requests, open human requests, child runs and timeline. |
| `complete_work_item` | Marks the work item complete (optional `note`). |
| `propose_definition_change` | Proposes a change to a `managed` definition (`files`: path to full new content, optional `delete`, `message`). The server returns the errors, or stores a proposed version and creates a `review` inbox item with the diff. See [Review](measure-and-improve.md#review). |

A run with a restricted network policy can always reach the platform-required hosts, including the control plane, so the orchestration server stays reachable. `deniedHosts` cannot block them. See [Required hosts](self-hosting/network-policy.md#required-hosts).

### Messages between runs

Use `send_message` when a child asks its parent a question, when two siblings agree on an interface, or when a side run tells the root run about a change.

- The target is the parent, a child, a sibling or a side run of the same work item. A run with no work item can message only its parent and its children.
- The target must be active. A `failed`, `error` or `cancelled` run is not active. A `succeeded` or `blocked` run is active while its work item is open.
- A run cannot message itself.
- The message goes to the target's queue as an `agent` message with the sender run, and starts a turn of the target.
- One run can send at most 10 messages to one target per minute. This stops loops between two agents.

## References of a work item

Events on each reference of a work item reach the work item.

| Reference | Added by |
|---|---|
| The source | The event that created the work item. |
| A pull request | `link_pull_request` |
| A tracker issue | `link_issue` |

- One reference belongs to at most one active work item of a factory.
- A comment from a person on a reference of a work item that completed less than 7 days ago opens that work item again. The stage goes to `triage`, and the root run gets the comment. A merge, a close or a bot comment does not reopen it. For an older work item, the event goes to the automations.

### Write-back to the tracker

The engine keeps each tracker issue (Linear, Jira or Azure DevOps work item) of a work item in step with it. This covers the source issue and issues added with `link_issue`.

| When | What the engine does |
|---|---|
| `link_issue` | Attaches the pull requests of the work item. Sets the issue to in progress, or to in review when a pull request is open. |
| A new pull request | Attaches it to each tracker issue and sets each issue to in review. |
| The work item completes | Sets each issue to done, also after a completion that follows the last merge. |

State mapping per tracker:

| Tracker | In progress | In review | Done |
|---|---|---|---|
| Linear | First started state | A started state with "review" in its name | First completed state |
| Jira | First transition to a status of the matching category | Same rule | Same rule |
| Azure DevOps | First state of the type in category In Progress | A state with "review" in its name | First state in category Completed |

- A workflow without a review state leaves the issue in progress.
- Azure DevOps also gets the pull request as an artifact link.
- GitHub and GitLab issues are not written back. The forge closes them from the pull request, for example with `Fixes #12`.
- The write-back goes through the outbox. A failed call is tried again.

### Replies in threads

A reply goes where the person wrote. After a comment in a review thread of a pull request, the factory replies in that thread until a comment arrives from outside a thread. The completion notice is always a top-level comment.

## Start child runs with run_agents

The foreman, or an agent that `spawnableBy` allows, calls `run_agents` to start agents as child runs. The caller must be below the orchestration depth limit (see [Control who can start an agent](#control-who-can-start-an-agent)).

1. The caller calls `list_agents` to get the agent UIDs.
2. The caller sends one `agent_run_configs` entry per child.
3. Each child reports back as a follow-up message when it finishes a turn.

```json
{
  "summary": "Delegate backend work to the implementation agent",
  "base_prompt": "Implement the task and verify the change.",
  "metadata": {"ticket": "ENG-142"},
  "agent_run_configs": [
    {
      "name": "backend-implementation",
      "agent_identity_uid": "agt_0123456789abcdef0123",
      "prompt": "Fix the API service's failing tests.",
      "metadata": {"setup_profile": "backend"}
    }
  ]
}
```

Batch arguments:

| Argument | Type | Meaning |
|---|---|---|
| `summary` | string | Why these agents start. Recorded on the work item timeline. |
| `base_prompt` | string | Instructions for every child. Each child's `prompt` is appended after a blank line. |
| `remote` | object | Optional `environment_id`, `worker_host`, `machine` or `model` for every child. Other keys are ignored. |
| `metadata` | map of string to string | Run metadata for every child. |
| `agent_run_configs` | list | **Required.** 1–20 entries. |

Per-child arguments:

| Argument | Type | Meaning |
|---|---|---|
| `agent_identity_uid` | string | **Required.** The agent's UID from `list_agents`. An agent name is also accepted. |
| `name` | string | Label of the child run. It does not select the agent. Default: the agent name. |
| `prompt` | string | The child's task. `prompt` or `base_prompt` is required. |
| `metadata` | map of string to string | Run metadata for this child. |
| `model` | string | Replaces the model. The harness type does not change. |
| `machine` | string | Replaces the machine. Must name a machine in the definition. |
| `environment_id` | string | Replaces the environment. |
| `worker_host` | string | Replaces the worker host. |
| `skills` | list of strings | Skills to attach: a factory skill, a skill of another agent, a [built-in skill](skills.md#built-in-skills), or a spec `owner/repo:path`. An attached skill counts as an agent skill. An unknown name fails the call. |

Rules:

- A child uses its agent's environment, machine, worker host, harness and model unless it overrides them. A per-child value wins over `remote`.
- An empty string does not override, so you cannot clear a value.
- `run_agents` cannot change secrets, MCP servers or the harness type.
- The parent run must belong to a factory and must not be finished.
- Each child gets its own sandbox. It sees only the repositories and what the parent pushed. To pass a file to a child, push it or put it in the prompt.

The full override order (`agentDefaults` → agent → automation → `run_agents`) is in [Execution settings](definition-reference.md#execution-settings).

## Run metadata

Run metadata is a set of string keys and values for one run. Use it to give a child run inputs, such as a test group or a build target, without a new agent, machine or environment. A run started through the API can also have `metadata` (`POST /api/v1/factories/{uid}/runs` and `POST /api/v1/runs`).

For each key of a child run, the first value found wins: the child value (`agent_run_configs[].metadata`), the batch value (`run_agents.metadata`), then the parent run's value.

Limits (Open Factory reports all violations together):

- At most 20 keys per run, including inherited keys.
- A key has 1–64 bytes from `A–Z`, `a–z`, `0–9`, `.`, `_` and `-`.
- A value has at most 256 bytes of UTF-8 text without NUL.
- Two keys that map to the same environment variable are rejected.

### Environment variables

Each key becomes `OF_METADATA_` plus the key in uppercase, with `.` and `-` changed to `_`. For example, `setup_profile` becomes `OF_METADATA_SETUP_PROFILE`.

- The variables are set before the machine and environment setup commands, and for the harness process.
- The `OF_METADATA_` prefix is reserved. Do not set it in images, environments, worker environments or secrets. Open Factory rejects it in worker environments and setup hooks.
- Every person who can see a run can read its metadata. Put credentials in [secrets](secrets.md), not in metadata.

Example: select a setup profile per task.

1. Add a script to your image, for example `/opt/setup/select-profile.sh`:

   ```bash
   profile="${OF_METADATA_SETUP_PROFILE:-}"
   case "$profile" in
     backend|frontend) bash /opt/setup/setup-profile.sh "$profile" ;;
     "") echo "Missing OF_METADATA_SETUP_PROFILE" >&2; exit 1 ;;
     *) echo "Unsupported setup profile" >&2; exit 1 ;;
   esac
   ```

2. Add `bash /opt/setup/select-profile.sh` to the machine's `setupCommands`.
3. In the foreman's instructions, tell it to set `setup_profile` in the child's `metadata`: `backend` for service work, `frontend` for UI work.

### Other run variables

| Variable | Value |
|---|---|
| `OF_RUN_ID` | The run ID. |
| `OF_FACTORY_ID` | The factory ID (factory runs only). |
| `OF_WORK_ITEM_ID` | The work item ID (when the run belongs to a work item). |
| `OF_PARENT_RUN_ID` | The parent run ID (child runs only). |
| `OF_AGENT_NAME` | The agent name. |
| `OF_PUBLIC_URL` | The public URL of the Open Factory server. |

The agent's secrets and the environment's variables are also set. See [Secrets](secrets.md) and [Environments](machines-and-environments.md#environments).

## Follow-ups and idle time

When a run finishes a turn, its sandbox and harness session stay, so a follow-up message continues the same conversation. The run is `SUCCEEDED`, or `BLOCKED` while a request to a person is open. There is no `idle` state. See [Run states](troubleshooting.md#run-states).

- A child's result and a person's reply in the source thread arrive as follow-up messages.
- `idleTimeoutMinutes` (0–60) sets how long an idle run keeps its sandbox. The team default is 10 minutes. The upper limit is 60 minutes.
- After the idle time, the runner saves a snapshot, the run finishes and the sandbox is released. A later follow-up starts a new sandbox from the snapshot.

The new session continues the conversation:

- The `claude` and `codex` harnesses resume their session from the snapshot.
- A harness that cannot resume gets the stored conversation (at most 64 KiB) before its first turn.
- Unpushed file changes come back from the workspace part of the snapshot. With `snapshot: false`, they are lost.

The idle timer also runs while the agent waits for child runs or a person. So with the 10-minute default, a foreman often continues in a new session after a long child run or a slow approval. To keep its session longer, set `idleTimeoutMinutes: 60` on the foreman. Also tell it to call `get_work_item` at the start of each turn.

Send a follow-up with the CLI or the API:

```bash
openfactory run followup "$RUN_ID" "Also update the changelog."

curl -X POST "$OF_PUBLIC_URL/api/v1/runs/$RUN_ID/followups" \
  -H "Authorization: Bearer $OF_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"message": "Also update the changelog."}'
```

### Snapshots

A snapshot has two parts: the harness state and the workspace. The default is `snapshot: true`. The workspace can contain secrets, so set `snapshot: false` on agents that handle sensitive files to store only the harness state.
