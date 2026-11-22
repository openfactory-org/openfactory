# Self-hosting

Open Factory runs in your Cloudflare account. You deploy one Worker, and agent runs execute on hosted sandbox providers or on your own Linux hosts.

## Topology

| Part | What it is |
|---|---|
| Control plane | One Cloudflare Worker. It serves the REST API (`/api/v1`), the dashboard, the Factory MCP (`/mcp`), webhooks and integration endpoints, and the runner API (`/runner/v1`). |
| State | Durable Objects with SQLite storage: one for the team data, one for each work item, and others for dispatch, routing, the read model, inbound events, worker connections and benchmarks. |
| Run data | An R2 bucket: snapshots, artifacts, attachments, transcripts. See [Object storage](object-storage.md). |
| Plugins | Separate Workers over service bindings: GitLab, Azure DevOps and your own. See [Plugins](plugins.md). |
| Sandboxes | Where agents run: `cloudflare-sandbox`, `daytona`, `boatdev`, or a microVM worker on your own host. See [Sandbox providers](../sandbox-providers.md). |
| Runtime | `openfactory-runtime`, a Go program in every sandbox. It prepares the workspace, starts the harness and reports to the control plane. |

Every sandbox makes outbound HTTPS calls to the control plane. The control plane never connects into a sandbox.

## Install paths

| Goal | Page |
|---|---|
| Deploy to your Cloudflare account | [Deploy to Cloudflare](cloudflare.md) |
| Try it on your workstation | [Quickstart](../quickstart.md) |
| Run agents on your own Linux hosts | [Self-hosted workers](../self-hosted-machines.md) |
| Test changes to Open Factory | [Local testing](local-testing.md) |

## Operate

| Page | What it covers |
|---|---|
| [Configuration](configuration.md) | Variables, secrets, bindings and key rotation. |
| [Object storage](object-storage.md) | The R2 bucket and presigned URLs. |
| [OIDC login](oidc.md) | Single sign-on and role mapping. |
| [Plugins](plugins.md) | Plugin Workers. |
| [Network policy](network-policy.md) | Egress control for runs. |
| [Private registries](private-registries.md) | Private sandbox images. |
| [Workers](workers.md) | The development process worker. |
| [Upgrading](upgrading.md) | Versions, deploys and Durable Object migrations. |
| [Security](../security.md) | The security model. |
| [Monitoring](../monitoring.md) | Logs, metrics and health. |
| [Troubleshooting](../troubleshooting.md) | Common errors and their fixes. |

## Teams

An install has one team, with the roles admin, editor and user. Integrations, secrets, environments and workers belong to the team.
