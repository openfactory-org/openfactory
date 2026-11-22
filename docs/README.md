# Open Factory documentation

Open Factory is an open-source system that turns issues, chat messages, webhooks and schedules into reviewed pull requests. You deploy it to your own Cloudflare account. Read the sections in order the first time.

## Get started

| Page | What it covers |
|---|---|
| [Overview and concepts](overview.md) | Architecture, concepts, glossary and run states. |
| [Quickstart](quickstart.md) | A local control plane with `wrangler dev`, a first factory and a first work item. Then deploy. |

## Concepts

| Page | What it covers |
|---|---|
| [How factories work](how-factories-work.md) | The path of a request from an event to a merged pull request. |
| [Agents](agents.md) | Agent types, custom agents, `run_agents`, run metadata and follow-ups. |
| [Automations](automations.md) | Providers, events, filters and schedules that start work. |
| [Harnesses](harnesses.md) | The `claude` and `codex` harnesses, models and authentication. |
| [Skills](skills.md) | Factory, agent, repository and built-in skills (`SKILL.md`). |
| [Machines and environments](machines-and-environments.md) | Worker hosts, images, setup commands and instance shapes. |
| [Sandbox providers](sandbox-providers.md) | `cloudflare-sandbox`, `daytona`, `boatdev` and workers: capabilities, limits and selection. |
| [Secrets](secrets.md) | Encrypted secrets, secret types and `{{SECRET}}` templates. |
| [MCP servers](mcp-servers.md) | The team MCP server registry, the control-plane proxy, OAuth connections and built-in integration tools. |
| [Measure and improve](measure-and-improve.md) | Scorers, benchmarks, self-improvement and dashboard metrics. |

## Reference

| Page | What it covers |
|---|---|
| [Definition reference](definition-reference.md) | Every file and field of the definition tree, with its validation rules. |
| [REST API](api.md) | Authentication, endpoints and run streaming. The OpenAPI document is served at `GET /api/v1/openapi.json` (source: [api/openapi.json](../api/openapi.json)). |
| [Command-line client](cli.md) | Install the `openfactory` command, connect, and its relation to `openfactory-runtime`. |
| [Factory MCP](factory-mcp.md) | Connect Claude Code, Codex or another MCP client to your factories. |
| [Webhooks](webhooks.md) | Custom webhook ingress, authentication modes, signatures and payload filters. |
| [Dashboard and inbox](dashboard.md) | The dashboard pages and the human inbox. |
| [Example definitions](../examples/) | Ready-to-copy definitions for common setups. |

## Operate

| Page | What it covers |
|---|---|
| [Self-hosting overview](self-hosting/README.md) | Topology and install paths. |
| [Deploy to Cloudflare](self-hosting/cloudflare.md) | Account setup, the R2 bucket, secrets and `wrangler deploy`. |
| [Configuration](self-hosting/configuration.md) | Variables, secrets, bindings and key rotation. |
| [Self-hosted workers](self-hosted-machines.md) | Run sessions in Firecracker microVMs on your own Linux hosts. |
| [Object storage](self-hosting/object-storage.md) | Snapshots, artifacts and attachments in R2. |
| [OIDC login](self-hosting/oidc.md) | Single sign-on and role mapping. |
| [Plugins](self-hosting/plugins.md) | Plugin Workers, including GitLab and Azure DevOps. |
| [Workers](self-hosting/workers.md) | The development process worker and the worker protocol. |
| [Network policy](self-hosting/network-policy.md) | `networkPolicy`, built-in policies, the egress proxy and the in-sandbox lock. |
| [Private registries](self-hosting/private-registries.md) | Private images for each provider. |
| [Upgrading](self-hosting/upgrading.md) | Versions, deploys and Durable Object migrations. |
| [Security](security.md) | Security model, roles and permissions. |
| [Monitoring](monitoring.md) | Health endpoints, logs and metrics. |
| [Troubleshooting](troubleshooting.md) | Common errors and their fixes. |
| [Local testing](self-hosting/local-testing.md) | `wrangler dev`, the contract suite and live checks. |

## Integrations

| Page | What it covers |
|---|---|
| [Integrations overview](integrations/README.md) | How integrations connect to a team and a factory. |
| [GitHub](integrations/github.md), [GitLab](integrations/gitlab.md), [Azure DevOps](integrations/azure-devops.md), [Other forges](integrations/other-forges.md) | Code forges. |
| [Slack](integrations/slack.md), [Microsoft Teams](integrations/teams.md) | Chat. |
| [Linear](integrations/linear.md), [Jira](integrations/jira.md) | Issue trackers. |

## Use cases

| Page | What it covers |
|---|---|
| [Use cases overview](use-cases/README.md) | What each use case returns, and its example definition. |
| [Issue triage](use-cases/issue-triage.md), [Code review](use-cases/code-review.md), [Issue implementation](use-cases/issue-implementation.md), [Incident resolution](use-cases/incident-resolution.md), [End-to-end SDLC](use-cases/end-to-end-sdlc.md) | Complete setups, each with an example definition. |

## Contributing

- [Contributing](../CONTRIBUTING.md): development setup, checks and the [project layout](../CONTRIBUTING.md#project-layout).
- [Security policy](../SECURITY.md): how to report a vulnerability.
- [Deployment files](../deploy/): the sandbox image and bootstrap files.
