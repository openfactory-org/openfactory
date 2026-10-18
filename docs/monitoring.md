# Monitoring

Health endpoints, Prometheus metrics for the server and remote workers, API queries and logs.

## Health endpoints

`openfactory serve` serves these on the `OF_LISTEN` address, without authentication. Requests to them are logged at `debug` level only.

| Endpoint | Response |
|---|---|
| `GET /healthz` | Liveness. Always `200` with `{"status": "ok", "version": "..."}` while the process serves HTTP. |
| `GET /readyz` | Readiness. `200` with `{"status": "ready"}` after startup, when a database query succeeds within 3 seconds. `503` with `{"status": "starting"}` during startup, or `{"status": "unavailable", "error": "store: ..."}` when the database fails. |
| `GET /metrics` | Prometheus text exposition format. The server computes the values at most once every 5 seconds. |

Kubernetes probe example:

```yaml
livenessProbe:
  httpGet: { path: /healthz, port: 8080 }
  periodSeconds: 10
readinessProbe:
  httpGet: { path: /readyz, port: 8080 }
  periodSeconds: 5
```

## Scrape metrics

`/metrics` is on the main listener, so it is reachable wherever the API is. If the server is public, block `/metrics` at your reverse proxy and scrape from inside your network.

```yaml
# prometheus.yml
scrape_configs:
  - job_name: openfactory
    metrics_path: /metrics
    static_configs:
      - targets: ["openfactory.internal:8080"]
```

## Server metrics

All server series are gauges. Series that end in `_total` count since the process started, so `rate()` and `increase()` work on them.

| Metric | Labels | Description |
|---|---|---|
| `of_build_info` | `version` | Build information (always `1`). |
| `of_runs` | `state` | Agent runs by state: `queued`, `starting`, `running`, `idle`, `pending`, `claimed`, `blocked`, `succeeded`, `failed`, `error`, `cancelled`. Counting stops at 100,000 runs per state. |
| `of_run_queue_depth` | | Runs that wait to be claimed (the same value as `of_runs{state="queued"}`). |
| `of_workers_connected` | | Connected remote workers (distinct worker ids). |
| `of_worker_connections` | `worker`, `backend` | Live connections per remote worker id. |
| `of_worker_capacity` | `worker` | Sandbox capacity per remote worker, summed over its connections. `0` means no limit. |
| `of_worker_active_sandboxes` | `worker` | Active sandboxes per remote worker. |
| `of_event_bus_published_total` | | Events published on the in-process event bus. |
| `of_event_bus_dropped_total` | | Events dropped because a subscriber was too slow. |
| `of_outbox_deliveries_total` | `result` | Durable outbox deliveries of this replica by result: `delivered`, `retried`, `dead_lettered`, `in_process_fallback`. |
| `of_leader` | `replica` | `1` when this replica holds the leader lease, else `0`. |
| `of_plugins_loaded` | | Plugins that run in this replica. |
| `of_notifications_total` | `result` | Outbound notifications by result: `delivered`, `failed`, `unroutable`, `retried`. |

`of_runs` counts all runs in the database, so terminal states grow over time; use `delta()` on them. `starting` and `idle` appear only for older stored runs.

Example queries:

```promql
of_run_queue_depth > 10                                        # queue backlog
of_worker_active_sandboxes / (of_worker_capacity > 0)          # remote worker saturation
increase(of_event_bus_dropped_total[15m]) > 0                  # dropped events
increase(of_notifications_total{result="failed"}[15m]) > 0     # failed notifications
```

### Suggested server alerts

| Alert | Condition |
|---|---|
| Server down | `up{job="openfactory"} == 0`, or `/readyz` fails. |
| Queue grows | `of_run_queue_depth` rises for 15 minutes. Check `workerHost` values and worker status. |
| Worker missing | `of_workers_connected` is lower than the number of workers you run. |
| Events dropped | `of_event_bus_dropped_total` increases. |

## Worker metrics

A remote worker started with `--metrics-addr` (or `OF_METRICS_ADDR`, or the Helm value `worker.metricsAddr`, for example `":9090"`) serves `GET /metrics` without authentication. The listener is off by default.

```bash
openfactory worker --id cluster-eu-1 --backend kubernetes --metrics-addr :9090
curl -s http://cluster-eu-1:9090/metrics
```

```yaml
# prometheus.yml
scrape_configs:
  - job_name: openfactory-workers
    static_configs:
      - targets: ["cluster-eu-1:9090"]
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
| `setup` | `image_pull` | The image pull failed (`ImagePullBackOff` or `InvalidImageName`). |
| `setup` | `unschedulable` | The Kubernetes Pod stayed unschedulable. |
| `setup` | `network_policy_timeout` | The NetworkPolicy of a restricted sandbox did not take effect in time. |
| `setup` | `invalid_spec` | The backend refused the sandbox spec. |
| `run` | `process_failed` | The main process ended with a non-zero exit code. |
| `run` | `container_oom` | The container was killed for memory. |
| `run` | `evicted` | Kubernetes evicted the Pod. |
| `run` | `node_lost` | The node was lost or shut down. |
| `run` | `deadline_exceeded` | The sandbox passed its deadline. |
| `run` | `unknown` | The backend cannot tell the reason. |

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

## Monitor with the API

### Workers

```bash
curl -sS "${OF_PUBLIC_URL}/api/v1/workers" -H "Authorization: Bearer $OF_API_KEY"
```

Each worker has `backend`, `labels`, `capacity`, `active`, `last_heartbeat`, `version` and `status` (`online` when the last heartbeat is under two minutes old, else `offline`).

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

## Logs

- `OF_LOG_LEVEL` sets the level: `debug`, `info`, `warn` or `error`.
- `OF_LOG_FORMAT=json` writes one JSON object per line. Use it with a log collector.

Useful log messages:

| Message | Meaning |
|---|---|
| `worker connected` / `worker disconnected` | A remote worker opened or closed its connection. |
| `worker heartbeat timeout` | The server closed a silent worker connection. |
| `worker connection lost; reconnecting` | Worker side: the connection dropped. |
| `task job created` | The `kubernetes` backend created the Job of a sandbox. The entry has the task, the Job and the namespace. |
| `network policy blocked egress` | The egress proxy of a sandbox blocked a host. The entry has the host. |
