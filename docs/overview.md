# Overview and concepts

Open Factory runs teams of AI coding agents on your own infrastructure. A team connects its code forge, chat and issue tracker. Requests arrive from these tools as events. A factory turns each request into a reviewed change, usually a pull request, and reports back where the request started.

This page defines the terms that the other pages use.

## Architecture

Everything is one binary, `openfactory`.

| Component | Location |
|---|---|
| Server (`openfactory serve`) | One process with the API (`/api/v1`), the dashboard, the Factory MCP (`/mcp`), webhook and integration endpoints (`/webhooks/w/...`, `/integrations/...`), the orchestration server of each run (`/mcp/run/{runId}`) and the worker endpoint (`/api/v1/workers/connect`). On Kubernetes it runs with the Helm chart: one replica with SQLite, or several with PostgreSQL. |
| Database | SQLite (default) or PostgreSQL. It holds the run messages. `sqlite://:memory:` is for tests. |
| Object storage | Artifacts and raw harness transcripts. See [Object storage](self-hosting/object-storage.md). |
| Sandboxes | Where agents run: Kubernetes pods started by the server or by a remote worker in another cluster, or a plugin backend. The `local` backend (one host process per sandbox) is for development and tests. |
| Harnesses (`claude`, `codex`) | In the sandbox, started by the runner. The model credential is in the CLI's environment. The control plane makes no model calls. |

## Concepts

| Concept | Meaning |
|---|---|
| Factory | A named team of agents bound to a set of repositories. Its `alias` is the handle that people mention, for example `@payments`. |
| Definition | The versioned file tree that describes a factory, rooted at `factory.yaml`. The server applies a new version atomically and keeps the last valid version when a new one fails. |
| Definition source | `managed` (stored in the database, edited in the dashboard or API) or `git` (a repository and production branch, synchronized on push). |
| Agent | A role in a factory: instructions, a harness, a model and execution settings. Types: `FOREMAN` (alias `MAIN`), `TRIAGE`, `SPEC`, `IMPLEMENT`, `REVIEW`, `VERIFY`, `CUSTOM`. Each factory has exactly one foreman. |
| Work item | One request: an issue, a pull request, a chat thread, a webhook delivery, a schedule tick, or a request through the API or MCP. It keeps its source so that results go back there. |
| Stage | The position of a work item: `intake`, `triage`, `planning`, `awaiting_approval`, `building`, `reviewing`, `handoff`, `complete`, `cancelled`. The foreman can skip stages or go back. |
| Run | One agent execution: one harness session in one sandbox. Runs form a tree: the foreman run is the root, and `run_agents` adds children. A run accepts follow-up messages and can be cancelled. |
| Automation | Triggers (provider, event and filter, or a schedule) plus a target agent, a starting prompt and optional execution overrides. |
| Harness | The runtime that executes an agent: `claude` (Claude Code CLI) or `codex` (Codex CLI). |
| Machine | A compute specification: platform, image, instance shape and setup commands. |
| Environment | A reusable workspace description: repositories, image, setup commands and variables. |
| Worker host | Where sandboxes run: `server` (the server's own backend) or the ID of a remote worker. |
| Inbox | Items that need a person: approvals, questions, handoffs, failures and reviews. |
| Scorer | An LLM judge that labels sampled runs. Failures can start self-improvement. |
| Benchmark | A suite of tasks that runs under several configurations and is scored for comparison. |

## Glossary

Each word has one meaning in the docs, the CLI and the API.

| Word | Meaning |
|---|---|
| machine | The definition resource `machines/<name>.yaml` with an image and a compute shape. An agent or a scorer selects it with `machine:`. |
| runner | The process `openfactory runner` in the sandbox. It prepares the workspace, starts the harness and reports to the control plane. |
| work item | A unit of work that agents move through stages. CLI: `openfactory work-item`. MCP tools: `list_work_items` and the other `*_work_item` tools. |
| sandbox | The place where a run executes, for example a Kubernetes pod. The sandbox backend creates it from the sandbox image. |
| `workerHost: server` | The default worker host: the server runs the sandbox with its own backend. |
| `OF_SANDBOX_BACKEND=local` | The development backend. It runs the runner as a host process, with no isolation. |
| `image` | The image field: `platform.linux.image` of a machine in the definition, `image` of an environment in the REST API. |
| `openfactory mcp-registry` | Manages the team's MCP server registry. |
| `openfactory mcp` | Serves the Factory MCP over stdio. |

## Run states

| State | Meaning |
|---|---|
| `queued` | The run waits for a sandbox slot. |
| `pending` | A backend prepares the sandbox. |
| `claimed` | The runner does its setup, for example the repository checkout. |
| `running` | The harness works on the current turn. |
| `succeeded` | The last turn ended without an open request. The session stays available for follow-ups until the idle timeout. |
| `blocked` | The last turn ended with an open request to a person, or the harness asked for input. |
| `failed` | The run stopped because of the agent, for example the harness failed. |
| `error` | The run stopped because of the platform, for example a timeout or a sandbox that did not start. |
| `cancelled` | A person or the foreman cancelled the run. |

Older stored runs can also show `starting` or `idle`; nothing sets these states now. A follow-up message continues the run in the live session. If the session ended, the run goes back to `queued` and a new session restores the snapshot. See [How factories work](how-factories-work.md#5-child-runs-report-back).

## Next steps

- [Quickstart](quickstart.md): run a server and send a first work item.
- [How factories work](how-factories-work.md): follow one request from end to end.
- [Definition reference](definition-reference.md): write your own factory.
