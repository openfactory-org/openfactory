# Configuration

Settings of the control plane Worker and flags of the runtime programs.

## Configuration sources

The control plane reads three kinds of input:

| Source | Set with | Holds |
|---|---|---|
| Variables | The `vars` block of `apps/control-plane/wrangler.jsonc` | Plain settings. All values are strings. |
| Secrets | `wrangler secret put`, or `wrangler deploy --secrets-file` | Keys and credentials. |
| Bindings | The same file | Durable Objects, the R2 bucket, the queue, service bindings and Static Assets. |

Local development reads variables and secrets from `.dev.vars` next to the config file. Keep secrets out of `wrangler.jsonc` and out of version control.

After you change the file, redeploy with `npx wrangler deploy -c apps/control-plane/wrangler.jsonc`.

## Variables

| Variable | Default | Description |
|---|---|---|
| `OF_PUBLIC_URL` | `https://factory.example.com` | The public origin of the control plane (absolute `http` or `https`). Used for webhook URLs, OAuth redirects, links, and as the control URL of runners. |
| `OF_R2_ACCOUNT_ID` | none | Your Cloudflare account ID. Builds the R2 S3 host `<account>.r2.cloudflarestorage.com`. |
| `OF_R2_BUCKET` | `openfactory-data` | The R2 bucket for run data. |
| `OF_OBJECT_STORE_PRESIGN_TTL` | `15m` | Life of presigned run-data URLs. At most 7 days. |
| `OF_QUEUE_TIMEOUT` | `30m` | Longest time in `queued`. Then the run goes to `error` with `infrastructure_timeout`. |
| `OF_MAX_SESSION_DURATION` | `4h` | Longest session life. Then the control plane stops the session with reason `deadline`. |
| `OF_SESSION_STOP_GRACE` | `30s` | Time for the runner's snapshot after a stop or cancel, before the sandbox is stopped. |
| `OF_SANDBOX_START_TIMEOUT` | `10m` | Longest time from sandbox assignment to session start. Then the session ends with `sandbox_create_failed`. |
| `OF_SANDBOX_PROVIDER` | `cloudflare-sandbox` | The default provider for `workerHost: server`. `local` is for development. See [Sandbox providers](../sandbox-providers.md). |
| `OF_SANDBOX_IMAGE` | `ghcr.io/openfactory-org/sandbox:latest` | The default image of a sandbox task. The image of the run's environment wins, then the image of the agent's machine. Empty uses the default. The image must contain `sh` and `git`. |
| `OF_ALLOW_UNENFORCED_NETWORK_POLICY` | `false` | `true` lets a restricted network policy run on a provider with `egress: none`. See [Network policy](network-policy.md#the-capability-gate). |
| `OF_DEV_PROCESS_WORKERS` | `false` | `true` allows development process workers. Do not set it in production. See [Workers](workers.md). |
| `OF_RUNNER_VERSION` | `0.0.0-dev` | The version of `openfactory-runtime` that session start requires. |
| `OF_OIDC_ISSUER_URL` | none | The issuer URL for dashboard sign-in. See [OIDC login](oidc.md). |
| `OF_OIDC_CLIENT_ID` | none | The OIDC client ID. Required with an issuer. |
| `OF_LOG_LEVEL` | `info` | The log level. |
| `OF_MAX_SANDBOXES` | `0` (no limit) | Most open sessions on the built-in providers. A queued run waits at the limit. Sessions on workers do not count. |
| `OF_MAX_ORCHESTRATION_DEPTH` | `1` | Depth limit of a run tree. A root run has depth 0. A run at depth `d` can start child runs with `run_agents` only if `d` is less than this value. |
| `OF_OBJECT_STORE_RETENTION` | `0` (keep forever) | Age after which run data in R2 is deleted, a duration such as `720h`. `0` or empty keeps run data forever. See [Object storage](object-storage.md). |
| `OF_CORRECTNESS_MODEL` | empty | Model of the built-in correctness judge. Empty uses the built-in default. See [Measure and improve](../measure-and-improve.md). |
| `OF_ANALYSIS_MODEL` | empty | Model of the self-improvement failure analysis. Empty uses the built-in default. |
| `OF_IMPROVEMENT_AGENT` | empty | Agent that runs self-improvement. Empty uses the built-in default. |
| `OF_CLOUD_ID_ISSUER_URL` | empty (`OF_PUBLIC_URL`) | Issuer URL (`iss` claim) of the [cloud identity](../definition-reference.md#cloudproviders) tokens of runs. Cloud providers read its `/.well-known/` documents. Do not confuse it with `OF_OIDC_ISSUER_URL`, the identity provider for dashboard sign-in. |
| `OF_DAYTONA_TIER` | `3` | Tier of the Daytona organization, `1` to `4`. The control plane refuses tiers 1 and 2. See [Sandbox providers](../sandbox-providers.md). |
| `OF_DAYTONA_TARGET` | empty (account default) | Daytona shared region: `eu` or `us`. |

Durations use the form `90s`, `15m`, `4h` or `1h30m`.

## Secrets

| Secret | Required | Description |
|---|---|---|
| `OF_SECRET_KEY` | yes | Base64 of 32 random bytes. Wraps the data key of every stored secret. See [Secret key](#secret-key). |
| `OF_SIGNING_KEY` | yes | Base64 of 32 random bytes. Signs dashboard sessions, runner tokens and upload IDs. |
| `OF_R2_ACCESS_KEY_ID`, `OF_R2_SECRET_ACCESS_KEY` | yes | S3 credentials of an R2 API token, for presigned URLs. |
| `OF_SECRET_KEY_PREVIOUS`, `OF_SIGNING_KEY_PREVIOUS` | no | Comma-separated old keys, read only. See [Key rotation](#key-rotation). |
| `OF_OIDC_CLIENT_SECRET` | no | The OIDC client secret. A PKCE public client needs none. |
| `OF_CLOUD_ID_KEY` | no | The P-256 private key that signs the [cloud identity](../definition-reference.md#cloudproviders) tokens of runs. |
| `OF_DAYTONA_API_KEY` | no | API key of the `daytona` sandbox provider. Without it, `daytona` is not available. |
| `OF_BOATDEV_API_KEY` | no | API key (`boat_...`) of the `boatdev` sandbox provider. Without it, `boatdev` is not available. |

The optional OIDC settings (`OF_OIDC_REDIRECT_URL`, `OF_OIDC_SCOPES`, `OF_OIDC_ALLOWED_DOMAINS`, `OF_OIDC_GROUPS_CLAIM`, `OF_OIDC_ADMIN_GROUPS`, `OF_OIDC_EDITOR_GROUPS`, `OF_OIDC_DEFAULT_ROLE`, `OF_OIDC_DISPLAY_NAME`, `OF_OIDC_REQUIRE_VERIFIED_EMAIL`) are read from the same variables. See [OIDC login](oidc.md).

## Bindings

| Binding | Kind | Use |
|---|---|---|
| `TEAM`, `WORK`, `DISPATCH`, `ROUTER`, `INDEX`, `INGRESS`, `WORKER_HUB`, `BENCHMARK` | Durable Objects | State and coordination. |
| `DATA` | R2 bucket | Run data and the runner binary. |
| `JOBS` | Queue | Unordered background jobs. |
| `ASSETS` | Static Assets | The dashboard. |
| `PROVIDER_CLOUDFLARE_SANDBOX` | Service binding | The `cloudflare-sandbox` provider Worker. |
| `PLUGIN_GITLAB`, `PLUGIN_AZUREDEVOPS` | Service bindings | Plugin Workers. |

## Team settings

Limits that an admin changes at run time are team settings, not Worker variables: model spend limits, the idle timeout, the number of open sessions, and run data retention. Factories also set limits in their definition, for example `maxConcurrentRuns`, `spendLimit` and `idleTimeoutMinutes`. See the [definition reference](../definition-reference.md).

## Credentials that need no setting

- **Model credentials.** The control plane has none. Each harness CLI runs in the sandbox with the factory credential (`harness.auth`, a team secret). See [Harnesses](../harnesses.md).
- **Forge tokens.** A GitHub App installation token lives 1 hour. Git gets a token from the control plane at each fetch or push through a credential helper, so `.git/config` holds no token.
- **Cloud identity tokens.** At most 1 hour. See [cloudProviders](../definition-reference.md#cloudproviders).
- **Integration credentials.** Kept in integration records and team secrets. See [Integrations](../integrations/README.md).

## Local development

`npm run dev` starts `wrangler dev` on port 8787. It uses local Durable Objects and a local R2 bucket. See [Local testing](local-testing.md).

> **Warning:** A development control plane with `OF_DEV_PROCESS_WORKERS=true` runs agents on your host with no isolation. Never expose it to other people.

## Secret key

`OF_SECRET_KEY` is 32 random bytes in base64 (standard or URL alphabet, with or without padding):

```bash
openssl rand -base64 32
```

There are two kinds of key. You can rotate each one separately.

- The **encryption key** (`OF_SECRET_KEY`) wraps the data key of each stored secret, MCP server OAuth token and personal forge token. See [Secrets at rest](../security.md#secrets-at-rest).
- The **signing key** (`OF_SIGNING_KEY`, default `OF_SECRET_KEY`). The control plane derives sub-keys with HMAC-SHA256 of the signing key and a label:

  | Label | Used for | Accepts the previous key |
  |---|---|---|
  | `openfactory/web-session-key` | Dashboard session and login-flow cookies. | Yes |
  | `openfactory/runner-token-key` | Runner session tokens and upload IDs of snapshots and artifacts. | Yes |
  | `openfactory/oidc-signing-key` | The cloud identity key that signs run OIDC tokens. | Yes |
  | `openfactory/account-link-key` | Account-linking requests from chat and tracker integrations. | No |
  | `openfactory/upload-id-key` | Upload IDs and other short MACs. | No |

Each stored secret records the version of its wrapping key: the first 8 hex characters of the key's SHA-256 (not secret). On read, the control plane picks the matching key from `OF_SECRET_KEY` and `OF_SECRET_KEY_PREVIOUS`.

> **Warning:** If you lose the key, or change it without the [key rotation](#key-rotation) steps, the control plane cannot decrypt stored secrets. Store the keys in a secret manager and back them up.

## Key rotation

The new key goes in the current secret and the old key in the previous secret. The control plane reads with both keys and writes with the new key.

### Rotate the secret key

1. Generate a key with `openssl rand -base64 32`.
2. Set `OF_SECRET_KEY` to the new key and `OF_SECRET_KEY_PREVIOUS` to the old key, then deploy.
3. Run `openfactory secret rewrap`. It wraps the data keys of all secrets again with the current key. Encrypted values do not change.
4. Check the counts that the command prints. If a value failed, fix the cause and run it again. The command is safe to repeat.
5. Remove `OF_SECRET_KEY_PREVIOUS` and deploy.

Without `OF_SIGNING_KEY`, these steps also rotate the signing keys. Sessions and runner tokens stay valid until step 5.

### Rotate the signing key

1. Generate a key with `openssl rand -base64 32`.
2. Set `OF_SIGNING_KEY` to the new key and `OF_SIGNING_KEY_PREVIOUS` to the old signing key (the value of `OF_SECRET_KEY` if `OF_SIGNING_KEY` was not set), then deploy. New tokens and cookies use the new key.
3. Wait for the longest token life below, then remove `OF_SIGNING_KEY_PREVIOUS` and deploy.

| Item | Life | Note |
|---|---|---|
| Dashboard session | 7 days | Wait 7 days to keep every session. |
| Runner session token and upload IDs | 1 hour | A runner with a stale token asks for a new one. |
| Cloud identity token | 1 hour | The key set lists the new key first. Also wait for the cache time of the cloud provider. |

After you remove the previous key, items signed with the old key fail and users sign in again. Account-link requests have no previous key and fail at once after a signing key change.

With `OF_SIGNING_KEY` set, a change of `OF_SECRET_KEY` does not affect sessions or runner tokens, and a change of `OF_SIGNING_KEY` does not affect stored secrets.

## Runtime flags

The runtime program `openfactory-runtime` runs in sandboxes and on worker hosts.

### Development process worker

`openfactory-runtime worker --process` connects a development worker. See [Workers](workers.md).

| Flag | Environment variable | Default | Description |
|---|---|---|---|
| `--server URL` | `OF_SERVER` | none | Control plane URL. |
| `--api-key KEY` | `OF_API_KEY` | none | Team API key (`of_...`). Not needed with `--key-file`. |
| `--key-file PATH` | `OF_WORKER_KEY_FILE` | none | Ed25519 private key of an enrolled worker. The worker signs the challenge of the control plane with it. |
| `--id ID` | `OF_WORKER_ID` | host name, lower case | Agents select the worker with `workerHost: ID`. |
| `--name N` | `OF_WORKER_NAME` | the id | Display name. |
| `--capacity N` | `OF_WORKER_CAPACITY` | `0` (no limit) | Most concurrent sessions. |
| `--label k=v` | `OF_WORKER_LABELS` | none | Repeatable. The variable is comma-separated, for example `region=eu,pool=gpu`. |
| `--one-shot` | `OF_WORKER_ONE_SHOT` | off | Run one session, then exit. |
| `--metrics-addr ADDR` | `OF_METRICS_ADDR` | off | Serve `GET /metrics` with `of_worker_*` metrics, for example on `:9090`. |
| `--process` | none | off | Run each session as a host process. |

A flag overrides its variable. The worker also reads `OF_SANDBOX_ROOT` (default `~/.openfactory/sandboxes`) and `OF_RUNNER_COMMAND`.

A worker ID has 1 to 64 characters: letters, digits, `.`, `_` and `-`, starting with a letter or digit. These IDs are reserved: `server`, `local`, `direct`, `managed`, the provider IDs `cloudflare-sandbox`, `daytona` and `boatdev`, and any ID that starts with `openfactory`.

### Runner and egress proxy

| Variable or flag | Description |
|---|---|
| `OF_EGRESS_LOCK` | Set by the bootstrap: `none`, `netns` or `iptables`. See [Network policy](network-policy.md#in-sandbox-lock). |
| `OF_LOOPBACK_DAEMON_PORTS` | Comma-separated loopback ports of provider daemons that the lock blocks for the agent. |
| `--allow-private-host` (`netguard`) | Lets the egress proxy dial a private address for a named host. For tests. |

### Worker host settings

A microVM worker reads `/etc/openfactory/worker.yaml`. See [Self-hosted workers](../self-hosted-machines.md#configure).
