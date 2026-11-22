# Monitoring

Health endpoints, logs, dashboard metrics, API queries and worker metrics.

## Health endpoints

The control plane serves these without authentication:

| Endpoint | Response |
|---|---|
| `GET /healthz` | Liveness. `200` with `ok` while the Worker serves requests. |
| `GET /readyz` | Readiness. `200` with `ok`. |

`/metrics` is not public on Cloudflare. The dashboard shows run and queue state, and the API returns the same data (see [Monitor with the API](#monitor-with-the-api)).

## Logs

Observability is on for the Worker. Read logs in the Cloudflare dashboard, or stream them:

```bash
npx wrangler tail -c apps/control-plane/wrangler.jsonc
```

`OF_LOG_LEVEL` sets the level: `debug`, `info`, `warn` or `error`.

Useful log messages:

| Message | Meaning |
|---|---|
| `worker connected` / `worker disconnected` | A worker opened or closed its connection. |
| `worker heartbeat timeout` | The control plane closed a silent worker connection. |
| `worker connection lost; reconnecting` | Worker side: the connection dropped. |
| `network policy blocked egress` | The egress proxy of a sandbox blocked a host. The entry has the host. |

Logs of a sandbox provider Worker are separate. Tail it with `npx wrangler tail -c providers/cloudflare-sandbox/wrangler.jsonc`.

## Dashboard

The dashboard shows runs by state, the queue, workers (status, slots in use, last connection), dead-lettered deliveries and the factory metrics. See [Dashboard and inbox](dashboard.md).

## Monitor with the API

### Workers

```bash
curl -sS "${OF_PUBLIC_URL}/api/v1/workers" -H "Authorization: Bearer $OF_API_KEY"
```

Each worker has `kind` (`microvm` or `process`), `labels`, `slots`, `slots_used`, `last_seen`, `version` and `status` (`pending`, `online`, `offline`, `draining` or `revoked`). For a worker of a self-hosted host, see [Self-hosted workers](self-hosted-machines.md#operate).

### Runs

Failed runs of the last day:

```bash
curl -sS "${OF_PUBLIC_URL}/api/v1/runs?state=FAILED&since=1d" \
  -H "Authorization: Bearer $OF_API_KEY"
```

`state` accepts `QUEUED`, `PENDING`, `CLAIMED`, `RUNNING`, `BLOCKED`, `SUCCEEDED`, `FAILED`, `ERROR` and `CANCELLED`, and can repeat. `since` accepts an RFC 3339 time, a date (`2026-11-01`), or days or weeks (`1d`, `2w`). Runs that stay `QUEUED` mean that no worker serves their `workerHost` or that a limit is reached; see [Troubleshooting](troubleshooting.md#runs-stay-queued).

### Inbox failures

A failed run of a work item creates an inbox item of kind `failure` ("Run failed: ..."):

```bash
curl -sS "${OF_PUBLIC_URL}/api/v1/inbox" -H "Authorization: Bearer $OF_API_KEY"
```

### Factory metrics

`GET /api/v1/factories/{uid}/metrics?since=30d` returns the dashboard metrics of a factory: pull request activity, autonomy, cycle time and cost. See [Measure and improve](measure-and-improve.md).

## Suggested alerts

| Alert | Condition |
|---|---|
| Control plane down | `/readyz` fails. |
| Queue grows | The number of `queued` runs rises for 15 minutes. Check `workerHost` values and worker status. |
| Worker offline | A worker has status `offline` for 5 minutes. |
| Worker saturated | `slots_used` equals `slots` for 15 minutes. |

## Worker metrics

A worker that runs `openfactory-runtime worker` with `--metrics-addr` (or `OF_METRICS_ADDR`, for example `":9090"`) serves `GET /metrics` without authentication. The listener is off by default.

```bash
openfactory-runtime worker --process --id dev-box-1 --metrics-addr :9090
curl -s http://dev-box-1:9090/metrics
```

```yaml
# prometheus.yml
scrape_configs:
  - job_name: openfactory-workers
    static_configs:
      - targets: ["dev-box-1:9090"]
```

All series exist from startup with the value `0`, so dashboards and alerts work before the first sandbox.

| Metric | Type | Labels | Description |
|---|---|---|---|
| `of_worker_connected` | gauge | | `1` while the worker has an active connection to the control plane, else `0`. |
| `of_worker_sandboxes_active` | gauge | | Sandboxes that run now. |
| `of_worker_sandboxes_max_concurrent` | gauge | | Configured capacity. `0` means no limit. |
| `of_worker_sandboxes_claimed_total` | counter | | Sandboxes accepted since the process started. |
| `of_worker_sandboxes_rejected_total` | counter | `reason` | Sandboxes the worker declined. `reason` is `at_capacity` when all slots are in use, or `one_shot` when a `--one-shot` worker already has its sandbox. |
| `of_worker_sandboxes_completed_total` | counter | `result` | Completed sandboxes. `result` is `succeeded`, `failed`, `cancelled` or `dispatched`. |
| `of_worker_sandbox_duration_seconds` | histogram | `result` | Wall-clock sandbox duration. |
| `of_worker_sandbox_failures_total` | counter | `phase`, `reason` | Sandbox failures by phase and reason. |
| `of_worker_websocket_reconnects_total` | counter | `reason` | Reconnect attempts. `reason` is `dial_failed` or `remote_close`. |
| `of_worker_info` | gauge | `version`, `backend`, `worker_id` | Build and runtime information (always `1`). |

Duration histogram buckets, in seconds: 1, 5, 15, 30, 60, 120, 300, 600, 1200, 1800, 3600, 7200, 14400, 28800.

### Failure reasons

In `of_worker_sandbox_failures_total`, `phase` is `setup` for a sandbox that never started and `run` for one that started and then failed. `reason` is the sandbox's end reason:

| `phase` | `reason` | Cause |
|---|---|---|
| `setup` | `image_pull` | The image pull failed . |
| `setup` | `unschedulable` | No slot or resource was free. |
| `setup` | `network_policy_timeout` | The NetworkPolicy of a restricted sandbox did not take effect in time. |
| `setup` | `invalid_spec` | The provider refused the sandbox spec. |
| `run` | `process_failed` | The main process ended with a non-zero exit code. |
| `run` | `container_oom` | The container was killed for memory. |
| `run` | `evicted` | The platform evicted the sandbox. |
| `run` | `node_lost` | The host was lost or shut down. |
| `run` | `deadline_exceeded` | The sandbox passed its deadline. |
| `run` | `unknown` | The provider cannot tell the reason. |

A sandbox that ends with `completed` or `cancelled` is not a failure.

### Example worker queries

```promql
# Disconnected worker
of_worker_connected == 0
# Capacity use per worker
of_worker_sandboxes_active / on(instance) (of_worker_sandboxes_max_concurrent > 0)
# Rejected sandboxes in the last 15 minutes
increase(of_worker_sandboxes_rejected_total[15m]) > 0
# Failure ratio over one hour
sum(increase(of_worker_sandboxes_completed_total{result="failed"}[1h]))
  / sum(increase(of_worker_sandboxes_completed_total[1h]))
# 95th percentile sandbox duration
histogram_quantile(0.95, sum by (le) (rate(of_worker_sandbox_duration_seconds_bucket[1h])))
# Setup failures by reason
sum by (reason) (increase(of_worker_sandbox_failures_total{phase="setup"}[1h]))
```

### Suggested worker alerts

| Alert | Condition |
|---|---|
| Worker offline | `of_worker_connected == 0` for 5 minutes. |
| Worker saturated | Capacity use above 90% for 15 minutes, or rejected sandboxes increase. |
| Image pull failures | `of_worker_sandbox_failures_total{reason="image_pull"}` increases. |
| Reconnect loop | `increase(of_worker_websocket_reconnects_total[10m]) > 5`. |

