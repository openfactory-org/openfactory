# Self-hosted workers

Run agent sessions on your own Linux hosts, with the isolation of a Firecracker microVM. A host that you enroll is a **worker**. It sits next to the hosted [sandbox providers](sandbox-providers.md) and serves the same runs.

Use a self-hosted worker when:

- repositories, package mirrors or MCP servers are reachable only from your network;
- builds need hardware or tools that hosted providers do not have;
- source checkouts must stay on your hosts.

The worker connects out to the control plane over a WebSocket. It needs no inbound port.

## How it works

1. You install `openfactory-runtime` on the host and enroll it with the control plane.
2. When a session is assigned to the worker, the host boots one Firecracker microVM from the sandbox image.
3. The runner inside the VM talks to the control plane directly over HTTPS. The host never relays runner traffic.
4. The host reports the state of the VM to the control plane over worker protocol v2.

Each session gets a fresh VM. Workspaces go to snapshots in R2, as with the hosted providers.

## Host requirements

| Item | Requirement |
|---|---|
| Operating system | Linux with systemd and cgroup v2. macOS and Windows hosts are not supported. |
| CPU architecture | amd64 or arm64. |
| Virtualization | A usable `/dev/kvm` (read and write). A host without KVM cannot enroll. Many VPS plans do not offer KVM: check before you rent. |
| Host kernel | 6.1 or 6.18 are validated. Version 5.15 or later works with a warning. |
| Tools | `nft`, `ip`, `git` and e2fsprogs (`mkfs.ext4`). |
| Privileges | root. |
| Network | Outbound HTTPS to the control plane. |

`openfactory-runtime worker doctor` checks these items and checks for firewall tools (Docker, ufw, firewalld) whose rules would drop VM traffic. It prints the rule to add.

## Install

1. Install the runtime, Firecracker, the jailer and the guest kernel. The install script downloads each file, checks its checksum and writes the `openfactory-worker.service` unit. It does not start the service.
2. Run the check:

   ```bash
   sudo openfactory-runtime worker doctor
   ```

   Fix every failure. A warning does not stop the worker.

## Enroll

1. An admin creates a single-use enrollment token. It lives 1 hour.

   ```bash
   curl -sS -X POST "$OF_SERVER/api/v1/workers/enrollments" \
     -H "Authorization: Bearer $OF_API_KEY"
   ```

   The response has the `token` (prefix `ofe_`) and a `command` to run on the host.
2. On the host, enroll:

   ```bash
   sudo openfactory-runtime worker enroll --url https://factory.example.com --token ofe_... --name build-01
   ```

   The command makes an Ed25519 key pair, sends the public key and the facts of the host (architecture, kernel, CPUs, memory, disk, Firecracker version, runtime version, KVM), and stores the worker ID. The private key stays on the host.
3. Start the service:

   ```bash
   sudo systemctl enable --now openfactory-worker
   ```

4. Check the worker:

   ```bash
   openfactory worker list
   ```

At each connection, the control plane sends a challenge and the worker signs it with its key. The worker needs no API key.

## Configure

The file `/etc/openfactory/worker.yaml` holds the settings. `enroll` writes `control_url` and `name`.

| Key | Default | Description |
|---|---|---|
| `labels` | none | Labels, for `workerHost: pool:<label>`. |
| `slots` | `2` | Sessions that run at once. |
| `slot.vcpus`, `slot.memory_mib`, `slot.scratch_gib` | `2`, `4096`, `40` | The fixed size of each VM and its scratch disk. |
| `data_dir` | `/var/lib/openfactory` | Images, kernels, jails and the identity. |
| `disk.reserve_gib` | `20` | Disk that the worker never uses. |
| `disk.images_max_gib` | `60` | Limit of the image cache. Old images go first. |
| `disk.gitcache_max_gib` | `50` | Limit of the git mirror cache. |
| `network.subnet` | `10.213.0.0/16` | Address range of the VMs, one /30 for each slot. |
| `network.internal_allow` | none | `ip:port` exceptions to the private-range block, for example a self-managed forge. |
| `snapshots` | `false` | Restore VMs from a snapshot for a faster start. |

Slots times the VM size must fit the host: memory for all slots plus a 2 GiB reserve must not exceed host memory, and `doctor` warns when the vCPUs of all slots exceed the host CPUs.

## Route runs to a worker

```yaml
# factory.yaml
agentDefaults:
  workerHost: build-01        # a worker ID or name
```

| Value | Selects |
|---|---|
| a worker ID or name | That worker. |
| `pool:<label>` | The online worker that has the label, allows the factory and has the most free slots. |

- A new worker serves all factories of the team. An admin can restrict it to a list of factories with `PATCH /api/v1/workers/{id}`.
- A full worker or pool queues runs in FIFO order. The run shows the reason `waiting_for_worker`. After `OF_QUEUE_TIMEOUT`, a queued run ends with `infrastructure_timeout`.
- Machine sizes (`instanceShape`) must fit the slot size.
- The harness auth `workerEnvironment` does not work on microVM workers. See [Harnesses](harnesses.md#use-the-worker-environment).

See [examples/07-self-hosted-worker](../examples/07-self-hosted-worker/).

## Operate

| Action | Command or request |
|---|---|
| List workers | `openfactory worker list`, `GET /api/v1/workers` |
| Show status on the host | `sudo openfactory-runtime worker status` |
| Change labels, slots, slot size or factories | `PATCH /api/v1/workers/{id}` |
| Drain: finish running sessions, take no new ones | `POST /api/v1/workers/{id}/drain`, or `openfactory-runtime worker drain` on the host |
| Resume | `POST /api/v1/workers/{id}/resume` |
| Revoke: close the connection and refuse the worker for good | `POST /api/v1/workers/{id}/revoke` |
| Remove the record | `DELETE /api/v1/workers/{id}` |

The dashboard Workers page shows status, slots in use, labels, version, last connection and these actions. Admin rights are required for changes.

A worker has one of these states: `pending` (enrolled, never connected), `online`, `offline`, `draining` and `revoked`.

The systemd unit uses `Delegate=yes` and `KillMode=process`, so a restart of the agent does not stop running VMs. A turn survives an agent restart. The agent adopts the VMs when it starts again.

### Updates

The worker sends its runtime version when it connects. The control plane refuses a protocol version below its minimum and shows "update available". Update by installing the new release and restarting the service. There is no self-update.

### Connection and recovery

A worker that loses its connection leaves its sessions running. Each runner keeps its own connection to the control plane. When the worker connects again, it lists the sessions that it holds and the control plane updates their states. While a worker is disconnected, the control plane treats its sessions like a silent runner: it waits 90 seconds, then 180 seconds, then ends the session with `runner_lost`.

## Network

Each VM has one tap device, one /30 subnet and one egress proxy on the host address of the tap.

- With any policy, the host drops VM-to-VM traffic, VM-to-host traffic (except the proxy port), private ranges, link-local, carrier-grade NAT, multicast, the cloud metadata addresses and all IPv6.
- With a restricted policy, the VM has no forward path and no DNS. It reaches the internet only through the proxy. The control plane and the R2 host are always allowed.
- With no policy, the VM gets NAT and a DNS relay, and the drops above still apply.
- The proxy refuses connections to private addresses even for an allowed host name.

See [Network policy](self-hosting/network-policy.md).

## Git mirror cache

The host keeps a mirror of the repositories of recent runs, and attaches a read-only image of each repository to the VMs of sessions that use it. The runner then clones from the cache and a second clone is faster. A VM sees only the repositories of its own session. The host fetches with a read-only, single-repository credential from `GET /api/v1/workers/{id}/git-credential`, and each request is audited.

## Failures

| What happened | Failure code |
|---|---|
| The image pull fails | `sandbox_create_failed` |
| The guest runs out of memory | `out_of_memory` |
| The host reboots, or the VM is lost | `sandbox_lost`, with a retry |
| The deadline passes | `infrastructure_timeout` |
| The disk is full at assignment | `resource_unavailable` |

## Trust model

The owner of the host can read the secrets of every run on that host. Enroll only hosts that you trust with the secrets of the factories that use them. A microVM isolates the agent from the host and from other VMs. It does not hide the run from the host owner.

A revoked worker cannot connect and cannot get git credentials.

## Development process worker

For development, `openfactory-runtime worker --process` runs sessions as host processes with no isolation. See [Workers](self-hosting/workers.md).
