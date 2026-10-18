# Open Factory documentation

Open Factory is an open-source, self-hosted system that turns issues, chat messages, webhooks and schedules into reviewed pull requests. Read the sections in order the first time.

## Get started

| Page | What it covers |
|---|---|
| [Overview and concepts](overview.md) | Architecture, concepts, glossary and run states. |
| [Quickstart](quickstart.md) | A local server in development mode, a first factory and a first work item. |

## Concepts

| Page | What it covers |
|---|---|
| [How factories work](how-factories-work.md) | The path of a request from an event to a merged pull request. |
| [Agents](agents.md) | Agent types, custom agents, `run_agents`, run metadata and follow-ups. |
| [Automations](automations.md) | Providers, events, filters and schedules that start work. |
| [Harnesses](harnesses.md) | The `claude` and `codex` harnesses, models and authentication. |
| [Skills](skills.md) | Factory, agent, repository and built-in skills (`SKILL.md`). |
| [Machines and environments](machines-and-environments.md) | Worker hosts, images, setup commands and instance shapes. |
| [Secrets](secrets.md) | Encrypted secrets, secret types and `{{SECRET}}` templates. |
| [MCP servers](mcp-servers.md) | The team MCP server registry, the control-plane proxy, OAuth connections and built-in integration tools. |
| [Measure and improve](measure-and-improve.md) | Scorers, benchmarks, self-improvement and dashboard metrics. |

## Reference

| Page | What it covers |
|---|---|
| [Definition reference](definition-reference.md) | Every file and field of the definition tree, with its validation rules. |
| [REST API and Go SDK](api.md) | Authentication, endpoints, run streaming, the CLI client and the Go SDK. The OpenAPI document is served at `GET /api/v1/openapi.json` (source: [internal/api/openapi.json](../internal/api/openapi.json)). |
| [Factory MCP](factory-mcp.md) | Connect Claude Code, Codex or another MCP client to your factories. |
| [Webhooks](webhooks.md) | Custom webhook ingress, authentication modes, signatures and payload filters. |
| [Dashboard and inbox](dashboard.md) | The dashboard pages and the human inbox. |
| [Example definitions](../examples/) | Ready-to-copy definitions for common setups. |

## Operate

| Page | What it covers |
|---|---|
| [Self-hosting overview](self-hosting/README.md) | Install paths, topology, database modes and sandbox backends. |
| [Kubernetes](self-hosting/kubernetes.md) | The Helm chart (SQLite and PostgreSQL modes) and the `kubernetes` sandbox backend. [Quick install](self-hosting/kubernetes.md#quick-install) is one `helm install` with no values file, for small teams (for example k3s on one VM). Until the first release publishes the OCI chart, install the in-repo chart `deploy/helm/openfactory`. |
| [Configuration](self-hosting/configuration.md) | Every setting of `openfactory serve` and `openfactory worker`. |
| [Object storage](self-hosting/object-storage.md) | Snapshots, artifacts and attachments in S3, GCS, Azure Blob or local disk. |
| [OIDC login](self-hosting/oidc.md) | Single sign-on and role mapping. |
| [Plugins](self-hosting/plugins.md) | gRPC plugins, including GitLab and Azure DevOps. |
| [Remote workers](self-hosting/workers.md) | Workers that run sandboxes in another cluster or network. |
| [Network policy](self-hosting/network-policy.md) | `networkPolicy`, built-in policies and the egress proxy. |
| [Private registries](self-hosting/private-registries.md) | Image pull secrets and mirroring the sandbox image. |
| [Upgrading](self-hosting/upgrading.md) | Versions, releases and upgrades. |
| [Security](security.md) | Security model, roles and permissions. |
| [Monitoring](monitoring.md) | Health endpoints, Prometheus metrics and logs. |
| [Troubleshooting](troubleshooting.md) | Common errors and their fixes. |
| [Local testing](self-hosting/local-testing.md) | Test the whole flow on a workstation with the AWS emulator and a local cluster. Test only. |

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
- [Deployment files](../deploy/): the Helm chart, Dockerfiles, a pod template and the local test stack.
