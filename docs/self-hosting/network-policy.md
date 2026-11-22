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
| Control plane | The host of `OF_PUBLIC_URL`, for the runner and the orchestration MCP server. |
| Run data storage | The R2 S3 host `<account>.r2.cloudflarestorage.com`, built from `OF_R2_ACCOUNT_ID`. Without it, a restricted run cannot upload its workspace snapshots. |
| Repository forges | The host of each repository clone URL of the run, for example a GitHub Enterprise Server or self-managed GitLab host. For github.com repositories, also `github.com`, `api.github.com` and `codeload.github.com`. For GitLab.com, `gitlab.com`. For Azure DevOps, `dev.azure.com`. |
| MCP servers | The host of each `url` MCP server, including servers from `managedId`. Proxied and built-in servers resolve to the control plane, which reaches their upstream hosts. |
| Inference | The harness CLI calls the model provider from the sandbox. `claude`: `api.anthropic.com`. `codex`: `api.openai.com`. An `ANTHROPIC_BASE_URL` or `OPENAI_BASE_URL` in the harness credential replaces the default host. |
| Cloud identity | With `cloudProviders`: `sts.googleapis.com` and `iamcredentials.googleapis.com` for Google Cloud. |

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

Each sandbox provider enforces a restricted policy in its own way. A provider states what it can do as its `egress` capability: `native`, `in-sandbox` or `none`. See [Sandbox providers](../sandbox-providers.md) for the value of each provider.

| `egress` | Enforcement | Providers |
|---|---|---|
| `native` | The platform blocks all hosts that the policy does not allow, outside the sandbox. | `cloudflare-sandbox` (the provider Worker intercepts all outbound HTTP and HTTPS), `daytona` (domain allow list, when the policy fits), microVM workers (host firewall and a proxy for each VM) |
| `in-sandbox` | The runner locks the network inside the sandbox before it starts the agent. See [In-sandbox lock](#in-sandbox-lock). | `boatdev`, and `daytona` when its allow list cannot hold the policy |
| `none` | Nothing. The provider cannot limit egress. | Development process workers, and a provider that has no mechanism |

Egress control on hosted providers is best effort: Open Factory uses what each provider offers.

### The capability gate

When a run has a restricted policy and its target declares `egress: none`, the dispatcher refuses the run with the failure code `invalid_configuration`. The error names the target. Set the control-plane variable `OF_ALLOW_UNENFORCED_NETWORK_POLICY=true` to start such runs with a warning instead. Set it only if another control limits egress for that target.

A policy that has IP literals is refused on `cloudflare-sandbox`, because its intercept matches host names. Use host names, or choose another provider.

### Egress proxy

A network rule cannot match host names for HTTPS, so the runtime enforces `allowedHosts` and `deniedHosts` with `openfactory-runtime netguard`, an HTTP forward proxy for plain HTTP and `CONNECT` tunnels. The runner starts it as the user `ofproxy`. On a microVM worker, the host starts one for each VM.

The proxy applies these rules in order:

1. The control plane and the other required hosts are always allowed.
2. A denied host is blocked, even when it is also allowed.
3. Only allowed hosts are reachable.

The proxy checks the address of each connection when it dials. It refuses private, link-local, CGNAT, cloud metadata and other reserved addresses, so an allowed host name that resolves to such an address is blocked.

A blocked request gets HTTP status `403` with the body `Open Factory network policy blocked access to <host>`.

The sandbox gets these variables when the policy is restricted:

| Variable | Value |
|---|---|
| `HTTPS_PROXY`, `HTTP_PROXY`, `https_proxy`, `http_proxy` | The address of the proxy. |
| `NO_PROXY`, `no_proxy` | `localhost` and `127.0.0.1`. |
| `OF_NETGUARD_REPORT` | The URL of the report of blocked hosts. See [Blocked hosts in the diagnostics of the run](#blocked-hosts-in-the-diagnostics-of-the-run). |

An unrestricted sandbox gets none of these variables.

> **Important:** The proxy controls only programs that honour the proxy variables. On `in-sandbox` and microVM targets, a lock closes every other path. On `native` targets, the platform closes it.

### In-sandbox lock

The in-sandbox lock is for providers that cannot filter egress themselves. The runner starts as root and does this before it starts the agent:

1. It creates the users `ofproxy` and `agent` (uid 1001).
2. It starts the proxy as `ofproxy`.
3. It adds `iptables` and `ip6tables` OUTPUT rules. `ofproxy` may go out. `agent` may use loopback, except the daemon ports of the provider. Every other TCP or UDP connection from `agent` is rejected.
4. It probes a direct connection, a connection by IP address and the daemon ports as `agent`. If a probe connects, the run fails and does not start the agent.
5. It starts the harness as `agent`. The runner never runs git as root in the workspace.

The agent can use loopback, for example to run a test server. The sandbox image has no `sudo`, so the agent cannot change the rules. The agent can reach only the proxy and its own loopback.

A provider whose sandbox cannot run `iptables` as root declares `egress: none`.

### MicroVM workers

A microVM worker adds host firewall rules (`nftables`) for each VM. With any policy, the host drops VM-to-VM traffic, VM-to-host traffic (except the proxy port), private ranges, cloud metadata addresses and all IPv6. With a restricted policy, the VM has no route and no DNS: it reaches the internet only through the proxy. With no policy, the VM gets NAT and a DNS relay. See [Self-hosted workers](../self-hosted-machines.md#network).

### Blocked hosts in the diagnostics of the run

The proxy serves a report of blocked hosts. The runner reads it every 5 seconds and when the session ends (`OF_NETGUARD_REPORT`). For each blocked host, the runner sends one diagnostic `network/egress_blocked` with `data.blocked_host` and `data.count`. A diagnostic is a warning, not a result, and does not appear in the transcript. Read it with `GET /api/v1/runs/{runId}/diagnostics`.

### A restricted run does not start

| Symptom | Cause |
|---|---|
| `invalid_configuration` naming the target | The target has `egress: none`. Choose another provider, or set `OF_ALLOW_UNENFORCED_NETWORK_POLICY=true`. |
| `invalid_configuration`, "IP literals" | The policy lists an IP address and the target is `cloudflare-sandbox`. List host names. |
| The run fails before the agent starts, with a lock probe error | The in-sandbox lock did not close a path. Check that the sandbox image is the current one and that the provider allows `iptables` for root. |

## Related pages

- [Definition reference](../definition-reference.md)
- [Agents](../agents.md)
- [Security](../security.md)
- [Troubleshooting](../troubleshooting.md#network-access-is-blocked)
