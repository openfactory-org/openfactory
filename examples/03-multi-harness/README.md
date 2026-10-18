# 03 - Multi-harness

A factory whose agents run on different harnesses.

## What it shows

| Agent | Harness | How it is configured |
| --- | --- | --- |
| `foreman` | Claude Code (`claude`) | Inherits `agentDefaults.harness`. |
| `implement` | Codex (`codex`) | Its own `harness`, with `OPENAI_API_KEY` as `auth`. |
| `review` | Claude Code (`claude`) | `model: claude-sonnet-5-5`. Keeps the harness and its `auth`. |
| `docs` | Claude Code (`claude`) | `model: claude-haiku-5-5`, a smaller model for documentation. |

Rules:

- `model` and `harness` are mutually exclusive in the same block.
- `model: X` selects the harness from the model name. A Claude model selects
  `claude`, and an OpenAI model selects `codex`. Another name is an error.
- An agent's `harness` is a sparse override of `agentDefaults.harness`. A
  change of `type` requires a `model`.
- When the harness type stays the same, the agent keeps the inherited `auth`
  and `reasoningLevel`. When the type changes, they do not carry over. Set
  `auth: null` to clear inherited authentication.
- Each harness needs a model credential (`auth`). The credential goes into the
  sandbox of the run, where the CLI uses it.
- `auth.source: workerEnvironment` uses credentials from the environment of a
  self-hosted worker and requires `workerHost` (see
  [07-self-hosted-worker](../07-self-hosted-worker/)).
- The network policy extends the built-in Claude Code and Codex policies.
  Both CLIs can then reach their inference and authentication services.
- `machines/linux-large.yaml` pulls a private image with the registry
  credential `GHCR_PULL_CREDENTIAL`.

## Prerequisites

- A running server, with `OF_SERVER` and `OF_API_KEY` set
  (see [Quickstart](../../docs/quickstart.md)).
- The managed secrets:

  ```bash
  openfactory secret set ANTHROPIC_API_KEY --type anthropic_api_key
  openfactory secret set OPENAI_API_KEY --type openai_api_key
  openfactory secret set GHCR_PULL_CREDENTIAL --type docker_registry
  ```

## Apply

1. In `factory.yaml`, under `repositories`, change `owner: acme` and
   `name: platform-api` to your repository.
2. In `machines/linux-large.yaml`, change `image: ghcr.io/acme/platform-dev:2026.10`
   to your image.
3. Validate and apply:

   ```bash
   openfactory validate examples/03-multi-harness
   openfactory factory apply examples/03-multi-harness
   ```

## What to expect

`validate` reports 4 agents. `apply` creates the factory `multi-harness`.
The foreman dispatches `implement` on Codex and `review` and `docs` on
Claude Code.
