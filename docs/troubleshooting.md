# Troubleshooting

Each section starts with the symptom or the exact error text.

## First steps

1. Check that the control plane is ready: `curl -fsS ${OF_PUBLIC_URL}/readyz`. A `503` response names the cause.
2. Set `OF_LOG_LEVEL=debug`, deploy, and reproduce the problem. Stream the logs with `npx wrangler tail -c apps/control-plane/wrangler.jsonc`.
3. Read the run transcript in the dashboard, or with `GET /api/v1/runs/{runId}/messages`. A failed run has an `error` field with the failure code and the cause. The requester gets the inbox item `Run failed: <work item title>`.
4. Check the inbox for items of kind `failure`.

## Run states

The API shows run states in upper case.

| API state | Meaning |
|---|---|
| `QUEUED` | The run waits for capacity, or for a worker that serves its `workerHost`. |
| `PENDING` | A provider prepares the sandbox. |
| `CLAIMED` | The sandbox exists and the runner does its setup. |
| `RUNNING` | A turn is active. |
| `BLOCKED` | The last turn ended with an open request to a person, or the harness asked for input. `blocked_reason` tells which. |
| `SUCCEEDED` | The last turn ended without an open request. A new message continues the run. |
| `FAILED` | A problem of the user, the definition or the agent ended the run. `status_message` has the cause. |
| `ERROR` | A problem of the platform ended the run. `status_message` has the failure code. |
| `CANCELLED` | A user or the foreman cancelled the run. |

The idle timeout is the agent's `idleTimeoutMinutes` (0 to 60). If it is not set, the team default (10 minutes) applies, up to 60 minutes. A message continues the run in the live session, or in a new session that restores the snapshot. See [Follow-ups and idle time](agents.md#follow-ups-and-idle-time).

After a run fails, its sandbox stays for the machine's `failureSessionRetentionMinutes`. Then the server removes it.

## The control plane does not start

The control plane validates its configuration on the first request. A configuration error returns `internal_error` with the text `server configuration error`, and the log names the cause. Read it with `wrangler tail`.

| Error in the log | Fix |
|---|---|
| `OF_SECRET_KEY is not set` | Set the secret: `openssl rand -base64 32 \| npx wrangler secret put OF_SECRET_KEY`. See [Secret key](self-hosting/configuration.md#secret-key). |
| `OF_SECRET_KEY: key is not valid base64` or `key must be 32 bytes, got N` | The key is malformed. Generate a new 32-byte key. |
| `OF_PUBLIC_URL ... must be an absolute http(s) URL` | Set a full URL, such as `https://factory.example.com`. |
| `OF_OIDC_CLIENT_ID is required` | An OIDC issuer is set without a client ID. See [OIDC login](self-hosting/oidc.md). |
| The deploy fails with a missing service binding | A Worker named in the `services` block does not exist. Deploy the provider or plugin Worker first, or remove its binding. See [Deploy to Cloudflare](self-hosting/cloudflare.md). |

## Stored secrets cannot be decrypted

### `secrets: ciphertext was encrypted with a different master key`

**Cause:** The control plane runs with a different `OF_SECRET_KEY` than the key that encrypted the secrets. A common case: a development deployment and a production deployment use different keys.

**Fix:** Restore the original key. During a rotation, put the old key in `OF_SECRET_KEY_PREVIOUS`. See [Key rotation](self-hosting/configuration.md#key-rotation). Without the key, there is no recovery: delete the affected secrets and create them again.

A message such as `(key version "1a2b3c4d" is not configured)` means that no configured key has that version. The version is the first 8 hex characters of the SHA-256 of the key.

### `secrets: cannot unwrap data key: keys: cannot unwrap the data key` or `secrets: cannot decrypt value (tampered ciphertext)`

**Cause:** A secret record was changed or copied.

**Fix:** Create the secret again through the API.

## Cannot sign in to the dashboard

| Symptom | Fix |
|---|---|
| `invalid API key` | The key does not exist, or it is revoked or expired. Use an active key of your user. |
| `missing X-OF-CSRF: 1 header` | The sign-in request had neither the header `X-OF-CSRF: 1` nor an `Origin` of the server. Reload `/login`. |
| You are signed out after each deploy. | Keep `OF_SIGNING_KEY` (or `OF_SECRET_KEY`) the same. |
| Sign-in works on HTTP but not behind a proxy. | Set `OF_PUBLIC_URL` to the external `https://` URL. |

## API errors

Every API error is an RFC 7807 problem document with a stable `code`. Read `detail`, and `errors` for definition validation. A `403` with `not_authorized` means that the role is too low (see [Roles and permissions](security.md#roles-and-permissions)). The full list of codes is in [Errors](api.md#errors).

## Worker does not connect

| Symptom | Cause | Fix |
|---|---|---|
| A new worker stays `pending`. | The service did not start, or the worker cannot reach the control plane. | Run `sudo openfactory-runtime worker status` and `sudo openfactory-runtime worker doctor` on the host. Read `journalctl -u openfactory-worker`. |
| `enroll` fails with a token error. | The token expired (1 hour) or was used. | Create a new enrollment token. |
| `enroll` fails because of KVM. | The host has no usable `/dev/kvm`. | Use a host with KVM. `doctor` prints the cause. |
| The worker logs `auth_failed` or the control plane refuses the signature. | The worker was revoked, or its key does not match. | Revoke the worker record and enroll the host again. |
| A development worker exits with `--api-key (or OF_API_KEY) or --key-file (or OF_WORKER_KEY_FILE) is required`. | No credential was given. | Pass `--api-key` or `--key-file`. |
| A development worker exits with `invalid --label "..." (want key=value)`. | A label has no `=`. | Use `--label key=value`. |
| A development worker exits with `worker API key rejected by server`. | The key is invalid or revoked, or its owner does not have the team-wide `admin` role. | Create a key of an admin user or service account, and restart the worker. |
| A development worker is refused. | `OF_DEV_PROCESS_WORKERS` is not `true` on the control plane. | Set it for development. Never set it in production. |
| The worker logs `worker connection lost; reconnecting` repeatedly. | The worker cannot reach the control plane, or a proxy blocks WebSocket upgrades. | Allow outbound HTTPS and WebSocket traffic to the control plane. |
| The control plane logs `unsupported worker protocol`. | The worker is too old. The control plane needs worker protocol v2. | Update the runtime on the worker to the release of the control plane. |
| The worker exits with `worker id "..." is reserved`. | The ID is reserved. | Choose another ID. See [Worker ID rules](self-hosting/configuration.md#development-process-worker). |
| The control plane logs `worker heartbeat timeout`. | The worker was silent too long. | Check the network between the worker and the control plane. |
| Traffic from VMs fails on a host with Docker, ufw or firewalld. | A host firewall drops VM traffic. | Run `worker doctor` and add the rule that it prints. |

## Runs stay queued

A run stays `queued` in these cases:

| Cause | Fix |
|---|---|
| No online worker has the ID of the `workerHost`, or its slots are full. The run shows the reason `waiting_for_worker`. | Start the worker, raise its slots, or drain less. |
| The team limit of open sessions is reached. | Wait until a session ends, or raise the limit. |
| The factory's `maxConcurrentRuns` is reached. | Wait, or raise it in `factory.yaml`. |
| `queued_reason` is `spend_limit`. | The run starts when the next UTC day or month begins. The queue timeout does not apply. See [Spend limits](security.md#spend-limits). |

After the queue timeout `OF_QUEUE_TIMEOUT` (default 30 minutes), the run goes to `error` with the failure code `infrastructure_timeout`.

To find the cause:

1. Read the agent's effective `workerHost` with `GET /api/v1/factories/{uid}/agents/{name}`.
2. If it names a worker, run `openfactory worker list` (or `GET /api/v1/workers`). A worker with exactly that ID or name must be `online` and not `draining`. The match is case-sensitive. For `pool:<label>`, a worker with that label must be online and allow the factory.
3. If it is a provider ID (`cloudflare-sandbox`, `daytona`, `boatdev`), check that the provider is configured. A provider that hits its start rate limit queues runs.
4. Make sure that the worker belongs to the team of the factory.

## `spend_limit_exceeded`

**Cause:** At a turn end, the spend of the factory (`spendLimit` in `factory.yaml`) or of the team (the team spend limits) was at or over its limit. The control plane stopped the run tree. The `error` field of each run gives the limit, the spend and the reset time.

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
| The provider refused the sandbox, for example for its size or its rate limit. | Use a smaller `instanceShape`, or wait. See [Sandbox providers](sandbox-providers.md#limits-and-notes). |
| The runner did not start before `OF_SANDBOX_START_TIMEOUT` (default 10 minutes). | Read the provider logs. Raise the timeout only for a slow image pull. |

## `invalid_configuration` at start

The run is in `error` with the failure code `invalid_configuration` before a sandbox exists. The message names the cause:

| Cause | Fix |
|---|---|
| The target has `egress: none` and the run has a restricted network policy. | Use a provider that enforces policies, or set `OF_ALLOW_UNENFORCED_NETWORK_POLICY=true`. See [Network policy](self-hosting/network-policy.md#the-capability-gate). |
| The machine asks for more CPU or memory than the largest instance type. | Use a smaller `instanceShape`. See [Machine sizes](sandbox-providers.md#machine-sizes). |
| The provider cannot run the architecture or the Daytona tier is below 3. | Use another provider. See [Sandbox providers](sandbox-providers.md). |

## `runner_lost`

The run is in `error` with the failure code `runner_lost`: the runner stopped its requests, and the provider has no end reason. The platform retries this code.

**Cause:** The sandbox was killed, the provider lost it, or the network between the sandbox and the control plane failed. On a worker, the worker was disconnected for about three minutes.

**Fix:**

1. Check that the sandbox can reach `OF_PUBLIC_URL`.
2. Read the provider logs. For `cloudflare-sandbox`, run `npx wrangler tail -c providers/cloudflare-sandbox/wrangler.jsonc`.
3. For a worker, check that the worker reconnects. See [Worker does not connect](#worker-does-not-connect).

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
| The foreman cannot call `run_agents` or `ask_human`. | The sandbox cannot reach the control plane, the session ended (`401`), or a newer session exists (`409`). | Set `OF_PUBLIC_URL` to a URL that sandboxes can reach, not `localhost`. The sandbox reaches the control plane at `OF_PUBLIC_URL`. |

## Image pull failures

The sandbox ends with `image_pull` or `sandbox_create_failed`: the provider or host cannot pull the image.

- Check the image name and tag.
- On a microVM worker, a private image needs `registryCredentialSecretName` in the machine. See [Private registries](self-hosting/private-registries.md).
- On Daytona and boat.dev, rebuild the snapshot. A Daytona snapshot goes inactive after 2 weeks without use, and preflight builds it again.
- If a host cannot reach `ghcr.io`, mirror the sandbox image.

## Sandbox provider errors

| Error | Cause | Fix |
|---|---|---|
| `daytona: tier 1 and 2 ...` at preflight | The Daytona organization is on tier 1 or 2. | Use tier 3 or higher. |
| `boatdev: the plan does not allow machine type ...` | The boat.dev plan has no such machine type. | Use a smaller machine or a larger plan. |
| Runs wait without a sandbox on `boatdev` | The start limits (12 a minute, 60 an hour, 200 a day) are used up. | Wait, or reduce the number of runs. |
| `sandbox_lost` on `cloudflare-sandbox` | The container host restarted, or the container slept too long. | The platform retries. See [Sandbox providers](sandbox-providers.md#cloudflare-sandbox). |
| `out_of_memory` | The sandbox used more memory than its limit. | Raise `memoryGb` in the instance shape of the machine. |
| Session start is refused because of the runner version | `OF_RUNNER_VERSION` differs from the runtime in the sandbox image. | Publish the runtime that matches `OF_RUNNER_VERSION`. |

## Network access is blocked

**Symptom:** A command in the sandbox gets HTTP `403` with `Open Factory network policy blocked access to <host>`. The run has the diagnostic `network/egress_blocked` with `data.blocked_host` and `data.count` (`GET /api/v1/runs/{runId}/diagnostics`).

**Cause:** The run has a restricted network policy, and the host is not allowed.

**Fix:**

1. Add the host to `allowedHosts`, or extend a built-in policy that contains it.
2. Remove the host from `deniedHosts` at factory and agent scope. A denial always wins.
3. List each subdomain. There is no wildcard matching.

See [Network policy](self-hosting/network-policy.md).

**Symptom:** A restricted run on a development process worker can reach every host.

**Cause:** The development process worker has `egress: none` and enforces nothing. Use a provider or a microVM worker that enforces policies.

**Symptom:** A restricted run fails before the agent starts, with an error from the in-sandbox lock.

**Cause:** A probe of the lock reached the network, so the runner failed closed. Check that the sandbox image is the current one and that the provider lets root run `iptables`. See [In-sandbox lock](self-hosting/network-policy.md#in-sandbox-lock).

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
