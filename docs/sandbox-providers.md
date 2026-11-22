# Sandbox providers

A sandbox provider creates the sandbox where a run executes. Every sandbox runs the same program, `openfactory-runtime runner`, with the harness (`claude` or `codex`). The runner connects out to the control plane over HTTPS. The control plane never connects in.

## Providers

| Provider ID | Where it runs | Image | Architecture | Egress control |
|---|---|---|---|---|
| `cloudflare-sandbox` | Cloudflare Containers, in its own Worker | OCI image from the Cloudflare registry | amd64 | `native` |
| `daytona` | Daytona cloud | A Daytona snapshot built from the sandbox image | amd64 | `native` when the policy fits, else `in-sandbox`, else `none` |
| `boatdev` | boat.dev | A saved boat.dev snapshot | amd64 | `in-sandbox` |
| a worker ID | A microVM worker on your own Linux host | The sandbox image, cached on the host | amd64 or arm64 | `native` |
| `process` | A development process worker. See [Workers](self-hosting/workers.md) | None | The host | `none` |
| a plugin provider | A plugin Worker | Set by the plugin | Set by the plugin | Set by the plugin |

Each provider states its capabilities: egress control, image type, architectures, longest lifetime, start rate, instance types and the daemon ports on loopback. The control plane takes the architecture from the provider, not from its own host.

Egress control on hosted providers is best effort. A provider that cannot enforce a policy declares `egress: none`. See [Network policy](self-hosting/network-policy.md#enforcement).

## Select a provider

`workerHost` selects where a run executes. Set it in `agentDefaults`, in an agent, in an automation, or per run.

| `workerHost` | Where the run executes |
|---|---|
| empty or `server` | The default provider of the team. The Worker variable `OF_SANDBOX_PROVIDER` names it. |
| a provider ID | That provider: `cloudflare-sandbox`, `daytona` or `boatdev`. |
| a worker ID or name | That enrolled worker. |
| `pool:<label>` | The online worker that has that label, allows the factory and has the most free slots. |

Use `workerHost: server` when you want the default.

For the development provider, set `OF_SANDBOX_PROVIDER=local` on a development control plane. It runs sessions on development process workers. See [Workers](self-hosting/workers.md).

## Machine sizes

A machine (`machines/<name>.yaml`) sets `instanceShape` with `vcpus` and `memoryGb`. At preflight, the control plane maps the shape to the smallest instance type of the provider that is at least as large. A run that asks for more than the largest type fails with `invalid_configuration`. A microVM worker has a fixed size for each slot, and the machine shape must fit it.

| Provider | Instance types | Largest |
|---|---|---|
| `cloudflare-sandbox` | `lite`, `basic`, `standard-1` to `standard-4` | 4 vCPU, 12 GiB memory, 20 GB disk |
| `daytona` | `default` (1 vCPU, 1 GiB, 3 GiB disk) and `max` | 4 vCPU, 8 GiB memory, 10 GiB disk (the default organization limit) |
| `boatdev` | `small` (2 vCPU, 4 GiB), `default` (4 vCPU, 8 GiB), `large` (8 vCPU, 16 GiB) | `large`. `xlarge` needs a larger plan. |
| microVM worker | One slot size for each worker | The slot size |

## Limits and notes

### cloudflare-sandbox

- It is a separate Worker (`openfactory-provider-cloudflare-sandbox`) that the control plane reaches through a service binding. A control-plane deploy does not restart running sandboxes.
- A container stops after its Durable Object is idle, after at most 6 hours, and on a host restart. A host restart ends the session with `sandbox_lost`, and the platform can retry it.
- The provider Worker intercepts all outbound HTTP and HTTPS and checks the allow list of the policy. Other ports are blocked. A policy that lists IP addresses is refused.
- Deploy it as described in [Deploy to Cloudflare](self-hosting/cloudflare.md#5-deploy-the-provider-worker).

### daytona

- The organization must be on tier 3 or higher. Tiers 1 and 2 cannot reach `*.workers.dev` and are refused at preflight.
- Sandboxes do not stop on their own. A time limit bounds each sandbox, so a leaked sandbox ends.
- Daytona sets a snapshot inactive after 2 weeks without use. Preflight builds it again.
- A native domain allow list holds at most 100 hosts and cannot mix with IP rules. Daytona re-encrypts TLS with its own CA, which the sandbox trusts.
- Shared regions are `eu` and `us`.

### boatdev

- boat.dev limits starts to 12 a minute, 60 an hour and 200 a day, and allows 100 sandboxes at once. The control plane queues runs to stay inside these limits.
- The API key needs the `ci` preset. The region is EU only.
- The command API has no environment field, so the runner starts from a setup script, and its environment goes into a file that only root can read.
- The in-sandbox lock is applied again at each boot, because firewall rules are not kept in snapshots.

### microVM workers

See [Self-hosted workers](self-hosted-machines.md).

## Credentials

- `cloudflare-sandbox` needs no credential: it is a service binding in the same Cloudflare account.
- `daytona` needs a Daytona API key and the organization tier. Any key of the organization can control every running sandbox of that organization, so use a dedicated organization.
- `boatdev` needs a boat.dev API key (`boat_...`) with the `ci` preset.

The control plane reads the credentials of `daytona` and `boatdev` from the Worker secrets `OF_DAYTONA_API_KEY` and `OF_BOATDEV_API_KEY`, and the Daytona tier and region from the variables `OF_DAYTONA_TIER` and `OF_DAYTONA_TARGET`. See [Configuration](self-hosting/configuration.md). Do not put the keys in a definition or in an agent secret.

## Build the sandbox image

The image is `ghcr.io/openfactory-org/sandbox`. It has the users `agent` (uid 1001) and `ofproxy`, `git`, `iptables`, `ip6tables`, `claude` and `codex`. It has no `sudo` and no setuid programs. `/opt/openfactory/bootstrap.sh` starts the runner: it downloads `openfactory-runtime` from a control-plane route when the image has no matching copy, checks its `sha256`, and starts it. Session start refuses a runner whose version does not match `OF_RUNNER_VERSION`.

The files are in `deploy/sandbox/`: `Dockerfile` (Daytona snapshot and generic image), `Dockerfile.cloudflare` (Cloudflare Containers) and `install.sh` (the install steps, also used to build the boat.dev snapshot).

## Plugin providers

A plugin Worker can export a sandbox provider. See [Plugins](self-hosting/plugins.md).
