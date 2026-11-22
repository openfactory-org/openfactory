# Machines and environments

Three settings decide where and how an agent's sandbox runs: the **worker host** (which provider or worker), the **machine** (image, compute shape and setup) and the **environment** (a reusable workspace of repositories, image, setup and variables).

## How the settings combine

| Setting | Declared in | Selected by | Controls |
|---|---|---|---|
| Worker host | The control plane and its workers | `workerHost` | Which sandbox provider or worker runs the sandbox. |
| Machine | `machines/<name>.yaml` in the definition | `machine` | Platform, container image, CPU and memory, setup commands, registry credential, failed-sandbox retention. |
| Environment | The Open Factory API (team resource) | `environmentId` | Repositories to check out, container image, setup commands, environment variables. |

All three are [execution settings](definition-reference.md#execution-settings): set a default in `agentDefaults`, and override it per agent, per automation, or per child run in [`run_agents`](agents.md#start-child-runs-with-run_agents) (`machine`, `environment_id`, `worker_host`).

When a sandbox starts:

1. The worker host selects the provider or worker.
2. The image is the environment's `image`, else the machine's `platform.linux.image`, else the default sandbox image of the provider.
3. The repositories are the environment's `repos`, else the factory's `repositories`.
4. The machine's `setupCommands` run, then the environment's `setup_commands`.
5. The environment variables are the environment's `env_vars`, then the agent's secrets, then the run metadata and `OF_*` variables. A later source wins for the same name.

## Worker hosts

| Value | Where the run executes |
|---|---|
| empty or `server` (default) | The default sandbox provider of the team (`OF_SANDBOX_PROVIDER`). |
| a provider ID: `cloudflare-sandbox`, `daytona`, `boatdev` | That provider. |
| a worker ID or name | That enrolled worker. |
| `pool:<label>` | The online, non-draining worker that has the label, allows the factory and has the most free slots. |

- The preflight refuses a worker that the team has not registered. A run for a registered worker that is not connected waits in the queue with the reason `waiting_for_worker`.
- `workerHost: null` at the agent or automation level clears an inherited value, so the run uses `server`.
- The `workerEnvironment` harness auth works only on development process workers. See [Use the worker environment](harnesses.md#use-the-worker-environment).

To run sessions on your own Linux hosts, enroll them as workers. See [Self-hosted workers](self-hosted-machines.md). `GET /api/v1/workers` lists the workers. For the providers, see [Sandbox providers](sandbox-providers.md).

## Machines

A machine is a file `machines/<name>.yaml`; `machine: <name>` selects it.

```yaml
# yaml-language-server: $schema=https://openfactory-org.github.io/schemas/v1/machine.schema.json
description: Larger Linux machine for API builds
setupCommands:
  - make bootstrap
instanceShape:
  vcpus: 4
  memoryGb: 8
failureSessionRetentionMinutes: 30
platform:
  os: linux
  arch: x86_64
  linux:
    image: ghcr.io/acme/platform-dev:2026.10
    registryCredentialSecretName: GHCR_PULL_CREDENTIAL
```

The field list and validation rules are in the [definition reference](definition-reference.md#machinesnameyaml). `platform` is required, `os` is `linux` (the only value), and a Linux machine requires `platform.linux.image`.

### What each machine field does

| Field | Effect |
|---|---|
| `platform.linux.image` | The image of the sandbox, unless the environment sets an image. Providers that build images ahead of time (`cloudflare-sandbox`, `daytona`, `boatdev`) use the image that you built into them. See [Private registries](self-hosting/private-registries.md). |
| `platform.linux.registryCredentialSecretName` | The secret with a pull credential for a private image. Only microVM workers pull images at run time and use it. |
| `platform.linux.user` | The user of the sandbox: a number (the default is `1000`), or `image` to keep the user of the image. Root is not allowed. |
| `platform.arch` | `x86_64` or `aarch64`. The hosted providers run `x86_64`. A run fails with `invalid_configuration` if the target has no sandbox of that architecture. |
| `instanceShape` | CPU and memory of the sandbox. The control plane maps it to the smallest instance type of the provider that is at least as large. A shape above the largest type fails with `invalid_configuration`. See [Machine sizes](sandbox-providers.md#machine-sizes). |
| `setupCommands` | Shell commands that run after checkout, each in a new shell (`sh -c`) in the workspace working directory. A `cd`, `export` or shell variable does not carry over to the next command, so join dependent steps (`cd app && npm ci`). Files and installed tools stay. The whole workspace setup (clones and all commands) can run for at most 30 minutes. A failed command fails the sandbox setup. |
| `failureSessionRetentionMinutes` | How long the sandbox of a failed run is kept for inspection (1–60 minutes), where the provider supports it. Unset, the sandbox is released when the run fails. |

If the environment sets no git identity, the runner gives every sandbox process, including the harness, the identity `openfactory <openfactory@localhost>` so that an agent can commit. To use another identity, grant `GIT_AUTHOR_NAME`, `GIT_AUTHOR_EMAIL`, `GIT_COMMITTER_NAME` and `GIT_COMMITTER_EMAIL` as [secrets](secrets.md#grant-a-secret-to-agents).

Platform support:

- Sandboxes run Linux. Open Factory does not support macOS or Windows agents.
- The development process worker runs commands on its Linux host with no image, resource limits or isolation. It is for development and tests.

### Use a machine

```yaml
# factory.yaml: the default for every agent
agentDefaults:
  model: claude-opus-5-5
  machine: linux-build
```

```markdown
---
# agents/build/agent.md: one agent runs on a larger machine on a worker pool
machine: linux-large
workerHost: pool:big
---
```

`GET /api/v1/machines?factory_id=$FACTORY_ID` lists a factory's machines.

## Environments

An environment is a reusable team workspace. Use one when several agents or factories share repositories, image, setup and variables, or when an agent needs repositories outside the factory's `repositories`.

| Field | Type | Meaning |
|---|---|---|
| `name` | string | **Required.** Non-empty. Unique in the team, without regard to case. |
| `description` | string | Optional. |
| `repos` | list of strings | Repositories to check out. Each entry is `[forge:]owner/name` (for example `GITLAB:acme/platform/billing`) or an `https://` repository URL (for example `https://github.com/acme/web`). Without a forge, the repository is on GitHub. A URL on a GitLab or Azure DevOps host selects that forge. |
| `image` | string | Sandbox image, a valid image reference such as `node:22-bookworm` or `registry.example.com/team/tools:1.2@sha256:...`. It wins over the machine's image. |
| `setup_commands` | list of strings | Run after the machine's setup commands. |
| `env_vars` | map of string to string | Environment variables for the sandbox. Names have letters, digits and `_`, and do not start with a digit. Names that start with `OF_` are reserved. |

### Create an environment

1. Create the environment:

   ```bash
   curl -X POST "$OF_PUBLIC_URL/api/v1/environments" \
     -H "Authorization: Bearer $OF_API_KEY" \
     -H "Content-Type: application/json" \
     -d '{
       "name": "web-monorepo",
       "repos": ["acme/web-app", "acme/design-system"],
       "image": "node:22-bookworm",
       "setup_commands": ["corepack enable", "pnpm install --frozen-lockfile"],
       "env_vars": {"NODE_ENV": "test"}
     }'
   ```

2. Copy the `id` from the response (for example `env_...`).
3. Set `environmentId` in `agentDefaults`, an agent or an automation:

   ```yaml
   agentDefaults:
     model: claude-opus-5-5
     environmentId: env_0123456789abcdef
   ```

4. Apply the definition.

### Manage environments

| Request | Purpose |
|---|---|
| `GET /api/v1/environments` | List the team's environments. |
| `GET /api/v1/environments/{id}` | Read one environment. |
| `PATCH /api/v1/environments/{id}` | Change only the fields in the body. |
| `DELETE /api/v1/environments/{id}` | Delete the environment. Refused with `409` while an agent, automation or `scorerDefaults` of an active definition in the team uses it. |

Create, change and delete require the `editor` role.

### Rules

- The preflight refuses an unknown `environmentId`: `environment "<id>" does not exist in the team`.
- With an environment, the run checks out the environment's `repos` instead of the factory's `repositories`. The agent's working directory is the workspace root.
- Without an environment, each factory repository is checked out to `<workspace>/<name>`. If the factory has exactly one repository, the agent's working directory is that repository.
- Do not put credentials in `env_vars`; use [secrets](secrets.md). A secret with the same name wins.
- The API refuses `env_vars` names that start with `OF_`. The platform sets these variables in a run, for example `OF_RUN_ID` and the [run metadata](agents.md#run-metadata) variables `OF_METADATA_*`.
- The API refuses a second environment with the same name: `an environment named "<name>" already exists`.
- The API refuses to delete an environment that an active definition uses, for example `Environment web-monorepo is used by agent foreman of factory payments. Remove the reference first.` Remove the `environmentId` from the definition, apply it, then delete the environment.
- `environmentId: null` at the agent or automation level clears an inherited environment.
