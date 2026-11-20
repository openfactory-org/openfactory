# Harnesses

A harness is the CLI that runs an agent's turns inside the run's sandbox. Open Factory has two harnesses: `claude` (Claude Code) and `codex` (Codex). Each agent can use a different harness.

## Compare the harnesses

| | `claude` (alias `claude-code`) | `codex` |
|---|---|---|
| Models | Claude models. | OpenAI models. |
| `reasoningLevel` | `low`, `medium`, `high`, `xhigh`, `max` | `minimal`, `low`, `medium`, `high`, `xhigh` |
| Session resume | `--resume <session>` | `codex exec resume <thread_id>` |
| Cost | The CLI reports the cost of each turn. | Estimated from the token counts that the CLI reports. |

The control plane makes no model calls. The model credential is in the environment of the harness CLI, inside the sandbox. The spend limit uses the usage that the CLI reports.

Both harnesses get the same inputs (see [How configuration reaches a run](#how-configuration-reaches-a-run)), including the `factory` orchestration server. So a foreman on either harness can start other agents with `run_agents`.

## The CLI in the sandbox

The harness CLI (`claude` or `codex`) must be on the `PATH` of the sandbox.

On the `kubernetes` backend, a tool image delivers the CLI, so the sandbox image does not need it. An init container copies the tool image content into the Pod, and the backend puts the tool's `bin` directory in front of the sandbox `PATH`. So a run with the image `ubuntu:22.04` and the `claude` harness works.

- Set the tool images with `OF_CLAUDE_TOOL_IMAGE` and `OF_CODEX_TOOL_IMAGE` on the control plane, also when remote workers start the sandboxes.
- Default: the runner image name with `tool-claude` or `tool-codex` and the same tag. The default runner image is the server image, so the defaults are `ghcr.io/openfactory-org/tool-claude` and `ghcr.io/openfactory-org/tool-codex` of the same release. They are built from `images/tool-claude` and `images/tool-codex`.
- Without a tool image, the sandbox image must hold the CLI. The default sandbox image has no harness CLI.
- In development mode (`OF_DEV=true` with the `local` backend), the workstation must have the `claude` or `codex` program.

See [Tool images](self-hosting/kubernetes.md#tool-images) for the rules of a tool image.

## Select a harness

Select the harness with `model` or `harness`. The inheritance rules are in [Harness and model](definition-reference.md#harness-and-model).

```yaml
# A harness object sets the harness, the model and the credential.
agentDefaults:
  harness:
    type: claude
    model: claude-opus-5-5
    reasoningLevel: high
    auth:
      source: managedSecret
      secretName: ANTHROPIC_API_KEY
```

```yaml
# The model shorthand selects the harness from the model name.
model: claude-sonnet-5-5
```

- `model: X` selects the harness from the model name. A Claude name (`claude-...`, `opus`, `sonnet`, `haiku`) selects `claude`. An OpenAI name (`gpt-...`, `chatgpt-...`, `codex-...`, or an o-series name such as `o3` or `o4-mini`) selects `codex`. Any other name is a validation error; set `harness` with a `type` and a `model` instead.
- If the shorthand keeps the inherited harness, the inherited `reasoningLevel` and `auth` stay. If it selects the other harness, they do not carry over.
- At the agent and automation level, `harness` is a sparse override. If it changes `type`, `model` is required, and the inherited `reasoningLevel` and `auth` are dropped.
- A new factory uses the `claude` harness with `claude-opus-5-5`.

[`../examples/03-multi-harness`](../examples/03-multi-harness) uses both harnesses in one factory.

## The Claude Code harness

The `claude` harness runs one CLI process per turn:

```text
claude -p --output-format stream-json --verbose --input-format text
       [--resume <session>] [--model <m>] [--effort <level>]
       [--append-system-prompt <s> | --append-system-prompt-file <f>]
       [--mcp-config .openfactory/claude/mcp.json --strict-mcp-config]
       --permission-mode bypassPermissions
```

- The prompt goes to standard input.
- Sandboxes are disposable, so permission prompts are bypassed. The harness sets `IS_SANDBOX=1`, `DISABLE_AUTOUPDATER=1` and `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`.
- The composed system prompt is appended to the CLI's own system prompt. A prompt larger than 64 KiB goes to `.openfactory/claude/system-prompt.md`.
- `--strict-mcp-config` makes the CLI use only the MCP servers of the run. MCP configuration files in the repository are not loaded.

Models and reasoning level:

- `""` and `default` select the CLI's default model. Other names (for example `claude-opus-5-5`, `opus`, `sonnet`, `haiku`) pass through to `--model`.
- `reasoningLevel` becomes `--effort <level>`. The definition checks only the format of the value.

## The Codex harness

The `codex` harness runs one CLI process per turn:

```text
codex exec [resume] --json --skip-git-repo-check --dangerously-bypass-approvals-and-sandbox
           [-m <model>] [-c model_reasoning_effort="<level>"] [-C <workdir>] [<thread_id>] -
```

- The prompt goes to standard input (the `-` argument).
- `CODEX_HOME` is `<workdir>/.openfactory/codex`. It holds `config.toml` (the MCP servers as `[mcp_servers.<name>]` tables), `AGENTS.md` (the composed system prompt) and `skills/`. The repository's own `AGENTS.md` does not change.
- Session files live in `CODEX_HOME`, so `codex exec resume` continues the conversation in the same sandbox.

Models and reasoning level:

- `""` and `default` select the CLI's default model. An `openai/` prefix is removed.
- The catalog lists `gpt-5.3-codex` (default), `gpt-5.2-codex`, `gpt-5.1-codex-max`, `gpt-5.1-codex-mini`, `gpt-5.2`, `gpt-5.1`, `gpt-5-codex` and `gpt-5`. Other model IDs pass through.
- `reasoningLevel` becomes `-c model_reasoning_effort="<level>"`. `max` maps to `xhigh` and `none` maps to `minimal`. Other values outside the accepted list are ignored.

## Authentication

A run needs a model credential. Set it with `harness.auth`, or save one secret of a credential type and let the run use it as the [default credential](#default-credential).

| `auth.source` | Where the credential comes from | Rules |
|---|---|---|
| `managedSecret` | A secret in the Open Factory secret store, named by `secretName`. | `secretName` is required. |
| `workerEnvironment` | The environment of a self-hosted worker. | `secretName` is not allowed. The effective `workerHost` must be a self-hosted worker. |

### Default credential

When the effective harness of a run has no `auth` (after `agentDefaults`, the agent, the automation and the run overrides are merged), Open Factory selects the credential in this order:

1. The environment of the run already holds a model credential: Open Factory passes nothing, and the CLI uses that credential. This covers the variables of the [environment](machines-and-environments.md), the agent's `secrets` (for example a secret named `ANTHROPIC_API_KEY`), and the environment of the sandbox process (for example `ANTHROPIC_API_KEY` set for the server with the `local` backend). The variables are `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, `CLAUDE_CODE_OAUTH_TOKEN`, `AWS_BEARER_TOKEN_BEDROCK`, `CLAUDE_CODE_USE_BEDROCK` and `CLAUDE_CODE_USE_VERTEX` for `claude`, and `OPENAI_API_KEY` and `CODEX_API_KEY` for `codex`.
2. Exactly one factory secret has a type that the harness accepts: the run uses it.
3. No factory secret fits, and exactly one team secret fits: the run uses it. A team secret does not count when a factory secret has the same name.
4. Otherwise the run gets no credential. The CLI uses the credentials that the sandbox has, for example a signed-in CLI.

| Harness | Secret types of the default credential |
|---|---|
| `claude` | `anthropic_api_key`, `anthropic_oauth_token`, `anthropic_bedrock_api_key`, `anthropic_bedrock_access_key` |
| `codex` | `openai_api_key` |

A `generic` secret is never a default credential. Other harness types have no default credential.

If more than one secret fits at the first scope that has one, Open Factory uses none of them, because the choice decides which account pays for the model. Set `harness.auth` to the secret you want, or delete the others. The definition check names the candidates in a warning, and the server logs a warning when the run starts.

The run shows which secret it uses:

- The `secrets.delivered` event of the run timeline (`GET /api/v1/runs/{runId}/timeline`) has `harness_credential`, for example `{"source": "default", "secret": "CLAUDE_OAUTH_TOKEN", "scope": "team", "type": "anthropic_oauth_token"}`. `source` is `managedSecret`, `workerEnvironment`, `default` or `none`. With `none`, `reason` is `run_environment`, `ambiguous` (with `candidates`) or `no_secret`.
- The server logs `model credential: default secret` at info level with the secret name.
- The audit event `runner.secrets.read` has `harness_credential`, for example `default:team:CLAUDE_OAUTH_TOKEN`.

None of them hold the secret value. If the sandbox process already has a credential variable, the runner drops the default credential and writes a `diagnostic` event with the code `default_credential_not_used`.

The definition check gives a warning for each agent without `auth` when no secret fits or more than one fits. A run without a credential fails with the CLI error, and the run error says that the agent has no harness credential.

### Use a managed secret

1. Create a typed secret for the provider:

   ```bash
   openfactory secret set ANTHROPIC_API_KEY --type anthropic_api_key
   ```

   The command reads the value from standard input. See [Secrets](secrets.md) for the API and for scopes.
2. Reference the secret in the harness:

   ```yaml
   harness:
     type: claude
     model: claude-opus-5-5
     auth:
       source: managedSecret
       secretName: ANTHROPIC_API_KEY
   ```

3. Apply the definition and start a run.

When the run starts, Open Factory maps the secret to environment variables of the harness process only. A factory-scoped secret wins over a team secret with the same name.

Value formats are in [Secret types](secrets.md#types).

| Secret type | Environment variables | Harness |
|---|---|---|
| `anthropic_api_key` | `ANTHROPIC_API_KEY` | `claude` |
| `anthropic_oauth_token` (from `claude setup-token`) | `CLAUDE_CODE_OAUTH_TOKEN` | `claude` only. Other harnesses ignore it. |
| `anthropic_bedrock_api_key` | `AWS_BEARER_TOKEN_BEDROCK`, `AWS_REGION`, `CLAUDE_CODE_USE_BEDROCK=1` | `claude` |
| `anthropic_bedrock_access_key` | `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN`, `AWS_REGION`, `CLAUDE_CODE_USE_BEDROCK=1` | `claude` |
| `openai_api_key` | `OPENAI_API_KEY`, `CODEX_API_KEY` | `codex` |
| `generic` | `ANTHROPIC_API_KEY` for `claude`; `OPENAI_API_KEY` and `CODEX_API_KEY` for `codex` | both |

Run errors:

| Cause | Error |
|---|---|
| Bedrock secret without `region` | `bedrock credentials require an AWS region` |
| `claude` with an OpenAI key | `claude cannot use an OpenAI API key` |
| `codex` with an Anthropic or Bedrock secret | `codex cannot use Anthropic or Bedrock credentials` |
| The secret does not exist | `harness auth secret "<name>": ...` |

### Use the worker environment

Use `workerEnvironment` when a self-hosted worker already has the credentials, for example in its environment variables or a signed-in CLI.

```yaml
agentDefaults:
  harness:
    type: claude
    model: claude-opus-5-5
    auth:
      source: workerEnvironment
  workerHost: acme-prod-worker
```

- Open Factory passes no credential. The worker must give the provider variables (for example `ANTHROPIC_API_KEY` or `OPENAI_API_KEY`) to its sandboxes.
- The definition rejects `workerEnvironment` when the effective `workerHost` is empty, also when an agent or automation clears `workerHost` and inherits this auth.

Example: [`../examples/07-self-hosted-worker`](../examples/07-self-hosted-worker). Worker setup: [Workers](self-hosting/workers.md).

## How configuration reaches a run

When a run starts a turn, Open Factory prepares the sandbox and calls the harness.

| Input | How it reaches the run |
|---|---|
| Secrets | Each name in the agent's `secrets` becomes an environment variable with the secret's value. |
| Run identity and metadata | `OF_RUN_ID`, `OF_FACTORY_ID`, `OF_WORK_ITEM_ID`, `OF_PARENT_RUN_ID`, `OF_AGENT_NAME`, `OF_PUBLIC_URL` and `OF_METADATA_*`. See [Run metadata](agents.md#run-metadata). |
| Harness auth | `harness.auth` or the [default credential](#default-credential), mapped to provider variables (see [Use a managed secret](#use-a-managed-secret)). Set only for the harness process. |
| MCP servers | `{{SECRET}}` placeholders in `env` and `headers` are expanded, and the `factory` orchestration server is added. Claude Code gets `.openfactory/claude/mcp.json`. Codex gets `CODEX_HOME/config.toml`. |
| Skills | Written to `.openfactory/skills/<name>/`. Claude Code also gets `.claude/skills/<name>/` (an existing repository skill is not overwritten). Codex also gets `CODEX_HOME/skills/`. The system prompt lists every skill. See [Skills](skills.md). |
| System prompt | Composed from the agent type, instructions, work item context, skills and agents. The workflow policy is in the agent instructions (see [Workflow policy](agents.md#workflow-policy)). |
| Event files | Written to `.openfactory/events/<message_id>/`. See [What the agent gets from the event](automations.md#what-the-agent-gets-from-the-event). |

Injected files live in `.openfactory/`, which has its own `.gitignore`, so they do not appear in `git status`.

If the agent's harness is not available on the server, the run fails with `harness "<type>" is not available`.

### What each harness loads from the repository

A repository root is the work directory and each direct subdirectory with a `.git` entry. In each root, Open Factory looks for the rule files `AGENTS.md`, `AGENT.md` and `CLAUDE.md`, in this order, and for skills in `.agents/skills/<name>/SKILL.md` and `.claude/skills/<name>/SKILL.md`.

| Source in the repository | `claude` | `codex` |
|---|---|---|
| `AGENTS.md`, `AGENT.md` in a root | Added to the system prompt. | `AGENTS.md` of the work directory: read by the CLI. Others: added to the system prompt. |
| `CLAUDE.md` in a root | `CLAUDE.md` of the work directory: read by the CLI. Others: added to the system prompt. | Added to the system prompt. |
| `.claude/skills/<name>/` | Of the work directory: read by the CLI. Others: copied to `.claude/skills/`. | Copied to `CODEX_HOME/skills/`. |
| `.agents/skills/<name>/` | Copied to `.openfactory/skills/` and `.claude/skills/`. | Copied to `CODEX_HOME/skills/`. |

Rules:

- A rule file is at most 64 KiB in the prompt. A longer file is cut, and the prompt says so.
- At most 50 repository skills load. If two repositories have a skill with the same name, the first one wins.
- A skill directory with a `.gitignore` that ignores everything was written by a harness. It is skipped.
- The rule files come after the factory and agent instructions. If they conflict, the factory and agent instructions win.
- For skills with the same name, the order is: agent skill, factory skill, repository skill, built-in skill. See [Skills](skills.md#where-skills-come-from).
