# Self-hosting Open Factory

Open Factory is one Go binary, `openfactory`, that runs on your Kubernetes cluster.

## Pages

| Page | Content |
|---|---|
| [Kubernetes](kubernetes.md) | Helm install, quick install, pod template, RBAC, sandbox images. |
| [Configuration](configuration.md) | Every environment variable and flag. |
| [Workers](workers.md) | Remote workers that run sandboxes in another cluster or network. |
| [Object storage](object-storage.md) | Local disk, S3, GCS and Azure Blob. |
| [OIDC login](oidc.md) | Single sign-on for the dashboard. |
| [Plugins](plugins.md) | GitLab, Azure DevOps and other gRPC plugins. |
| [Network policy](network-policy.md) | Egress control for sandboxes. |
| [Private registries](private-registries.md) | Image pull secrets and image mirrors. |
| [Upgrading](upgrading.md) | Versions, releases and upgrade steps. |
| [Local testing](local-testing.md) | The full stack on a workstation, and the [real-world checks](local-testing.md#real-world-checks). Test only. |

## Install paths

| Path | For | Guide |
|---|---|---|
| Helm chart on Kubernetes | Platform teams. The main supported install. | [Kubernetes](kubernetes.md) |
| Helm chart in SQLite mode, no values file | Small teams with a cluster, for example k3s on one VM. | [Quick install](kubernetes.md#quick-install) |
| `openfactory serve` with `OF_DEV=true` | Evaluation on a laptop. | [Quickstart](../quickstart.md) |
| `make local-up` on OrbStack or kind | Tests of Open Factory itself. **Not for production.** | [Local testing](local-testing.md) |

Docker and bare-host execution are not supported in production. In production, each run executes as a Kubernetes Job.

## Deployment parts

| Part | What it does | How you run it |
|---|---|---|
| Server replicas | Each replica receives events, runs the engine, and serves the REST API, the dashboard, the Factory MCP server and the per-run orchestration MCP server. One replica holds the leader lease and runs the scheduler and timers. | `openfactory serve` (StatefulSet or Deployment) |
| Database | The only source of truth: factories, definitions, work items, runs, run queue, outbox, leases and encrypted secrets. | SQLite (one replica) or PostgreSQL (one or more) |
| Object storage | Artifacts, raw harness transcripts, snapshots and prompt attachments. Run messages stay in the database. | See [Object storage](object-storage.md). |
| Sandbox backend | Assigns one sandbox per run. The `kubernetes` backend creates one Job per sandbox. | Built into `serve` |
| Remote worker | Runs sandboxes in another cluster or network. It connects out to the server over a WebSocket. | `openfactory worker --backend kubernetes` |
| Plugins | GitLab, Azure DevOps and community integrations. | See [Plugins](plugins.md). |

The server only assigns a sandbox. The runner (`openfactory runner`) runs inside the sandbox, runs the harness, and sends events to the server over HTTP with a session token. The server stores the transcript. Workers always connect outbound, so the server never needs to reach into a customer network.

A server replica holds no agent turn. A replica restart or a rolling upgrade does not interrupt a turn: the runner sends events with a cursor and reconnects to any replica.

| URL | Must be reachable from | Default |
|---|---|---|
| `OF_PUBLIC_URL` | Browsers, forges, chat providers, and sandboxes of remote workers. | `http://localhost:8080` |
| `OF_INTERNAL_URL` | Every sandbox pod of the server backend. It can be an in-cluster address. | `OF_PUBLIC_URL` |

Each run gets an orchestration MCP server at `/mcp/run/{runId}` on the URL that its sandbox uses.

## Database modes

| Mode | Replicas | Chart workload | Use it for |
|---|---|---|---|
| SQLite | Exactly 1 | StatefulSet with a PersistentVolumeClaim for the database and local object storage | Small teams, one node, simple backups. |
| PostgreSQL | 1 or more | Deployment, with a PodDisruptionBudget when there is more than one replica | High availability, upgrades without downtime, more concurrent runs. |

Both modes are supported in production. The chart refuses `server.replicaCount` above 1 in SQLite mode. With more than one PostgreSQL replica, use shared object storage (S3, GCS or Azure Blob), because replicas do not share a disk.

## Sandbox backends

| Backend | Isolation | Network policy | Use |
|---|---|---|---|
| `kubernetes` | One Job per sandbox, with an `emptyDir` at `/workspace`. | Kubernetes NetworkPolicy. | Production. The default. |
| `direct` | A remote worker that runs the `kubernetes` backend in its own cluster. | As in the worker's cluster. | Private networks, VPN-only repositories, separate build clusters. |
| `local` | One runner process and workspace directory per sandbox. Processes share the host kernel, file system and network. | Not enforced. | Development and tests only. The default when `OF_DEV=true`. Outside dev mode, select it explicitly; the server logs a warning. |

In each sandbox, the runner checks out the repositories, then runs the machine's `setupCommands`, then the environment's setup commands. A setup command that fails or runs longer than 30 minutes fails the run with `environment_setup_failed`. See [Sandbox setup fails](../troubleshooting.md#sandbox-setup-fails).

## Route runs to a backend

The agent field `workerHost` (or `agentDefaults.workerHost` in `factory.yaml`) selects where runs execute.

| `workerHost` | Result |
|---|---|
| empty, `server`, `kubernetes`, `direct` or `managed` | The server backend (`kubernetes` in production). |
| the name of the server backend | The server backend, for example `local` in development. |
| any other value | The remote worker with that id. See [Workers](workers.md). |

## Production checklist

1. Set `OF_SECRET_KEY` to a stable random key and back it up. See [Secret key](configuration.md#secret-key).
2. Choose the database mode. Back up the SQLite volume or the PostgreSQL database.
3. Configure [object storage](object-storage.md). Keep local disk only in SQLite mode.
4. Set `OF_PUBLIC_URL` to an HTTPS URL that forges, chat providers and browsers can reach.
5. Put a TLS-terminating ingress in front of the server.
6. Do not set `OF_DEV=true`. Keep the `kubernetes` sandbox backend.
7. Configure [OIDC login](oidc.md), or bootstrap the first admin with `POST /api/v1/bootstrap` (see [Security](../security.md#bootstrap-the-first-admin)).
8. Configure [network policies](network-policy.md) for agents that handle untrusted input.
9. Scrape `/metrics` and watch `/healthz` and `/readyz`. See [Monitoring](../monitoring.md).
10. Read [Upgrading](upgrading.md) before you change versions.

See also [Security](../security.md) and [Troubleshooting](../troubleshooting.md).
