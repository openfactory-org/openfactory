# Workers

A worker is a host that runs agent sessions for the control plane. Two kinds exist:

| Kind | Isolation | Use |
|---|---|---|
| microVM worker | One Firecracker microVM for each session | Production. See [Self-hosted workers](../self-hosted-machines.md). |
| Development process worker | None. Each session is a host process. | Development and tests only. |

This page covers the development process worker.

## Development process worker

> **Warning:** The process worker runs agents as host processes. Commands share the host kernel, file system and network, and no network policy is enforced (`egress: none`). Use it only on a host that you trust the agents with. The control plane accepts it only when `OF_DEV_PROCESS_WORKERS=true`. Do not set that variable in production.

It needs `openfactory-runtime` and the harness commands that your agents use (`claude`, `codex`) on the host, and `git` and `sh`.

### Start a worker

1. Allow development workers on the control plane. For `wrangler dev`, put this in `apps/control-plane/.dev.vars`:

   ```text
   OF_DEV_PROCESS_WORKERS=true
   ```

2. Create an API key for the worker. Use a dedicated user or service account with the team-wide `admin` role.

   ```bash
   curl -sS -X POST "${OF_SERVER}/api/v1/api-keys" \
     -H "Authorization: Bearer $OF_API_KEY" \
     -H "Content-Type: application/json" \
     -d '{"name": "worker dev-box-1"}'
   ```

   The response shows the key once.

3. Start the worker:

   ```bash
   export OF_SERVER="http://localhost:8787"
   export OF_API_KEY="<worker API key>"
   openfactory-runtime worker --process \
     --id dev-box-1 \
     --name "Development box" \
     --capacity 2 \
     --label region=eu
   ```

   Without `--id`, the worker uses the host name. For all flags, see [Runtime flags](configuration.md#development-process-worker).

4. Check that the worker is online:

   ```bash
   openfactory worker list
   ```

5. Route agents to the worker. The value must match the worker ID exactly.

   ```yaml
   # factory.yaml
   agentDefaults:
     workerHost: dev-box-1
   ```

To make the process worker the default for `workerHost: server`, set `OF_SANDBOX_PROVIDER=local` on the control plane.

### Worker API key

- The key must have the team-wide `admin` role. The control plane refuses other keys with `403`. The runs of the worker belong to the team of the key.
- While a worker ID is connected, only the same user or service account can connect with that ID again.
- The key does not go into sandboxes. Sessions get only `HOME`, `TMPDIR` and `PATH` from the host, plus the variables and secrets of the run.
- If the control plane rejects the key, the worker stops with `worker API key rejected by server` and does not retry.

### Workspace

Workspaces are in `OF_SANDBOX_ROOT` (default `~/.openfactory/sandboxes`). Each session gets its own directory, and the runner clones each repository into its `workspace` sub-directory. The directory is removed when the run ends.

## Routing and queueing

- A run with `workerHost: <id>` waits in `queued` until a worker with that ID is connected and has a free slot.
- After `OF_QUEUE_TIMEOUT` (default 30 minutes), a run that is still `queued` goes to `error` with `infrastructure_timeout`. See [Runs stay queued](../troubleshooting.md#runs-stay-queued).
- `capacity` limits sessions that are not `exited`. `0` means no limit.
- Labels select workers with `workerHost: pool:<label>`.
- The control plane sets `OF_WORKER_ID` in the session environment.
- The runner uses `OF_PUBLIC_URL` as its control URL, so the host must reach that URL.
- `openfactory-runtime worker` has no setup or teardown hooks. Put preparation steps in the `setupCommands` of a machine or in the environment's setup commands. See [Machines and environments](../machines-and-environments.md).

## Connection and recovery

| Behaviour | Value |
|---|---|
| Heartbeat interval | 15 seconds (ping and pong) |
| Status `online` | Last heartbeat less than 2 minutes ago |
| Reconnect delay | Exponential backoff with jitter, 1 to 60 seconds |

A worker that stops or loses its connection leaves its sessions running. Each runner keeps its own connection to the control plane. On reconnect, the worker lists its sessions in `hello` and the control plane updates their states. While a worker is disconnected, the control plane waits 90 seconds, then 180 seconds, then ends the session with `runner_lost`.

The worker and the control plane must use the same worker protocol (version 2). Use a worker of the same release as the control plane.

## One-shot workers

With `--one-shot`, the worker takes one session and exits when it exited. The exit code is `0` only if the session ended with `completed`.

```bash
openfactory-runtime worker --process --one-shot --id dev-job-worker
```

## Worker protocol

The worker connects to `GET /api/v1/workers/connect` with the WebSocket subprotocol `openfactory.worker.v2`. The control plane speaks first with a `challenge` frame, and the worker answers with a signature of its key or a team API key. The schema is in `schemas/worker/v2/`.

Messages are JSON text frames with the fields `type`, `id`, `method`, `params`, `result` and `error`.

| Direction | Message | Content |
|---|---|---|
| worker to server | `hello` | `worker_id`, `name`, `labels`, `capacity`, `version`, `one_shot`, `facts`, `tasks` (the sessions it holds) |
| server to worker | `welcome` | `connection_id`, `heartbeat_ms` |
| server to worker | request `task.assign` | `task` |
| server to worker | request `task.cancel` | `task_id`, `grace_seconds` |
| server to worker | request `task.status` | `task_id` |
| server to worker | request `task.list` | none |
| server to worker | request `task.destroy` | `task_id` |
| worker to server | `response` | `status`, or an `error` with code `at_capacity`, `invalid_spec`, `not_found` or `internal` |
| worker to server | event `task.state` | `status`, on each change of a session |

`task.assign` returns when the worker accepts the spec. It does not wait for the session to start.

See also [Monitoring](../monitoring.md#worker-metrics) and [Worker does not connect](../troubleshooting.md#worker-does-not-connect).
