# Network policy

A network policy limits the hosts that a run can reach from its sandbox.

## Declare a policy

Set `networkPolicy` in `factory.yaml` (factory scope) or in the front matter of an agent's `agent.md` (agent scope).

```yaml
# factory.yaml
networkPolicy:
  extends:
    - builtin/package-registries@^1
  allowedHosts:
    - api.internal.example.com
  deniedHosts:
    - pastebin.com
```

```markdown
---
# agents/implement/agent.md
networkPolicy:
  extends:
    - builtin/claude-code@latest
---
```

| Field | Type | Description |
|---|---|---|
| `extends` | list of strings | Built-in policies, as `<policy-name>@<version-selector>`. |
| `allowedHosts` | list of strings | Extra hosts that the run can reach. |
| `deniedHosts` | list of strings | Hosts that the run cannot reach. |

### Host entries

Each entry is an exact hostname or a literal IP address. Matching is case-insensitive. There is no implicit subdomain match: `example.com` does not allow `api.example.com`. List each host.

| Rejected entry | Error |
|---|---|
| `https://example.com` | must be a hostname or IP address, not a URL |
| `*.example.com` | must not contain wildcards |
| `10.0.0.0/8` | must not be a CIDR range |
| `example.com:443` | must not include a port |

## Merge rules

The effective policy of a run combines the factory scope and the agent scope.

1. If neither scope declares a policy, access is unrestricted.
2. If at least one scope declares a policy, access is restricted: only allowed hosts are reachable.
3. Allowed hosts of both scopes combine, including the hosts from `extends`.
4. A host denied at either scope is removed from the allowed hosts. A denial always wins.
5. A policy with only `deniedHosts` still restricts access. It is not a deny list on top of open access.
6. An empty policy (`networkPolicy: {}`) restricts access and adds no hosts.

### Required hosts

In a restricted policy, Open Factory always adds the hosts that the run needs. `deniedHosts` cannot block them.

| Required host | Source |
|---|---|
| Control plane | The host of `OF_INTERNAL_URL`, else `OF_PUBLIC_URL`, for the runner and the orchestration MCP server. |
| Repository forges | The host of each repository clone URL of the run, for example a GitHub Enterprise Server or self-managed GitLab host. For github.com repositories, also `github.com`, `api.github.com` and `codeload.github.com`. For GitLab.com, `gitlab.com`. For Azure DevOps, `dev.azure.com`. |
| MCP servers | The host of each `url` MCP server, including servers from `managedId`. Proxied and built-in servers resolve to the control plane, which reaches their upstream hosts. |
| Image registry | The registry host of the sandbox image (environment image, then machine image, then `OF_SANDBOX_IMAGE`, then the built-in default). Docker Hub images add `registry-1.docker.io`. |
| Inference | The harness CLI calls the model provider from the sandbox. `claude`: `api.anthropic.com`. `codex`: `api.openai.com`. An `ANTHROPIC_BASE_URL` or `OPENAI_BASE_URL` in the harness credential replaces the default host. Claude Code on Bedrock uses `bedrock-runtime.<AWS_REGION>.amazonaws.com` (default region `us-east-1`), or the host of `ANTHROPIC_BEDROCK_BASE_URL`. |
| Cloud identity | With `cloudProviders`: `sts.amazonaws.com` for AWS; `sts.googleapis.com` and `iamcredentials.googleapis.com` for Google Cloud. |

Other hosts are not added. Add them yourself, for example package registries with `builtin/package-registries`, or the authentication hosts of `builtin/claude-code` and `builtin/codex`.

## Built-in policies

| Policy | Versions | Purpose |
|---|---|---|
| `builtin/package-registries` | `1.0.0`, `1.1.0` | Package registries and source or release downloads of development toolchains. |
| `builtin/claude-code` | `1.0.0`, `1.1.0` | Inference and authentication for Claude Code. |
| `builtin/codex` | `1.0.0` | Inference and authentication for Codex. |

Published versions never change. A new host list gets a new version.

### Version selectors

| Selector | Meaning |
|---|---|
| `1.0.0` | Exactly this version. |
| `^1`, `^1.1`, `^1.0.0` | The highest stable version in the caret range. `^1` matches `>=1.0.0 <2.0.0`. |
| `latest` | The highest stable version. |

An unknown policy name or a selector that matches no version is a definition validation error.

### Host lists

`builtin/package-registries@1.0.0`:
`registry.npmjs.org`, `registry.yarnpkg.com`, `repo.yarnpkg.com`, `nodejs.org`, `pypi.org`, `files.pythonhosted.org`, `pypi.python.org`, `crates.io`, `index.crates.io`, `static.crates.io`, `static.rust-lang.org`, `sh.rustup.rs`, `rubygems.org`, `index.rubygems.org`, `proxy.golang.org`, `sum.golang.org`, `go.dev`, `dl.google.com`, `repo.maven.apache.org`, `repo1.maven.org`, `plugins.gradle.org`, `services.gradle.org`, `downloads.gradle.org`, `api.nuget.org`, `packagist.org`, `repo.packagist.org`, `hex.pm`, `repo.hex.pm`, `github.com`, `codeload.github.com`, `objects.githubusercontent.com`, `raw.githubusercontent.com`, `release-assets.githubusercontent.com`.

`builtin/package-registries@1.1.0` adds:
`jsr.io`, `npm.jsr.io`, `bun.sh`, `deno.land`, `dl.deno.land`, `astral.sh`, `conda.anaconda.org`, `repo.anaconda.com`, `gitlab.com`.

`builtin/claude-code@1.0.0`:
`api.anthropic.com`, `console.anthropic.com`, `statsig.anthropic.com`.

`builtin/claude-code@1.1.0` adds:
`claude.ai`, `platform.claude.com`.

`builtin/codex@1.0.0`:
`api.openai.com`, `auth.openai.com`, `chatgpt.com`.

Get the current catalog from the API:

```bash
curl -sS "${OF_PUBLIC_URL}/api/v1/networking/policies" \
  -H "Authorization: Bearer $OF_API_KEY"
```

## Enforcement

| Backend | Enforcement |
|---|---|
| `local` (development only) | None. |
| `kubernetes` | A NetworkPolicy and an egress proxy sidecar for each restricted sandbox. See [Kubernetes](#kubernetes). |
| remote worker | The same as the worker's backend. |

If the `kubernetes` backend cannot enforce a restricted policy, the run ends with `invalid_configuration`, and the error names the settings to use. `OF_ALLOW_UNENFORCED_NETWORK_POLICY=true` starts the run with a warning instead. Set it only if another control enforces egress for sandbox Pods.

### Egress proxy

A NetworkPolicy cannot match host names, so the `kubernetes` backend enforces `allowedHosts` and `deniedHosts` with `openfactory netguard`, an HTTP forward proxy for plain HTTP and `CONNECT` tunnels. It runs as a sidecar in each restricted sandbox Pod (see [Proxy sidecar for each sandbox](#proxy-sidecar-for-each-sandbox)).

The proxy applies these rules in order:

1. The control plane and the other required hosts are always allowed.
2. A denied host is blocked, even when it is also allowed.
3. Only allowed hosts are reachable.

A blocked request gets HTTP status `403` with the body `Open Factory network policy blocked access to <host>`.

> **Important:** The proxy controls only programs that honour the proxy variables. A program that opens raw sockets bypasses it on ports 80 and 443. The NetworkPolicy blocks all other ports. To also close ports 80 and 443, turn on the [egress lock](#egress-lock-optional).

### Kubernetes

Each restricted sandbox gets a NetworkPolicy named after its Job. It selects the Pod by the label `openfactory.dev/sandbox` and allows egress only to:

| Destination | Rule |
|---|---|
| DNS | Port 53 over UDP and TCP, to Pods in any namespace. |
| Control plane | The Pods that match `OF_K8S_CONTROL_PLANE_SELECTOR` in `OF_K8S_CONTROL_PLANE_NAMESPACE`. Without a selector: the IP address of the control URL, only if its host is an IP address. |
| Ports 80 and 443 | TCP ports 80 and 443 to any address, for the proxy sidecar. Only if the Pod has the sidecar. |

When the control URL host is a DNS name (for example the server Service), the NetworkPolicy needs the selector. Otherwise it has no control plane rule. The selector rule allows all ports of the selected Pods, so it also matches after a Service forwards port 80 to the container port. The Helm chart sets the selector to the server Pods of the release by default. See [Kubernetes: Network policy](kubernetes.md#network-policy) for the default and the override (`sandbox.k8s.controlPlane`).

The Job owns the NetworkPolicy, so it goes away with the Job (the backend also deletes it). An unrestricted sandbox gets none.

- **Without the sidecar image (`OF_K8S_EGRESS_PROXY_SIDECAR_IMAGE`), a restricted sandbox can reach DNS and the control plane only.** This blocks forges, registries and inference. The Helm chart sets the image to the runner image by default.
- The cluster network plugin (CNI) must enforce NetworkPolicy. Otherwise the objects exist but have no effect. See [Check that the cluster enforces NetworkPolicy](#check-that-the-cluster-enforces-networkpolicy).
- The Role needs `create`, `get`, `patch` and `delete` for `networkpolicies`. The Helm chart adds them.

If the backend cannot create the NetworkPolicy, the run ends with `invalid_configuration`, unless `OF_ALLOW_UNENFORCED_NETWORK_POLICY=true`.

#### Check that the cluster enforces NetworkPolicy

If the network plugin does not enforce NetworkPolicy, the cluster accepts the object and a restricted run has no limit. At startup, the backend tests this in the background, in the sandbox namespace, with no extra permission:

1. It starts a target Pod that listens on a TCP port.
2. It creates a NetworkPolicy that denies all egress for Pods with the role `denied`.
3. It starts two client Pods. The `control` client has no policy and must connect. The `denied` client has the deny policy. It waits 3 seconds, then tries to connect.
4. It deletes all Pods and the policy.

| Result | Meaning |
|---|---|
| `enforced` | The `denied` client could not connect. The server logs this at info level. |
| `not_enforced` | The `denied` client connected. The server logs an error. |
| no answer | The test could not finish, for example the image does not pull or the `control` client could not connect. The server logs a warning and tries again when a restricted run needs the answer, at most every 5 minutes. |

The probe Pods run `openfactory netprobe` from `OF_K8S_EGRESS_PROXY_SIDECAR_IMAGE`, else `OF_RUNNER_IMAGE` (no image: no check). They run as user 65532 without capabilities, with a read-only root file system, and copy the node selector, tolerations, affinity, runtime class, priority class and `imagePullSecrets` of the pod template. A leftover probe Pod ends by itself after 5 minutes.

| Setting | Values | Effect |
|---|---|---|
| `OF_K8S_NETWORK_POLICY_CHECK` | `probe` (default), `off` | `off` does not run the probe. |
| `OF_K8S_NETWORK_POLICY_ENFORCEMENT` | `warn` (default), `require` | `warn`: a restricted run always starts. After `not_enforced`, the server logs a warning for each restricted run. `require`: a restricted run starts only after `enforced`, else it ends with `invalid_configuration`. `require` needs `OF_K8S_NETWORK_POLICY_CHECK=probe`. |

`OF_ALLOW_UNENFORCED_NETWORK_POLICY=true` overrides `require` and logs a warning. With `require`, the first restricted run waits for a running probe: a few seconds, or up to 3 minutes for an image pull.

Typical causes of `not_enforced`: a plugin without policy support or installed in a mode without it, or a managed cluster with its network policy add-on off.

#### The sandbox does not start before the policy is active

A network plugin applies a NetworkPolicy shortly after a Pod starts (on k3s, a connection at 5 ms passed, one at 250 ms was blocked). A process that connects in that gap is not limited.

To close the gap, a restricted sandbox Pod gets the init container `egress-wait`. It runs before every container with sandbox code: the runner and tool image copies, your pod template's init containers and the sandbox container. It runs `openfactory netprobe wait-blocked`, which dials the kubelet of the node (`status.hostIP`, port 10250). The kubelet always listens and a sandbox NetworkPolicy never allows it, so the dial works until the policy is active. After two failed dials in a row, the init container ends. The plugin limits the whole Pod, so the policy is then active for the sandbox container too.

| Rule | Detail |
|---|---|
| Guarantee | The runner, and so every agent process, starts only after a direct dial to the target fails. |
| Time | At most 30 seconds (`OF_K8S_EGRESS_WAIT_TIMEOUT`). Usually a few hundred milliseconds. |
| Timeout | Exit code 75. The sandbox container does not start. The sandbox ends with `network_policy_timeout`, and the run goes to `error` with that failure code. The platform retries it, at most 3 sessions in sequence. |
| No wait | For an unrestricted sandbox, after a probe result `not_enforced`, or with `OF_ALLOW_UNENFORCED_NETWORK_POLICY=true`. Without a probe answer, the wait runs: the sandbox fails instead of running without a limit. |
| Image | `OF_RUNNER_IMAGE`, else the proxy sidecar image, else the sandbox image. |

A refused connection counts as blocked, because a plugin can reject instead of drop. The target is not port 80 or 443, because the egress lock refuses these ports from the start. The lock does not replace the wait: the wait covers all ports.

Limits of the wait:

- If a node firewall refuses port 10250 from Pods, the wait ends at once and gives no guarantee. Test with `openfactory netprobe dial --addr <node IP>:10250` in a Pod without a policy: it must connect.
- A plugin that always allows Pod-to-node traffic never blocks the target. Every restricted sandbox then ends with `network_policy_timeout` after 30 seconds.

| Setting | Default | Effect |
|---|---|---|
| `OF_K8S_EGRESS_WAIT_TIMEOUT` | `30s` | Time of the wait, for example `60s`. At least `1s`. `off` turns the wait off. |
| `OF_K8S_EGRESS_WAIT_ADDR` | the kubelet of the node, port `10250` | `host:port` that the wait dials. The server refuses other formats. |

A custom target must be reachable without a policy and blocked by the sandbox NetworkPolicy: not DNS, not the control plane, not port 80 or 443. Test it with `openfactory netprobe dial --addr <host:port>` in a Pod without a policy.

> **Warning:** With `OF_K8S_EGRESS_WAIT_TIMEOUT=off`, a process that connects before the plugin applies the policy is not limited. Turn the wait off only if the plugin applies the policy before the Pod starts, or another control limits egress. `OF_ALLOW_UNENFORCED_NETWORK_POLICY=true` also turns the wait off; use it only for a cluster that does not enforce policies.

#### Proxy sidecar for each sandbox

Set `OF_K8S_EGRESS_PROXY_SIDECAR_IMAGE` to an image with the `openfactory` binary. The Helm chart uses the runner image by default. Each restricted sandbox Pod then gets the sidecar `egress-proxy`, which runs `openfactory netguard` with the host lists of the sandbox.

The sidecar is a native sidecar (an init container with `restartPolicy: Always`, Kubernetes 1.29 or later). The sandbox container starts after the proxy listens, and the Job completes when the sandbox container ends.

A restricted sandbox container gets these variables:

| Variable | Value |
|---|---|
| `HTTPS_PROXY`, `HTTP_PROXY`, `https_proxy`, `http_proxy` | `http://127.0.0.1:3128`, the sidecar. |
| `NO_PROXY`, `no_proxy` | The host of the control URL, `localhost` and `127.0.0.1`. |
| `OF_NETGUARD_REPORT` | The URL of the report of blocked hosts. See [Blocked hosts in the diagnostics of the run](#blocked-hosts-in-the-diagnostics-of-the-run). |

An unrestricted sandbox gets no sidecar and none of these variables.

A NetworkPolicy cannot select one container, so ports 80 and 443 are also open for the sandbox container. A program that ignores the proxy variables can connect directly on them. To close this gap, turn on the [egress lock](#egress-lock-optional).

#### Blocked hosts in the diagnostics of the run

The sidecar serves a report of blocked hosts at `http://127.0.0.1:3129/blocked`, reachable only inside the Pod (`OF_NETGUARD_REPORT`). The runner reads it every 5 seconds and when the session ends, so the sandbox image needs no `curl`. For each blocked host, the runner sends one diagnostic `network/egress_blocked` with `data.blocked_host` and `data.count`. A diagnostic is a warning, not a result, and does not appear in the transcript. Read it with `GET /api/v1/runs/{runId}/diagnostics`.

#### Egress lock (optional)

The lock is an optional hardening step. It closes ports 80 and 443 for every process of the sandbox except the proxy.

With `OF_K8S_EGRESS_LOCK=iptables`, a restricted sandbox Pod gets the init container `egress-lock`, which runs first. It adds a firewall rule to the Pod network (and an `ip6tables` rule for IPv6): a TCP connection to port 80 or 443 is refused unless its process runs as the proxy user, 65532. If the sandbox user or a template `runAsUser` is 65532, the proxy uses the next free user, for example 65533. A program that ignores the proxy variables gets `connection refused`. The rule goes away with the Pod and does not change the node or other Pods.

The lock needs a numeric sandbox user. The server does not start with `OF_K8S_EGRESS_LOCK=iptables` and `OF_K8S_RUN_AS_USER=image`, and a restricted run with `platform.linux.user: image` fails. The sandbox container always gets `runAsNonRoot: true` and no capabilities, so it cannot switch to the proxy user.

| Requirement | Reason |
|---|---|
| `OF_K8S_EGRESS_PROXY_SIDECAR_IMAGE` | The lock works with the sidecar. The setting is not valid without it. |
| `sh`, `grep`, `iptables` and `ip6tables` in the sidecar image | The lock container uses that image. The `openfactory` image of this project has them. In your own image, add the package `iptables`. |
| The capabilities `NET_ADMIN` and `NET_RAW` for the init container | The container runs as root with only these two capabilities and no privilege escalation. The Pod Security levels `baseline` and `restricted` refuse `NET_ADMIN`. Set the level `privileged` on the sandbox namespace, or add an exception for the container `egress-lock`. |
| A non-root sandbox container | A root process could switch to the proxy user. Use a numeric `OF_K8S_RUN_AS_USER` (default 1000), not 65532. |
| Linux nodes with `iptables` and its `owner` match | This is the normal case. |

If the control URL uses port 80 or 443 (for example `https://openfactory.example.com`), the sandbox reaches the control plane through the proxy: the backend always allows its host in the proxy and removes it from `NO_PROXY`. A program that ignores the proxy variables then cannot reach the control plane. On another port, for example `http://openfactory.ns.svc:8080`, the sandbox connects directly.

Seeing the rule from inside a Pod needs `NET_ADMIN`. Read `kubectl logs <pod> -c egress-lock` instead: a failed init container shows the `iptables` error.

#### A restricted run does not start

| Message | Cause |
|---|---|
| `create network policy for restricted task` | The Role cannot create NetworkPolicy objects. Add `networkpolicies` to the Role. |
| `the cluster does not enforce NetworkPolicy` | `require`, and the probe found `not_enforced`. Install a plugin that enforces policies. See [Check that the cluster enforces NetworkPolicy](#check-that-the-cluster-enforces-networkpolicy). |
| `it is not known that the cluster enforces NetworkPolicy` | `require`, and the probe had no answer. The error names the reason, for example an image that does not pull. |
| The run ends with `network_policy_timeout` | `egress-wait` could still reach the kubelet after 30 seconds: the plugin applied the policy too late, or not at all. Read `kubectl logs <pod> -c egress-wait`. See [The sandbox does not start before the policy is active](#the-sandbox-does-not-start-before-the-policy-is-active). |
| `egress-lock` init container fails | `OF_K8S_EGRESS_LOCK=iptables`, and the namespace refuses `NET_ADMIN`, or the image has no `iptables`. Read `kubectl logs <pod> -c egress-lock`. |

#### Extra rules

Kubernetes combines all policies that select a Pod, so your own NetworkPolicy in the sandbox namespace can only allow more.

This example allows DNS and HTTPS for all Pods that Open Factory manages. **It also opens port 443 for restricted runs**, so use it only when no run has a restricted policy:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: factory-sandbox-egress
  namespace: openfactory-sandboxes
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/managed-by: openfactory
  policyTypes: ["Egress"]
  egress:
    - ports:
        - protocol: UDP
          port: 53
        - protocol: TCP
          port: 53
    - ports:
        - protocol: TCP
          port: 443
```

Make sure that sandbox Pods can still reach `OF_INTERNAL_URL` (or `OF_PUBLIC_URL`), your forges and your registries.

## Related pages

- [Definition reference](../definition-reference.md)
- [Agents](../agents.md)
- [Security](../security.md)
- [Troubleshooting](../troubleshooting.md#network-access-is-blocked)
