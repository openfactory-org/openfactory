# Deploy to Cloudflare

The control plane is one Cloudflare Worker with Durable Objects (SQLite storage), an R2 bucket, a Queue, a Cron trigger and Static Assets for the dashboard. This page deploys it into your Cloudflare account.

## Before you start

- A Cloudflare account on the Workers Paid plan. The Worker holds long polls and event streams, and uses Durable Objects, Queues and Containers.
- Node.js 24 or later and a checkout of this repository.
- A domain for the control plane (recommended). Without one, use a `workers.dev` address.
- For the `cloudflare-sandbox` provider: access to Cloudflare Containers on your account, and Docker to build the image.

## 1. Install and build

```bash
npm ci
npm run build
npx wrangler login
```

The build writes the dashboard to `web/dist`. The Worker serves it as Static Assets.

## 2. Create the resources

The configuration is `apps/control-plane/wrangler.jsonc`. It declares these resources:

| Resource | Binding | Name in the file |
|---|---|---|
| R2 bucket | `DATA` | `openfactory-data` |
| Queue | `JOBS` | `openfactory-jobs`, with the dead-letter queue `openfactory-jobs-dlq` |
| Durable Objects | `TEAM`, `WORK`, `DISPATCH`, `ROUTER`, `INDEX`, `INGRESS`, `WORKER_HUB`, `BENCHMARK` | Created by the first deploy |

Create the bucket and the queues:

```bash
npx wrangler r2 bucket create openfactory-data
npx wrangler queues create openfactory-jobs
npx wrangler queues create openfactory-jobs-dlq
```

Create an R2 API token with read and write access to the bucket. The control plane uses its S3 credentials to sign URLs that the runner in a sandbox uses to upload snapshots and artifacts. See [Object storage](object-storage.md).

## 3. Set the variables

Edit the `vars` block of `wrangler.jsonc`:

| Variable | Set it to |
|---|---|
| `OF_PUBLIC_URL` | The public origin of the control plane, for example `https://factory.example.com`. |
| `OF_R2_ACCOUNT_ID` | Your Cloudflare account ID. |
| `OF_R2_BUCKET` | The bucket name. |
| `OF_SANDBOX_PROVIDER` | The default provider for `workerHost: server`. |

For a custom domain, add it to `routes` in the file. Without a domain, `workers_dev` gives an address under `workers.dev`, and `OF_PUBLIC_URL` must be that address. For every variable, see [Configuration](configuration.md).

## 4. Set the secrets

```bash
openssl rand -base64 32 | npx wrangler secret put OF_SECRET_KEY -c apps/control-plane/wrangler.jsonc
openssl rand -base64 32 | npx wrangler secret put OF_SIGNING_KEY -c apps/control-plane/wrangler.jsonc
npx wrangler secret put OF_R2_ACCESS_KEY_ID -c apps/control-plane/wrangler.jsonc
npx wrangler secret put OF_R2_SECRET_ACCESS_KEY -c apps/control-plane/wrangler.jsonc
```

Keep a copy of `OF_SECRET_KEY` in a secret manager. If you lose it, the control plane cannot decrypt stored secrets. See [Secret key](configuration.md#secret-key).

## 5. Deploy the provider Worker

Skip this step if you do not use `cloudflare-sandbox`. The control plane binds the provider Worker as `PROVIDER_CLOUDFLARE_SANDBOX`, so deploy the provider before the control plane. The provider is a separate Worker, so a control-plane deploy does not restart running sandboxes.

```bash
npx wrangler deploy -c providers/cloudflare-sandbox/wrangler.jsonc
```

Wrangler builds the sandbox image from `deploy/sandbox/Dockerfile.cloudflare` and pushes it to the Cloudflare registry.

## 6. Deploy the plugin Workers

Skip this step if you use neither GitLab nor Azure DevOps. The control plane binds `PLUGIN_GITLAB` and `PLUGIN_AZUREDEVOPS`. Deploy each plugin Worker that you use:

```bash
npx wrangler deploy -c plugins/gitlab/wrangler.jsonc
npx wrangler deploy -c plugins/azuredevops/wrangler.jsonc
```

A deploy of the control plane fails while a bound Worker does not exist. If you use only one of them, remove the other binding from the `services` block. See [Plugins](plugins.md).

## 7. Deploy the control plane

```bash
npx wrangler deploy -c apps/control-plane/wrangler.jsonc
```

The runner binary is served from R2 to the sandboxes. Publish the `openfactory-runtime` release that matches `OF_RUNNER_VERSION`. Session start refuses a runner with another version.

## 8. Create the first team

```bash
export OF_SERVER=https://factory.example.com
openfactory bootstrap --email you@example.com
```

The command prints an API key once. Store it, and set it as `OF_API_KEY`. Check it:

```bash
export OF_API_KEY=of_...
openfactory factory list
```

Then continue with the [Quickstart](../quickstart.md#2-store-the-model-credential): store the model credential, connect a forge and create a factory.

## Check the deployment

| Check | Expected |
|---|---|
| `curl $OF_SERVER/healthz` | `ok` |
| `curl $OF_SERVER/readyz` | `ok` |
| `openfactory version` | The client version. |
| `GET /api/v1/version` | The control plane version. |

To read the logs, run `npx wrangler tail -c apps/control-plane/wrangler.jsonc`. See [Monitoring](../monitoring.md).

## Cost and limits

- The Worker, Durable Objects, R2, Queues and Containers are billed by Cloudflare. Check the current prices.
- Durable Object values are limited to 8 MB. The control plane stores large definition trees, payloads and event bundles in R2.
- A Cron trigger runs every minute as a safety net that re-arms timers.

## Upgrade, roll back, remove

See [Upgrading](upgrading.md). To remove the deployment, delete the Workers, the queues and the bucket in the Cloudflare dashboard or with `wrangler delete`. Deleting the Durable Objects deletes all control-plane data.
