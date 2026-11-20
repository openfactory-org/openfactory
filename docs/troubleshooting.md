# Troubleshooting

Each section starts with the symptom or the exact error text.

## First steps

1. Check that the server is ready: `curl -fsS ${OF_PUBLIC_URL}/readyz`. A `503` response names the cause.
2. Set `OF_LOG_LEVEL=debug` and reproduce the problem.
3. Read the run transcript in the dashboard, or with `GET /api/v1/runs/{runId}/messages`. A failed run has an `error` field with the failure code and the cause. The requester gets the inbox item `Run failed: <work item title>`.
4. Check the inbox for items of kind `failure`.

## Run states

The API shows run states in upper case.

| API state | Meaning |
|---|---|
| `QUEUED` | The run waits for capacity, or for a worker that serves its `workerHost`. |
| `PENDING` | A backend prepares the sandbox. |
| `CLAIMED` | The sandbox exists and the runner does its setup. |
| `RUNNING` | A turn is active. |
| `BLOCKED` | The last turn ended with an open request to a person, or the harness asked for input. `blocked_reason` tells which. |
| `SUCCEEDED` | The last turn ended without an open request. A new message continues the run. |
| `FAILED` | A problem of the user, the definition or the agent ended the run. `status_message` has the cause. |
| `ERROR` | A problem of the platform ended the run. `status_message` has the failure code. |
| `CANCELLED` | A user or the foreman cancelled the run. |

The idle timeout is the agent's `idleTimeoutMinutes` (0 to 60). If it is not set, the server default `OF_IDLE_TIMEOUT_DEFAULT` (10 minutes) applies, up to `OF_IDLE_TIMEOUT_MAX` (60 minutes). A message continues the run in the live session, or in a new session that restores the snapshot. See [Follow-ups and idle time](agents.md#follow-ups-and-idle-time).

After a run fails, its sandbox stays for the machine's `failureSessionRetentionMinutes`. Then the server removes it.

## Server does not start

The server validates its configuration at startup and prints all errors together.

| Error | Fix |
|---|---|
| `OF_SECRET_KEY is required (generate one with ...)` | Generate a key with `openssl rand -base64 32` and set `OF_SECRET_KEY`. See [Secret key](self-hosting/configuration.md#secret-key). |
| `OF_SECRET_KEY: secrets: key is not valid base64` or `key must be 32 bytes, got N` | The key is malformed. Generate a new 32-byte key. |
| `OF_DATABASE_URL "...": unsupported scheme (want sqlite:// or postgres://)` | Use one of these schemes. See [Database](self-hosting/configuration.md#database). |
| `OF_PUBLIC_URL "..." must be an absolute http(s) URL` | Set a full URL, such as `https://factories.example.com`. |
| `OF_SANDBOX_BACKEND "...": want kubernetes or local, or the name of a plugin that serves a sandbox backend` | Use `kubernetes` in production. `local` is for development and tests. A plugin name must name a plugin that serves the SandboxBackend service. |
| Helm: `server.replicaCount is N but database.mode sqlite supports exactly one replica` | Set `server.replicaCount: 1`, or use `database.mode: postgres` to run more replicas. |
| `OF_K8S_POD_TEMPLATE: ...` | The pod template file does not exist or is not readable. |
| `config file ...: unknown key "..."` | Remove or correct the key in the YAML file. |

### `acquire migration lock` or a migration error

**Cause:** The database user cannot create tables, or another process holds the migration lock.

**Fix:** Give the database user permission to create tables. Make sure that no other server instance is stuck in a migration.

## Stored secrets cannot be decrypted

### `secrets: ciphertext was encrypted with a different master key`

**Cause:** The server runs with a different `OF_SECRET_KEY` than the key that encrypted the secrets. A common case: a development server created `~/.openfactory/secret.key`, and production runs with another key.

**Fix:** Restore the original key. During a rotation, put the old key in `OF_SECRET_KEY_PREVIOUS`. See [Key rotation](self-hosting/configuration.md#key-rotation). Without the key, there is no recovery: delete the affected secrets and create them again.

A message such as `(key version "1a2b3c4d" is not configured)` means that no configured key has that version. The version is the first 8 hex characters of the SHA-256 of the key.

### `secrets: cannot unwrap data key: keys: cannot unwrap the data key` or `secrets: cannot decrypt value (tampered ciphertext)`

**Cause:** A secret record was changed or copied in the database.

**Fix:** Create the secret again through the API.

## Cannot sign in to the dashboard

| Symptom | Fix |
|---|---|
| `invalid API key` | The key does not exist, or it is revoked or expired. Use an active key of your user. |
| `missing X-OF-CSRF: 1 header` | The sign-in request had neither the header `X-OF-CSRF: 1` nor an `Origin` of the server. Reload `/login`. |
| You are signed out after each restart. | Set a stable `OF_SECRET_KEY`. |
| Sign-in works on HTTP but not behind a proxy. | Set `OF_PUBLIC_URL` to the external `https://` URL. |

## API errors

Every API error is an RFC 7807 problem document with a stable `code`. Read `detail`, and `errors` for definition validation. A `403` with `not_authorized` means that the role is too low (see [Roles and permissions](security.md#roles-and-permissions)). The full list of codes is in [Errors](api.md#errors).

## Worker does not connect

| Symptom | Cause | Fix |
|---|---|---|
| The worker exits with `--api-key (or OF_API_KEY) is required`. | No API key was given. | Pass `--api-key` or set `OF_API_KEY`. |
| The worker exits with `invalid --label "..." (want key=value)`. | A label has no `=`. | Use `--label key=value`. |
| The worker exits with `worker API key rejected by server`. | The key is invalid or revoked, or its owner does not have the team-wide `admin` role. | Create a key of an admin user or service account, and restart the worker. |
| The worker logs `worker connection lost; reconnecting` repeatedly. | The worker cannot reach `--server`, or a proxy blocks WebSocket upgrades. | Allow outbound HTTPS and WebSocket traffic to the control plane. The reverse proxy must forward the `Upgrade` and `Connection` headers for `/api/v1/workers/connect`. |
| The server logs `expected hello` or `unsupported worker protocol`. | A proxy changes the request, or the worker is too old. The server needs worker protocol v2. | Remove request rewriting for the connect path. Upgrade the worker to the release of the server. |
| The worker exits with `worker id "..." is reserved` or `worker ids starting with "openfactory" are reserved`. | The id is reserved. | Choose another id. See [Worker id rules](self-hosting/configuration.md#worker-id-rules). |
| The server logs `worker heartbeat timeout`. | The worker was silent for 45 seconds. | Check the network between worker and server, and the idle timeouts of load balancers. |

## Runs stay queued

A run stays `queued` in these cases:

| Cause | Fix |
|---|---|
| No connected worker has the id of the `workerHost`, or its slots are full. | Start the worker, or raise its `--capacity`. |
| `OF_MAX_SANDBOXES` is reached. | Wait until a session ends, or raise the limit. |
| The factory's `maxConcurrentRuns` is reached. | Wait, or raise it in `factory.yaml`. |
| `queued_reason` is `spend_limit`. | The run starts when the next UTC day or month begins. The queue timeout does not apply. See [Spend limits](security.md#spend-limits). |

After the queue timeout `OF_QUEUE_TIMEOUT` (default 30 minutes), the run goes to `error` with the failure code `infrastructure_timeout`.

To find the cause:

1. Read the agent's effective `workerHost` with `GET /api/v1/factories/{uid}/agents/{name}`.
2. If it names a remote worker, run `openfactory worker list` (or `GET /api/v1/workers`). A worker with exactly that id must be `online`. The match is case-sensitive. A worker started without `--id` uses the host name.
3. If it is `kubernetes`, make sure that the server has that backend configured.
4. Make sure that the worker belongs to the team of the factory.

## `spend_limit_exceeded`

**Cause:** At a turn end, the spend of the factory (`spendLimit` in `factory.yaml`) or of the team (`OF_TEAM_SPEND_LIMIT_DAILY`, `OF_TEAM_SPEND_LIMIT_MONTHLY`) was at or over its limit. The control plane stopped the run tree. The `error` field of each run gives the limit, the spend and the reset time.

**Fix:** Wait for the next period, or raise the limit. Then send a message to the run to continue it. See [Spend limits](security.md#spend-limits).

## No integration record for the owner

The error is `no github integration record for the owner "x"` (or `gitlab`, `azure_devops`). The run fails at the start of its session with the failure code `invalid_configuration` and this error, or a comment or check is not posted. The runner does not retry a configuration error.

**Cause:** The server selects the forge record by the owner of the repository: the record whose `config.org` is the owner. Neither the factory nor the team has a record for that owner. The server never uses a record of a different owner.

**Fix:** Add a record with `"org": "x"` and a credential for that owner, or bind the factory to a record for `x` (`factory_id`). For GitLab, `org` is the top-level group of the project. See [Record selection](integrations/README.md#record-selection).

The error `N records (...) serve the owner "x"` means that old records break the rule of one record for each provider, `org` and factory. Delete the extra records. A create or update that makes such a record gets `409`. A record without `org` or without a credential gets `400`.

## An agent cannot merge a pull request

**Cause:** The control plane refused a merge tool call. See [Agents never merge](security.md#agents-never-merge).

**Fix:** Merge the pull request yourself. To let the agents of one factory merge, set `allowAgentMerge: true` in `factory.yaml`.

## A run is blocked

**Cause:** The last turn ended and a person must answer. `blocked_reason` is `human_request` when the agent called `ask_human` or `request_approval`, and `harness_needs_input` when the harness stopped and asked for input.

**Fix:** In the inbox, answer the question or decide the approval. The answer, or any message to the run, continues it. See [How factories work](how-factories-work.md#6-humans-decide).

## `sandbox_create_failed`

The run is in `error` with the failure code `sandbox_create_failed`. The platform retries this code. At most 3 sessions in sequence can end with a retry code. `status_message.detail` can have a `sandbox_reason` that names the cause.

| Cause | Fix |
|---|---|
| The image pull failed. | See [Image pull failures](#image-pull-failures). |
| The Pod cannot be scheduled. | Check the cluster capacity, node selectors and taints. Run `kubectl describe pod` in the sandbox namespace. |
| The runner did not start before `OF_SANDBOX_START_TIMEOUT` (default 30 minutes). | Find the reason in the Pod events. Raise the timeout only for a slow image pull. |

## `network_policy_timeout`

The run is in `error` with the failure code `network_policy_timeout`. The platform retries this code.

**Meaning:** The init container `egress-wait` of a restricted sandbox Pod ended with exit code 75. A direct connection to the kubelet of the node (port 10250) still worked after the time limit (default 30 seconds), so the sandbox NetworkPolicy was not active, and the sandbox never started. See [The sandbox does not start before the policy is active](self-hosting/network-policy.md#the-sandbox-does-not-start-before-the-policy-is-active).

| Cause | Fix |
|---|---|
| The CNI does not enforce NetworkPolicy now, but the startup check saved `enforced`: the CNI changed or broke after the check, or the check had no answer. | Restart the server to run the check again. Install a CNI that enforces NetworkPolicy. Set `OF_K8S_NETWORK_POLICY_ENFORCEMENT=require` to get a clear error when the check has no answer. |
| The CNI always allows traffic from a Pod to its node, so the kubelet is never blocked. | Set `OF_K8S_EGRESS_WAIT_ADDR` to a `host:port` that a Pod can reach without a policy and that the sandbox policy blocks. Do not use port 80 or 443. |
| The CNI needs more than 30 seconds to apply the policy. | Set `OF_K8S_EGRESS_WAIT_TIMEOUT` to a longer time, for example `90s`. |
| The cluster does not enforce NetworkPolicy, and another control limits sandbox egress. | Set `OF_ALLOW_UNENFORCED_NETWORK_POLICY=true`. The wait is then off, and a restricted sandbox can start without a policy. |

Read the log of the init container: `kubectl -n <sandbox namespace> logs <pod> -c egress-wait`. To turn off only the wait, set `OF_K8S_EGRESS_WAIT_TIMEOUT=off`. The sandbox then starts at once, and the policy takes effect a few hundred milliseconds later.

## `runner_lost`

The run is in `error` with the failure code `runner_lost`: the runner stopped its requests, and the backend has no end reason. The platform retries this code.

**Cause:** The sandbox was killed, the node was lost, or the network between the sandbox and the control plane failed. On a remote worker, the worker was disconnected for about three minutes.

**Fix:**

1. Check that the sandbox can reach `OF_INTERNAL_URL`. A sandbox on a remote worker needs `OF_PUBLIC_URL`.
2. Read the Pod events and logs. See [Kubernetes sandbox pods](#kubernetes-sandbox-pods).
3. For a remote worker, check that the worker reconnects. See [Worker does not connect](#worker-does-not-connect).

## Sandbox setup fails

A setup error ends the run in `FAILED` with the failure code `environment_setup_failed`. The run error starts with the code, names the failed step, and has the end of its output (at most 4000 bytes, secrets replaced by `***`):

```text
environment_setup_failed: step command:2 failed: exit status 3
npm ERR! missing script: build
```

`openfactory run get <run-id>` prints this text in the `Error` line. The dashboard shows it on the run page. The API has it in `error` and `status_message.message`; `failure_code` has only the code. The last 16 KiB of the step output are in the `setup.step` event with `"status": "failed"` of the run timeline (`GET /api/v1/runs/{runId}/timeline`).

| Step name | Cause | Fix |
|---|---|---|
| `clone:<path>` | Git cannot clone: wrong forge credentials, the network blocks the forge, or the image has no `git`. | Check the forge integration. Run `git clone` from the same host or container. Use an image with `git`, for example the default `ghcr.io/openfactory-org/sandbox:latest`. |
| `command:N` | The Nth setup command failed. Machine commands run first, then environment commands. | Run the command in the sandbox image. |
| `step ...: setup timed out` | The setup ran longer than the setup timeout (default 30 minutes). | Make the commands faster, or move work into the image. |

To keep the sandbox of a failed run for inspection, set the machine's `failureSessionRetentionMinutes`.

## Harness errors

When a turn fails, the run error has the failure code, then the text that the harness reported.

| Error | Cause | Fix |
|---|---|---|
| `harness "...": not found` | The server does not have this harness. | Check the agent's `harness.type` (`claude` or `codex`). |
| `start <command>: ...` with `not found` | The `claude` or `codex` command is not in the sandbox. | Add the CLI to the sandbox image, or install it in a setup command. |
| `harness exited without completing the turn` | The CLI stopped early: missing credentials, a crash, or a killed container. | Check the harness authentication (see [Harnesses](harnesses.md)). Read the transcript for the CLI output. |
| The foreman cannot call `run_agents` or `ask_human`. | The sandbox cannot reach the control plane, the session ended (`401`), or a newer session exists (`409`). | Set `OF_INTERNAL_URL` (or `OF_PUBLIC_URL`) to a URL that sandboxes can reach, not `localhost`. On Kubernetes, use the in-cluster Service URL or the ingress host. |

## Image pull failures

The sandbox ends with `image_pull` (`ImagePullBackOff` or `InvalidImageName`): the node cannot pull the image.

- Check the image name and tag.
- For a private image, add `imagePullSecrets` to the pod template, and create the secret in the sandbox namespace. The Kubernetes backend ignores the machine's `registryCredentialSecretName`.
- If nodes cannot reach `ghcr.io`, mirror the default image and set `OF_SANDBOX_IMAGE` (Helm: `sandbox.image`).

See [Private registries](self-hosting/private-registries.md).

## Kubernetes sandbox pods

```bash
kubectl -n <sandbox namespace> get pods -l app.kubernetes.io/managed-by=openfactory
kubectl -n <sandbox namespace> describe pod <pod>
```

| Error | Cause | Fix |
|---|---|---|
| `unschedulable` (a sandbox end reason) | No node satisfies the Pod's requests, selectors or tolerations. | Use a smaller `instanceShape`, change the pod template, or add nodes. |
| `CreateContainerConfigError` | A Secret, ConfigMap or service account of the pod template is missing. | Create it in the sandbox namespace. |
| `container_oom` (OOMKilled, exit 137) | The container used more memory than its limit. | Raise `memoryGb` in the machine's instance shape. |
| The Pod ends at once, and its log says that `openfactory` is not found. | The runner image has no binary at `/usr/local/bin/openfactory`, or the sandbox image is not Linux. | Set `OF_RUNNER_IMAGE` (Helm: `runner.image`) to the server image of your release. See [Runner images](self-hosting/kubernetes.md#runner-images). |
| `network_policy_timeout` | The sandbox NetworkPolicy did not block the kubelet in time. | See [`network_policy_timeout`](#network_policy_timeout). |
| `kubernetes API: HTTP 403: ...` | The service account lacks permissions. | Apply the Role in [Kubernetes RBAC](self-hosting/kubernetes.md#rbac). |
| `kubernetes: user "..." uses an exec/auth-provider plugin, which is not supported` | The kubeconfig uses a cloud login plugin. | Use a service account token or a client certificate. |

## Network access is blocked

**Symptom:** A command in the sandbox gets HTTP `403` with `Open Factory network policy blocked access to <host>`. The run has the diagnostic `network/egress_blocked` with `data.blocked_host` and `data.count` (`GET /api/v1/runs/{runId}/diagnostics`).

**Cause:** The run has a restricted network policy, and the host is not allowed.

**Fix:**

1. Add the host to `allowedHosts`, or extend a built-in policy that contains it.
2. Remove the host from `deniedHosts` at factory and agent scope. A denial always wins.
3. List each subdomain. There is no wildcard matching.

See [Network policy](self-hosting/network-policy.md).

**Symptom:** A restricted run on the `local` backend can reach every host.

**Cause:** The `local` backend does not enforce network policies. Use the `kubernetes` backend. It creates a NetworkPolicy and an egress proxy sidecar for each restricted sandbox.

**Symptom:** A restricted run on the `kubernetes` backend cannot reach any host, not even an allowed one. The sandbox has no `HTTPS_PROXY` variable.

**Cause:** `OF_K8S_EGRESS_PROXY_SIDECAR_IMAGE` is empty, so the Pod has no proxy sidecar, and the NetworkPolicy allows only DNS and the control plane. Set the variable to the runner image. The Helm chart does this by default (`sandbox.k8s.egressProxy.sidecarImage`).

## MCP server errors

| Message in the run error or log | Cause and fix |
|---|---|
| `no MCP server "<name>" is registered for this team` | The agent's `managedId` names no registry entry. Register the server, or fix the name. See [MCP servers](mcp-servers.md). |
| `MCP server "<name>" is not connected` | The server uses OAuth, and no editor connected it, or the connection was reset. Open the server in the dashboard and select **Connect**. |
| `... needs the server's public URL (OF_PUBLIC_URL)` | The control plane serves proxied, OAuth and built-in servers. Set `OF_PUBLIC_URL`. |
| The server page shows **OAuth: error** | The token refresh failed, or the server rejected the token. The error text gives the reason. Select **Reconnect**. |
| `the factory does not attach the <provider> integration` | A built-in server needs the integration in `integrations` of `factory.yaml`. |
| `<provider>: 2 records (...) can serve this source` | The factory, or else the team, has more than one record for the provider, and the work item does not name its workspace. Keep one record, or create a record for the factory (`factory_id`). See [Record selection](integrations/README.md#record-selection). |
| `no <provider> integration record for the <workspace> "<id>"` | Add a record with that workspace key (`team_id`, `organization_id` or `site`). |
| A built-in write tool is missing | Add `allowWrite: true` to the agent's `mcpServers` entry. |

## Related pages

- [Monitoring](monitoring.md)
- [Self-hosting overview](self-hosting/README.md)
- [Security](security.md)
