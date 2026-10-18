# Workers

A remote worker runs sandboxes in a Kubernetes cluster outside the control plane. In production a worker uses the `kubernetes` backend. The `local` backend is for development and tests only.

Use a remote worker when:

- repositories, package mirrors, databases or MCP servers are reachable only from a private network;
- builds need hardware, node pools or tools that the control plane cluster does not have;
- source checkouts and build artifacts must stay in your cluster.

The worker connects out to the control plane over a WebSocket. No inbound firewall port is needed in the worker's network.

## Start a worker

1. Create an API key for the worker. Use a dedicated user or service account with the team-wide `admin` role, so that you can revoke the key without side effects.

   ```bash
   curl -sS -X POST "${OF_PUBLIC_URL}/api/v1/api-keys" \
     -H "Authorization: Bearer $OF_API_KEY" \
     -H "Content-Type: application/json" \
     -d '{"name": "worker acme-prod-worker"}'
   ```

   The response shows the key once. Store it in a secret manager.

2. Start the worker in the target cluster. With the Helm chart, enable only the worker:

   ```yaml
   # worker-values.yaml
   server:
     enabled: false
   worker:
     enabled: true
     id: acme-prod-worker
     serverURL: https://factories.example.com
     existingSecret: factory-worker   # key "api-key"
     capacity: 4
     labels: {region: eu-west-1}
     metricsAddr: ":9090"
   sandbox:
     namespace: factory-sandboxes
     createNamespace: true
   ```

   ```bash
   helm upgrade --install factory-worker deploy/helm/openfactory \
     --namespace factory-worker --create-namespace -f worker-values.yaml
   ```

   Or run the command in a pod whose service account can manage pods in the sandbox namespace (see [RBAC](kubernetes.md#rbac)):

   ```bash
   export OF_SERVER="https://factories.example.com"
   export OF_API_KEY="<worker API key>"
   export OF_K8S_NAMESPACE=factory-sandboxes
   openfactory worker \
     --id acme-prod-worker \
     --name "ACME production worker" \
     --backend kubernetes \
     --capacity 4 \
     --label region=eu-west-1 \
     --metrics-addr :9090
   ```

   Without `--id`, the worker uses the host name. For all flags, their variables and the id rules, see [Worker flags](configuration.md#worker-flags).

3. Check that the worker is online:

   ```bash
   openfactory worker list
   ```

   `GET /api/v1/workers/acme-prod-worker` returns `"status": "online"` when the last heartbeat is less than 2 minutes old.

4. Route agents to the worker. The value must match the worker id exactly.

   ```yaml
   # factory.yaml
   agentDefaults:
     workerHost: acme-prod-worker
   ```

See [examples/07-self-hosted-worker](../../examples/07-self-hosted-worker/) for a complete factory.

### Worker API key

- The key must have the team-wide `admin` role (user or service account). The server refuses other keys with `403`. The worker's runs belong to the team of the key.
- A key cannot yet be limited to a list of worker ids. Any admin key of the team works for any worker id that no other principal holds.
- While a worker id is connected, only the same user or service account can connect with that id again. A worker id belongs to one team; the server refuses the same id from another team.
- The key does not go into sandboxes.
- If the server rejects the key, the worker stops with `worker API key rejected by server` and does not retry.

## Routing and queueing

- A run with `workerHost: <id>` waits in `queued`, without a session, until a worker with that id is connected and has a free slot.
- After `OF_QUEUE_TIMEOUT` (default 30 minutes), a run still `queued` goes to `error` with `infrastructure_timeout`. See [Runs stay queued](../troubleshooting.md#runs-stay-queued).
- If the worker has no free slot when the sandbox arrives, the control plane releases the claim and the run stays `queued`.
- If several processes connect with the same id, the control plane uses the least-loaded connection with free capacity. Use this for load balancing.
- `capacity` limits sandboxes that are not `exited`. `0` means no limit. Sessions on remote workers do not count for `OF_MAX_SANDBOXES`.
- Labels are recorded and shown by the API. They do not affect routing.
- The control plane sets `OF_WORKER_ID` in the sandbox environment.
- The runner in a worker sandbox uses `OF_PUBLIC_URL` as its control URL, so the worker network must reach that URL. Sandboxes of the server backend use `OF_INTERNAL_URL`.

## Sandbox settings

- The worker reads backend settings from the same `OF_*` variables as the server, for example `OF_K8S_NAMESPACE`, `OF_RUNNER_IMAGE` and `OF_SANDBOX_IMAGE`.
- If a sandbox has the image `local` (the control plane sends `local` when it has no image), the worker uses `OF_SANDBOX_IMAGE`.
- If `OF_IDLE_TIMEOUT_MAX_SECONDS` is set in the worker environment, the worker adds it to each sandbox. The runner uses the lower of the two idle times.
- `openfactory worker` has no setup or teardown hooks. Put preparation steps in the machine's `setupCommands` or the environment's setup commands. See [Machines and environments](../machines-and-environments.md).
- To give sandboxes credentials, use the sandbox image, the pod template (Kubernetes Secrets), or [secrets](../secrets.md).

## Connection and recovery

| Behaviour | Value |
|---|---|
| Heartbeat interval | 15 seconds (sent by the server in `welcome`) |
| Server closes a silent connection after | 3 intervals (45 seconds) |
| Hello timeout | 10 seconds |
| API status `online` | Last heartbeat less than 2 minutes ago |
| Reconnect delay | Exponential backoff with jitter, 1 to 60 seconds |

A worker that stops or loses its connection leaves its sandboxes running. Each runner keeps its own connection to the control plane. The worker can connect to any server replica. On reconnect, it lists its sandboxes in `hello` and the control plane updates their states.

While a worker is disconnected, the control plane treats its sandboxes like a silent runner: it waits two times 90 seconds, then ends the session with `runner_lost`.

The worker and the server must use the same worker protocol (version 2). Otherwise the server closes the connection with `unsupported worker protocol: this server needs worker protocol v2`. Use a worker of the same release as the server.

## One-shot workers

With `--one-shot`, the worker takes one sandbox and exits when it exited. Use it when a job system starts one worker per run.

```bash
openfactory worker --one-shot --id acme-job-worker --backend kubernetes
```

- The exit code is `0` only if the sandbox ended with `completed`.
- After `completed` or `cancelled`, the worker destroys the sandbox. After other reasons, it keeps the sandbox so you can read the logs; the sandbox retention time then applies.

## Worker protocol

The worker connects to `GET /api/v1/workers/connect` with the WebSocket subprotocol `openfactory.worker.v2` and a team API key as bearer token. The worker runs sandboxes; the runner in each sandbox talks to the control plane over HTTP with a session token. The control plane does not send commands to the sandbox.

Messages are JSON text frames with the fields `type`, `id`, `method`, `params`, `result` and `error`.

| Direction | Message | Content |
|---|---|---|
| worker to server | `hello` | `worker_id`, `name`, `backend`, `labels`, `capacity`, `version`, `one_shot`, `tasks` (the sandboxes it holds) |
| server to worker | `welcome` | `connection_id`, `heartbeat_ms` |
| worker to server | `heartbeat` | `active`, `capacity` |
| server to worker | request `task.assign` | `task` |
| server to worker | request `task.cancel` | `task_id`, `grace_seconds` |
| server to worker | request `task.status` | `task_id` |
| server to worker | request `task.destroy` | `task_id` |
| worker to server | `response` | `status`, or an `error` with code `at_capacity`, `invalid_spec`, `not_found` or `internal` |
| worker to server | event `task.state` | `status`, on each change of a sandbox |

- `task.assign` returns when the backend accepts the spec. It does not wait for the sandbox to start.
- The control plane takes sandbox states from `hello` and removes a sandbox that the worker does not list. It does not destroy sandboxes on connect; the dispatcher's orphan sweep destroys sandboxes that have no open session.

## The local backend (development only)

> **Warning:** The `local` backend runs agents as host processes. Commands share the host kernel, file system and network, and no network policy is enforced. Use it only on a host that you trust the agents with. In production, use `kubernetes`.

- It is the default of `openfactory serve` with `OF_DEV=true`. Elsewhere, select it explicitly; the process logs a warning.
- Workspaces are in `OF_SANDBOX_ROOT` (default `~/.openfactory/sandboxes`). Each sandbox gets its own directory, and the runner clones each repository into its `workspace` sub-directory. The directory is removed when the run ends.
- Sandboxes get only `HOME`, `TMPDIR` and `PATH` from the host, plus the run's variables and secrets. The worker's own variables, including its API key, are not passed.
- The host must provide `git`, `sh` and the harness commands (`claude`, `codex`) that the agents use.

See also [Monitoring](../monitoring.md#worker-metrics) and [Worker does not connect](../troubleshooting.md#worker-does-not-connect).
