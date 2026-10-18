# 07 - Self-hosted worker

A factory that sends every run to a self-hosted worker. Checkouts, builds and
harness credentials stay inside your network. The repository is on GitLab.

## What it shows

- `agentDefaults.workerHost: acme-prod-worker` sends every run to the worker.
  Agents and automations inherit it. To clear the inherited host on one of
  them, set `workerHost: null` or `""`.
- `machines/linux-worker.yaml` has a Linux platform, like the worker. The
  Kubernetes backend runs Linux sandboxes and turns `instanceShape` into
  container requests and limits. It ignores the machine's
  `registryCredentialSecretName` and pulls private images with the
  `imagePullSecrets` of the pod template. See
  [Private registries](../../docs/self-hosting/private-registries.md).
- `auth.source: workerEnvironment` makes Claude Code and Codex use the
  credentials in the worker's environment (`ANTHROPIC_API_KEY`,
  `OPENAI_API_KEY`), not a managed secret. It requires a `workerHost` and
  cannot name a `secretName`.
- `codeForges: [GITLAB]` turns on GitLab only. Repositories use the full
  project path in `owner`, with subgroups (`acme/platform`).
- The GitLab `bot_mentioned` trigger has a server-managed `mentioned` filter.
  The server binds it to the factory's own bot account.
- The `internal-docs` MCP server is reachable only from the worker's network.
  A self-hosted worker can connect to it by URL.

## Prerequisites

1. A running server, with `OF_SERVER`, `OF_API_KEY` and `OF_PUBLIC_URL` set
   (see [Self-hosting](../../docs/self-hosting/README.md)).
2. The GitLab integration (see [GitLab](../../docs/integrations/gitlab.md)).
3. The secret for the `internal-docs` MCP server:

   ```bash
   openfactory secret set INTERNAL_DOCS_TOKEN
   ```

4. A worker in a Kubernetes cluster in your network. The worker connects
   outbound to the server. It starts one sandbox (a Job with one pod) for each
   run, in its own namespace. The worker reads harness credentials from its
   environment, not from flags. The Helm chart can deploy it
   (`worker.enabled=true`). To start it by hand:

   ```bash
   export ANTHROPIC_API_KEY=...   # Claude Code, auth.source: workerEnvironment
   export OPENAI_API_KEY=...      # Codex, auth.source: workerEnvironment
   export OF_K8S_NAMESPACE=factory-sandboxes
   openfactory worker --id acme-prod-worker --backend kubernetes \
     --server "$OF_PUBLIC_URL" --api-key "$OF_WORKER_API_KEY"
   ```

   `--id` (env `OF_WORKER_ID`) is the ID that agents reference as
   `workerHost`. `--server` and `--api-key` can also come from `OF_SERVER` and
   `OF_API_KEY`.

The server rejects a `workerHost` that is not a registered worker, and a
missing secret. Do these steps before you apply.

## Apply

1. Set your project:
   - `factory.yaml`: under `repositories`, change `owner: acme/platform` and
     `name: billing`.
   - `automations/labeled-issue/automation.md`: in `filter.repos`, change
     `acme/platform/billing`.
2. In `machines/linux-worker.yaml`, change `image` to your image.
3. In `factory.yaml`, change `mcpServers.internal-docs.url`, or remove the
   server.
4. Validate and apply:

   ```bash
   openfactory validate examples/07-self-hosted-worker
   openfactory factory apply examples/07-self-hosted-worker
   ```

## What to expect

`apply` creates the factory `self-hosted`. When someone mentions the
factory's bot on a GitLab issue or merge request, a run starts on
`acme-prod-worker`. The agent works to a draft merge request and replies in
the same thread.
