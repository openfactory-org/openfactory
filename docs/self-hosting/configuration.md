# Configuration

Settings of `openfactory serve` and flags of `openfactory worker`.

## Configuration sources

Each server setting has an `OF_` environment variable and a YAML key, but no flag. The only flag of `openfactory serve` is `--config`. A later source overrides an earlier one:

1. Built-in defaults.
2. The YAML file named by `--config` or `OF_CONFIG`.
3. `OF_*` environment variables.

An unknown YAML key is an error. The server validates the result and stops with a list of all errors. See [Server does not start](../troubleshooting.md#server-does-not-start).

```yaml
# /etc/openfactory/config.yaml
listen: ":8080"
public_url: https://factories.example.com
database_url: postgres://openfactory:secret@db.internal:5432/openfactory?sslmode=require
sandbox_backend: kubernetes
object_store_url: s3://acme-factory-runs/prod?region=eu-west-1
log_format: json
```

```bash
OF_SECRET_KEY="$(cat /etc/openfactory/secret.key)" openfactory serve --config /etc/openfactory/config.yaml
```

Keep `secret_key` and API keys out of the YAML file. Pass them as environment variables from a secret manager.

## Server settings

### Core

| Variable | YAML key | Default | Description |
|---|---|---|---|
| `OF_LISTEN` | `listen` | `:8080` | HTTP listen address for the API, dashboard, MCP endpoints, webhooks, worker connections, `/healthz`, `/readyz` and `/metrics`. |
| `OF_PUBLIC_URL` | `public_url` | `http://localhost:8080` | External base URL (absolute `http` or `https`). Used for webhook URLs, OAuth redirects and links. Sandboxes use it unless `OF_INTERNAL_URL` is set. |
| `OF_INTERNAL_URL` | `internal_url` | `OF_PUBLIC_URL` | Base URL (absolute `http` or `https`) that sandboxes use to reach the server: the orchestration MCP URL (`${OF_INTERNAL_URL}/mcp/run/{runId}`), the MCP proxy, the built-in integration servers, the runner control URL and the `OF_PUBLIC_URL` variable of the run. Remote workers do not use it; they keep `OF_PUBLIC_URL`. |
| `OF_DATABASE_URL` | `database_url` | `sqlite://<data dir>/openfactory.db` | See [Database](#database). |
| `OF_SECRET_KEY` | `secret_key` | none | Base64-encoded 32-byte key. It encrypts secrets and, without `OF_SIGNING_KEY`, is the base of the signing keys. Required unless `OF_DEV=true`. See [Secret key](#secret-key). |
| `OF_SECRET_KEY_PREVIOUS` | `secret_key_previous` | none | Comma-separated old secret keys, read only. See [Key rotation](#key-rotation). |
| `OF_SIGNING_KEY` | `signing_key` | derived from `OF_SECRET_KEY` | Base64-encoded 32-byte key for sessions, tokens and links. Set it to rotate signing and secret keys separately. |
| `OF_SIGNING_KEY_PREVIOUS` | `signing_key_previous` | none | Comma-separated old signing keys, verify only. Needs `OF_SIGNING_KEY`. |
| `OF_DATA_DIR` | `data_dir` | `$HOME/.openfactory` | Local state: the default SQLite file, the default object store (`objects`), plugins, development sandboxes and the development key. |
| `OF_DEV` | `dev` | `false` | See [Development mode](#development-mode). |
| `OF_REPLICA_ID` | `replica_id` | host name | Replica identity in leases. The Helm chart sets the pod name. |
| `OF_LOG_LEVEL` | `log_level` | `info` | `debug`, `info`, `warn` or `error`. |
| `OF_LOG_FORMAT` | `log_format` | `text` | `text` or `json`. Logs go to standard error. |
| `OF_CONFIG` | | none | Path of the YAML file. `--config` sets it too. |

### Sandbox backend

`OF_K8S_*` values are settings, not pod template fields, because they set Job and Pod metadata, the workspace volume or the containers that the backend adds. A remote worker with the `kubernetes` backend reads the same settings; the Helm chart gives it the same `sandbox.k8s` values as the server.

| Variable | YAML key | Default | Description |
|---|---|---|---|
| `OF_SANDBOX_BACKEND` | `sandbox_backend` | `kubernetes`; `local` when `OF_DEV=true` | `kubernetes` (production), `local` (development and tests only), or the name of a plugin that serves the SandboxBackend version 2 service ([plugins](plugins.md#sandbox-backend-plugins)). Outside dev mode, `local` must be set explicitly and the server logs a warning. |
| `OF_SANDBOX_ROOT` | `sandbox_root` | `<data dir>/sandboxes` | `local` backend: workspace directory. |
| `OF_SANDBOX_IMAGE` | `sandbox_image` | `ghcr.io/openfactory-org/sandbox:latest` | Sandbox image when neither the environment nor the machine sets one. |
| `OF_K8S_NAMESPACE` | `k8s_namespace` | kubeconfig or service-account namespace, else `default` | Namespace of sandbox Jobs and Pods. |
| `OF_K8S_POD_TEMPLATE` | `k8s_pod_template` | none | Path of a pod template file (YAML or JSON). The file must exist. See [Pod template](kubernetes.md#pod-template). |
| `OF_KUBECONFIG` | `kubeconfig` | in-cluster, then `$KUBECONFIG`, then `~/.kube/config` | Explicit kubeconfig path. |
| `OF_K8S_LABELS` | `k8s_labels` | none | Labels of sandbox Jobs and Pods, as comma-separated `key=value`. |
| `OF_K8S_ANNOTATIONS` | `k8s_annotations` | none | Annotations of sandbox Jobs and Pods, as comma-separated `key=value`. See [Protect sandbox Pods from disruption](kubernetes.md#protect-sandbox-pods-from-disruption). |
| `OF_K8S_IMAGE_PULL_POLICY` | `k8s_image_pull_policy` | `IfNotPresent` | `Always`, `IfNotPresent` or `Never`. Also applies to the containers that the backend adds. |
| `OF_K8S_RUN_AS_USER` | `k8s_run_as_user` | `1000` | Numeric user of sandbox Pods; the group and `fsGroup` get the same number. `image` keeps the user of the image. See [Sandbox user](kubernetes.md#sandbox-user). |
| `OF_K8S_WORKSPACE_SIZE_LIMIT` | `k8s_workspace_size_limit` | none | Size limit of the workspace volume, for example `20Gi`. |
| `OF_K8S_PREFLIGHT_JOB` | `k8s_preflight_job` | `false` | Run a test Job at startup. The server does not start if it fails. See [Preflight](kubernetes.md#preflight). |
| `OF_K8S_CONTROL_PLANE_NAMESPACE` | `k8s_control_plane_namespace` | none | Namespace of the server Pods that a sandbox with a restricted network policy can reach. Needs `OF_K8S_CONTROL_PLANE_SELECTOR`. |
| `OF_K8S_CONTROL_PLANE_SELECTOR` | `k8s_control_plane_selector` | none | Label selector of those server Pods, for example `app.kubernetes.io/name=openfactory`. Used in the NetworkPolicy of a restricted sandbox. |
| `OF_K8S_EGRESS_PROXY_SIDECAR_IMAGE` | `k8s_egress_proxy_sidecar_image` | none (the chart sets the runner image) | Image with the `openfactory` binary. A restricted sandbox gets a proxy sidecar with its own host list. Without it, a restricted sandbox reaches only DNS and the control plane. See [Proxy sidecar](network-policy.md#proxy-sidecar-for-each-sandbox). |
| `OF_ALLOW_UNENFORCED_NETWORK_POLICY` | `allow_unenforced_network_policy` | `false` | Start a restricted sandbox even if its NetworkPolicy cannot be created, or if `OF_K8S_NETWORK_POLICY_ENFORCEMENT=require` finds no enforcement. Without it, the run ends with `invalid_configuration`. See [Network policy](network-policy.md#kubernetes). |
| `OF_K8S_EGRESS_LOCK` | `k8s_egress_lock` | `iptables` when `OF_K8S_EGRESS_PROXY_SIDECAR_IMAGE` is set and `OF_K8S_RUN_AS_USER` is numeric, else `none` | `iptables` adds an init container with `NET_ADMIN` to each restricted sandbox Pod. It blocks direct connections to ports 80 and 443 for every process but the proxy sidecar. Needs `OF_K8S_EGRESS_PROXY_SIDECAR_IMAGE`, a numeric `OF_K8S_RUN_AS_USER`, and a sandbox namespace that allows `NET_ADMIN` (Pod Security level `privileged`, or an exception). `none` turns it off. See [Egress lock](network-policy.md#egress-lock). |
| `OF_K8S_NETWORK_POLICY_CHECK` | `k8s_network_policy_check` | `probe` | `probe` checks at startup, with test Pods, that the cluster enforces NetworkPolicy. `off` skips the check, for clusters that do not allow probe Pods. |
| `OF_K8S_NETWORK_POLICY_ENFORCEMENT` | `k8s_network_policy_enforcement` | `warn` | `require` fails a restricted run unless the probe found enforcement. Needs `OF_K8S_NETWORK_POLICY_CHECK=probe`. |
| `OF_K8S_EGRESS_WAIT_TIMEOUT` | `k8s_egress_wait_timeout` | `30s` | How long a restricted sandbox waits for its NetworkPolicy to become active. At least `1s`; `off` disables the wait. See [the policy wait](network-policy.md#the-sandbox-does-not-start-before-the-policy-is-active). |
| `OF_K8S_EGRESS_WAIT_ADDR` | `k8s_egress_wait_addr` | the node's kubelet, port `10250` | `host:port` that the wait dials. It must accept connections without a policy, and the sandbox policy must block it. |

### Runner and sessions

The control plane assigns one sandbox per session. The runner (`openfactory runner`) runs in the sandbox, runs the harness and talks to the control plane under `/runner/v1`. It works with the `local` backend (a host process), the `kubernetes` backend (one Job per sandbox), remote workers on protocol version 2, and plugins that serve the TaskBackend version 2 service.

| Variable | YAML key | Default | Description |
|---|---|---|---|
| `OF_RUNNER_COMMAND` | `runner_command` | this binary with the argument `runner` | `local` backend only: command that starts the runner, split on spaces. |
| `OF_SNAPSHOT_MAX_BYTES` | `snapshot_max_bytes` | `2147483648` (2 GiB) | Largest snapshot part, more than 0. The control plane refuses a larger upload; the runner reports a diagnostic event. |
| `OF_RUNNER_IMAGE` | `runner_image` | `ghcr.io/openfactory-org/openfactory:<release>` (`latest` for a build from source) | Image with the runner at `/usr/local/bin/openfactory`. The `kubernetes` backend and plugin backends copy it into each sandbox. Set it if you mirror the images. |
| `OF_CLAUDE_TOOL_IMAGE` | `claude_tool_image` | derived from `OF_RUNNER_IMAGE` | Image with the `claude` CLI at `/opt/openfactory/tool`, copied into each `claude` sandbox. The default replaces `openfactory` with `tool-claude` in the runner image name and keeps the tag. This works only if the runner image repository is named `openfactory` and has a tag. See [Tool images](kubernetes.md#tool-images). |
| `OF_CODEX_TOOL_IMAGE` | `codex_tool_image` | derived from `OF_RUNNER_IMAGE` | As `OF_CLAUDE_TOOL_IMAGE`, for `codex` (`tool-codex`). |
| `OF_RUNNER_EXEC_COMMAND` | `runner_exec_command` | none | Development and tests only. Each run uses the harness type `exec`, and the runner runs this command with `sh -c`. |
| `OF_QUEUE_TIMEOUT` | `queue_timeout` | `30m` | Longest time in `queued`. Then the run goes to `error` with `infrastructure_timeout`. More than 0. |
| `OF_SANDBOX_START_TIMEOUT` | `sandbox_start_timeout` | `30m` | Longest time from sandbox assignment to session start. Then the session ends with `sandbox_create_failed`. More than 0. |
| `OF_MAX_SESSION_DURATION` | `max_session_duration` | `8h` | Longest session life. Then the control plane sends `session.stop` with reason `deadline`; an active turn goes to `error` with `infrastructure_timeout`. More than 0. |
| `OF_SESSION_STOP_GRACE` | `session_stop_grace` | `2m` | Time for the runner's snapshot after a stop or cancel, before the backend stops the sandbox. Not negative. |
| `OF_MAX_SANDBOXES` | `max_sandboxes` | `0` (no limit) | Most open sessions. A queued run waits at the limit. Two replicas that claim at the same moment can pass it slightly. Not negative. |
| `OF_IDLE_TIMEOUT_DEFAULT` | `idle_timeout_default` | `10m` | Idle timeout of an agent without `idleTimeoutMinutes`. `0` ends the session right after a turn. At most `OF_IDLE_TIMEOUT_MAX`. |
| `OF_IDLE_TIMEOUT_MAX` | `idle_timeout_max` | `60m` | Upper limit of the idle timeout; larger agent values are reduced to it. At most `60m`. |
| `OF_MAX_ORCHESTRATION_DEPTH` | `max_orchestration_depth` | `1` | Depth limit of a run tree. A root run has depth 0; a run at depth `d` can start child runs with `run_agents` only if `d` is less than this value. `0` means the default. Not negative. `spawnableBy` does not change this limit. |

There is no per-replica turn limit. The limits are the worker `--capacity`, `OF_MAX_SANDBOXES`, `maxConcurrentRuns` in `factory.yaml`, `OF_QUEUE_TIMEOUT` and `OF_MAX_SESSION_DURATION`.

### Spend limits

The team limit applies to each team. A factory can set its own with `spendLimit` in `factory.yaml`. See [Spend limits](../security.md#spend-limits).

| Variable | YAML key | Default | Description |
|---|---|---|---|
| `OF_TEAM_SPEND_LIMIT_DAILY` | `team_spend_limit_daily` | none | Model spend limit per team per UTC day, in US dollars, for example `12.5`. Empty or `0` means no limit. Not negative. |
| `OF_TEAM_SPEND_LIMIT_MONTHLY` | `team_spend_limit_monthly` | none | The same, per UTC month. |

### Object storage

Transcripts, artifacts and prompt attachments go to object storage; the database keeps metadata. See [Object storage](object-storage.md).

| Variable | YAML key | Default | Description |
|---|---|---|---|
| `OF_OBJECT_STORE_URL` | `object_store_url` | `file://<data dir>/objects` | `s3://bucket/prefix?region=...`, `gs://bucket/prefix`, `azblob://account/container/prefix` or `file:///path`. |
| `OF_OBJECT_STORE_TRANSCRIPTS_URL`, `OF_OBJECT_STORE_ARTIFACTS_URL`, `OF_OBJECT_STORE_ATTACHMENTS_URL` | `object_store_transcripts_url`, `object_store_artifacts_url`, `object_store_attachments_url` | `OF_OBJECT_STORE_URL` | Optional store per category. |
| `OF_OBJECT_STORE_SNAPSHOTS_URL` | `object_store_snapshots_url` | `OF_OBJECT_STORE_URL` | Optional store for session snapshots. Snapshots can contain every workspace file, including files with secrets. Restrict access. |
| `OF_OBJECT_STORE_RETENTION` | `object_store_retention` | keep forever | Delete run data and attachments older than this Go duration, for example `720h`. |
| `OF_OBJECT_STORE_RETENTION_INTERVAL` | `object_store_retention_interval` | `1h` | How often the leader runs retention. At least `1m`. |
| `OF_OBJECT_STORE_PRESIGN_TTL` | `object_store_presign_ttl` | `15m` | Lifetime of presigned run-data URLs. |
| `OF_OBJECT_STORE_SIGNING_KEY` | `object_store_signing_key` | none | HMAC key for `file://` presigned links. Use the same value on every replica. |

Provider credentials use the standard variables: `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN`, `AWS_REGION`; `GOOGLE_APPLICATION_CREDENTIALS`; `AZURE_STORAGE_KEY`.

### Cloud identity issuer

| Variable | YAML key | Default | Description |
|---|---|---|---|
| `OF_OIDC_ISSUER_URL` | `oidc_issuer_url` | `OF_PUBLIC_URL` | Issuer (`iss`) of the [cloud identity](../definition-reference.md#cloudproviders) tokens that the server signs. Absolute `http` or `https`. The cloud provider reads `<issuer>/.well-known/openid-configuration` and `<issuer>/.well-known/jwks.json` without authentication, so both must be reachable from the internet over HTTPS. Not the same as `OF_OIDC_ISSUER` (sign-in). |

### OIDC login

Dashboard single sign-on. `OF_OIDC_ISSUER` enables it. See [OIDC login](oidc.md).

| Variable | YAML key | Default | Description |
|---|---|---|---|
| `OF_OIDC_ISSUER` | `oidc_issuer` | none | Issuer URL. |
| `OF_OIDC_CLIENT_ID` | `oidc_client_id` | none | Required with an issuer. |
| `OF_OIDC_CLIENT_SECRET` | `oidc_client_secret` | none | Optional for PKCE public clients. |
| `OF_OIDC_REDIRECT_URL` | `oidc_redirect_url` | `${OF_PUBLIC_URL}/auth/oidc/callback` | Redirect URL registered with the identity provider. |
| `OF_OIDC_SCOPES` | `oidc_scopes` | `openid,profile,email` | `openid` is always added. |
| `OF_OIDC_ALLOWED_DOMAINS` | `oidc_allowed_domains` | any | Comma-separated email domains that may sign in. |
| `OF_OIDC_GROUPS_CLAIM` | `oidc_groups_claim` | `groups` | ID token claim with group names. |
| `OF_OIDC_ADMIN_GROUPS`, `OF_OIDC_EDITOR_GROUPS` | `oidc_admin_groups`, `oidc_editor_groups` | none | Comma-separated groups mapped to admin and editor. |
| `OF_OIDC_DEFAULT_ROLE` | `oidc_default_role` | `user` | Role for users in no mapped group: `admin`, `editor` or `user`. |
| `OF_OIDC_DISPLAY_NAME` | `oidc_display_name` | `Single sign-on` | Login button label. |
| `OF_OIDC_REQUIRE_VERIFIED_EMAIL` | `oidc_require_verified_email` | `true` | Reject ID tokens with an unverified email. |

### Plugins

| Variable | YAML key | Default | Description |
|---|---|---|---|
| `OF_PLUGIN_DIR` | `plugin_dir` | `<data dir>/plugins` | Scanned at start. Executables named `openfactory-plugin-<name>` are launched. See [Plugins](plugins.md). |
| `OF_PLUGINS` | `plugins` | none | Comma-separated paths of extra plugin executables (any file name). |
| `OF_PLUGIN_ENV_ALLOW` | `plugin_env_allow` | none | Comma-separated server variables that every plugin gets, in addition to `PATH`, `HOME`, `TMPDIR`, `LANG` and `TZ`. Server keys and credentials are never passed. See [Configure plugins](plugins.md#configure-plugins). |
| `OF_PLUGIN_ENV` | `plugin_env` | none | Server variables for single plugins, as `name=VAR1,VAR2;other=VAR3`. In YAML, a map from plugin name to a list. |

### Leases and delivery

The defaults suit most installs. A shorter lease recovers a crashed leader faster but renews more often.

| Variable | YAML key | Default | Description |
|---|---|---|---|
| `OF_LEADER_LEASE_TTL` | `leader_lease_ttl` | `15s` | Leader lease of the scheduler and timers. |
| `OF_OUTBOX_MAX_ATTEMPTS` | `outbox_max_attempts` | `8` | Delivery attempts before an event is dead-lettered. Admins see dead-lettered events on the Workers page and can redeliver them. |
| `OF_SHUTDOWN_DRAIN` | `shutdown_drain` | `5s` | Time a stopping server keeps its listener open after `/readyz` reports not ready, while Kubernetes removes the Pod from the Service endpoints. Then open requests get up to 30 seconds. `0` closes at once. Not negative. Keep the chart's `terminationGracePeriodSeconds` above this value plus 30 seconds. |

## Credentials that need no setting

- **Model credentials.** The server has none. Each harness CLI runs in the sandbox with the factory credential (`harness.auth`, a team secret). See [Harnesses](../harnesses.md).
- **Forge tokens.** A GitHub App installation token lives 1 hour. The server reuses a cached token only while it has at least 10 minutes left. Git gets a token from the control plane at each fetch or push through a credential helper, so `.git/config` holds no token.
- **Cloud identity tokens.** At most 1 hour. See [cloudProviders](../definition-reference.md#cloudproviders).
- **Integration credentials.** Kept in integration records and team secrets. See [Integrations](../integrations/README.md).

## Development mode

With `OF_DEV=true`, the server:

- creates `<data dir>/secret.key` when `OF_SECRET_KEY` is not set;
- if the database has no team, creates the team `Default`, the admin `admin@localhost` and an API key, and prints the key once:

  ```
  Open Factory dev bootstrap: created team "Default" and admin admin@localhost.
  API key (shown once): of_...
    export OF_SERVER=http://localhost:8080 OF_API_KEY=of_...
  ```

- accepts the printed key for dashboard sign-in (see [Security](../security.md#development-mode));
- uses the `local` sandbox backend by default.

> **Warning:** Never use development mode on a server that other people can reach.

## Database

`OF_DATABASE_URL` selects the store by its scheme.

| URL | Store |
|---|---|
| `sqlite:///var/lib/openfactory/of.db` | SQLite, absolute path. |
| `sqlite://data/of.db` | SQLite, relative to the working directory. |
| `postgres://user:pass@host:5432/db` or `postgresql://...` | PostgreSQL. |
| `sqlite://:memory:` | In-memory SQLite. Data is lost when the process stops. Tests and demos only. |

Other values fail with `OF_DATABASE_URL "...": unsupported scheme (want sqlite:// or postgres://)`.

- **SQLite** runs in WAL mode with foreign keys on. Production use needs **exactly one replica**. Put the file on a local disk or a ReadWriteOnce volume, not on a network file system.
- **PostgreSQL** supports one or more replicas, rolling upgrades without downtime and managed backups.

### Migrations

The server applies schema migrations when it opens the database. It records versions in `schema_migrations` and runs each migration in its own transaction. On PostgreSQL, an advisory lock lets only one process migrate at a time. Back up the database before an upgrade. See [Upgrading](upgrading.md).

## Secret key

`OF_SECRET_KEY` is 32 random bytes in base64 (standard or URL alphabet, with or without padding):

```bash
openssl rand -base64 32
```

Without `OF_SECRET_KEY` and without `OF_DEV=true`, the server does not start:

```
OF_SECRET_KEY is required (generate one with `openssl rand -base64 32`), or set OF_DEV=true to create one automatically
```

In development mode without a key, the server reads `<data dir>/secret.key`, or creates it with a random key and mode `0600`.

There are two kinds of key. You can rotate each separately.

- The **encryption key** (`OF_SECRET_KEY`) wraps the data key of each stored secret, MCP server OAuth token and personal forge token. See [Secrets at rest](../security.md#secrets-at-rest).
- The **signing key** (`OF_SIGNING_KEY`, default `OF_SECRET_KEY`). The server derives sub-keys with HMAC-SHA256 of the signing key and a label:

  | Label | Used for | Accepts the previous key |
  |---|---|---|
  | `openfactory/web-session-key` | Dashboard session and login-flow cookies. | Yes |
  | `openfactory/runner-token-key` | Runner session tokens (`ofs_`) and upload IDs of snapshots and artifacts. The token key ID is the sub-key version. | Yes |
  | `openfactory/oidc-signing-key` | The cloud identity key that signs run OIDC tokens. The JWKS lists the keys. | Yes |
  | `openfactory/account-link-key` | Account-linking requests from chat and tracker integrations. | No |
  | `openfactory/object-store-signing-key` | Presigned URLs of the local object store, when `OF_OBJECT_STORE_SIGNING_KEY` is not set. | No |

Each stored secret records the version of its wrapping key: the first 8 hex characters of the key's SHA-256 (not secret). On read, the server picks the matching key from `OF_SECRET_KEY` and `OF_SECRET_KEY_PREVIOUS`. For a secret without a version (stored before versions existed), it tries each key. New writes use the current key.

> **Warning:** If you lose the key, or change it without the [key rotation](#key-rotation) steps, the server cannot decrypt stored secrets. Store the keys in a secret manager and back them up separately from the database.

An embedding program can use a key management service for the encryption key with the `WithKeyProvider` option of `pkg/openfactory`. The signing key does not change.

## Key rotation

The new key goes in the current setting and the old key in the previous setting. Each replica reads with both keys and writes with the new key.

### Rotate the secret key

1. Generate a key with `openssl rand -base64 32`.
2. Set `OF_SECRET_KEY` to the new key and `OF_SECRET_KEY_PREVIOUS` to the old key.
3. Restart all replicas. Wait until all run with the new settings.
4. Run `openfactory secret rewrap` with the same settings and `OF_DATABASE_URL`. Use `--dry-run` to print the counts and write nothing.
5. Check the counts: `rewrapped`, `already current`, `changed during the run` and `failed`. A value that a replica wrote or deleted during the run is skipped. If a value failed, the command exits with code 1: fix the cause and run it again. The step is safe to repeat.
6. Remove `OF_SECRET_KEY_PREVIOUS` from all replicas and restart them.

`rewrap` re-wraps data keys only; encrypted values do not change.

Without `OF_SIGNING_KEY`, these steps also rotate the signing keys. Sessions and runner tokens stay valid until step 6, then the rules below apply.

### Rotate the signing key

1. Generate a key with `openssl rand -base64 32`.
2. Set `OF_SIGNING_KEY` to the new key and `OF_SIGNING_KEY_PREVIOUS` to the old signing key (the value of `OF_SECRET_KEY` if `OF_SIGNING_KEY` was not set).
3. Restart all replicas. New tokens and cookies use the new key.
4. Wait for the longest token life below, then remove `OF_SIGNING_KEY_PREVIOUS` and restart.

| Item | Life | Note |
|---|---|---|
| Dashboard session | 7 days | Wait 7 days to keep every session. |
| Runner session token and upload IDs | 1 hour | A runner with a stale token asks for a new one. |
| Cloud identity token | 1 hour | The JWKS lists the new key first and the old key second. Also wait for the provider's JWKS cache time. |

After you remove the previous key, items signed with the old key fail and users sign in again. Account-link requests and local object store presigned URLs have no previous key; they fail at once after a signing key change.

With `OF_SIGNING_KEY` set, a change of `OF_SECRET_KEY` does not affect sessions, runner tokens or the cloud identity key, and a change of `OF_SIGNING_KEY` does not affect stored secrets.

## Worker flags

`openfactory worker` connects a remote worker. See [Workers](workers.md).

| Flag | Environment variable | Default | Description |
|---|---|---|---|
| `--server URL` | `OF_SERVER` | `http://localhost:8080` | Control plane URL. Global flag. |
| `--api-key KEY` | `OF_API_KEY` | none (required) | Team API key (`of_<prefix>_<secret>`). Global flag. |
| `--id ID` | `OF_WORKER_ID` | host name, lower case, other characters changed to `-` | Agents select the worker with `workerHost: ID`. |
| `--backend` | `OF_SANDBOX_BACKEND` | `kubernetes`; `local` when `OF_DEV=true` | `kubernetes` (production) or `local` (development and tests only; logs a warning outside dev mode). |
| `--name N` | `OF_WORKER_NAME` | the id | Display name. |
| `--capacity N` | `OF_WORKER_CAPACITY` | `0` (no limit) | Most concurrent sandboxes. |
| `--label k=v` | `OF_WORKER_LABELS` | none | Repeatable. The variable is comma-separated, for example `region=eu,pool=gpu`. |
| `--one-shot` | `OF_WORKER_ONE_SHOT` | off | Run one sandbox, then exit. |
| `--metrics-addr ADDR` | `OF_METRICS_ADDR` | off | Serve `GET /metrics` with `of_worker_*` metrics, for example on `:9090`. |

A flag overrides its variable. The Helm chart sets only the variables. The worker also reads `OF_SANDBOX_ROOT`, `OF_SANDBOX_IMAGE`, `OF_RUNNER_IMAGE`, `OF_KUBECONFIG`, the `OF_K8S_*` settings, `OF_LOG_LEVEL` and `OF_LOG_FORMAT`. Its data directory is `$HOME/.openfactory`. It needs no secret key and no database.

### Worker id rules

A worker id has 1 to 64 characters: letters, digits, `.`, `_` and `-`, starting with a letter or digit. The server rejects these reserved ids:

- `server`, `local`, `kubernetes`, `direct`, `managed` (case-insensitive);
- any id that starts with `openfactory` (case-insensitive).
