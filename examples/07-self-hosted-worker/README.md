# 07 - Self-hosted worker

A factory that sends every run to a self-hosted microVM worker. Checkouts and
builds stay on a Linux host in your network, and each run gets its own
Firecracker microVM. The repository is on GitLab.

## What it shows

- `agentDefaults.workerHost: acme-prod-worker` sends every run to the worker.
  Agents and automations inherit it. To clear the inherited host on one of
  them, set `workerHost: null` or `""`. To spread runs over several workers,
  give them a label and use `workerHost: pool:<label>`.
- `machines/linux-worker.yaml` has a Linux platform, like the VM. The worker
  has a fixed VM size for each slot, and the machine's `instanceShape` must fit
  it. A microVM worker pulls the machine image on its host, and a private image
  uses the machine's `registryCredentialSecretName`. See
  [Private registries](../../docs/self-hosting/private-registries.md).
- The harness credentials are managed secrets (`auth.source: managedSecret`).
  `auth.source: workerEnvironment` is for development process workers only: a
  microVM worker gives its VMs no host variables. See
  [Harnesses](../../docs/harnesses.md#use-the-worker-environment).
- `codeForges: [GITLAB]` turns on GitLab only. Repositories use the full
  project path in `owner`, with subgroups (`acme/platform`).
- The GitLab `bot_mentioned` trigger has a server-managed `mentioned` filter.
  The server binds it to the factory's own bot account.
- The `internal-docs` MCP server is reachable by URL. A proxied server is
  reached from the control plane, which runs on Cloudflare, so a server on a
  private network needs `--direct` registration and a worker that can reach it.

## Prerequisites

1. A running control plane, with `OF_SERVER`, `OF_API_KEY` and `OF_PUBLIC_URL`
   set (see [Deploy to Cloudflare](../../docs/self-hosting/cloudflare.md)).
2. The GitLab integration, which needs the GitLab plugin Worker (see
   [GitLab](../../docs/integrations/gitlab.md)).
3. The secrets:

   ```bash
   openfactory secret set ANTHROPIC_API_KEY --type anthropic_api_key
   openfactory secret set OPENAI_API_KEY --type openai_api_key
   openfactory secret set INTERNAL_DOCS_TOKEN
   ```

4. A Linux host with KVM, enrolled as the worker `acme-prod-worker`. The worker
   connects outbound to the control plane and needs no inbound port.

   ```bash
   # An admin creates a single-use token (valid for 1 hour).
   curl -sS -X POST "$OF_SERVER/api/v1/workers/enrollments" \
     -H "Authorization: Bearer $OF_API_KEY"

   # On the host, as root: enroll and start the worker.
   openfactory-runtime worker enroll --url "$OF_PUBLIC_URL" --token ofe_... \
     --name acme-prod-worker
   systemctl enable --now openfactory-worker
   openfactory worker list
   ```

   See [Self-hosted workers](../../docs/self-hosted-machines.md) for the host
   requirements, the install steps and the settings.

The control plane rejects a `workerHost` that is not a registered worker, and a
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
factory's bot on a GitLab issue or merge request, a run starts in a microVM on
`acme-prod-worker`. The agent works to a draft merge request and replies in the
same thread.
