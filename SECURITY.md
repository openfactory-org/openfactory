# Security policy

Open Factory runs AI agents that read code, run commands and push branches. Report security problems privately.

## Report a vulnerability

Do not open a public issue, discussion or pull request for a security problem.

1. Go to [github.com/openfactory-org/openfactory/security/advisories/new](https://github.com/openfactory-org/openfactory/security/advisories/new). This is GitHub private vulnerability reporting.
2. Give the affected version or commit, the component (for example `internal/webhooks` or the Helm chart), the steps to reproduce and the impact.
3. If you have a fix, attach it to the advisory, not to a public pull request.

The maintainers handle each report privately and acknowledge it as soon as possible. When a fix is available, they publish an advisory and credit you, unless you ask them not to.

## Supported versions

Releases are `v0.x` until `v1.0.0`. Before `v1.0.0`, only the latest release gets security fixes. Upgrade to the latest release to get a fix.

| Version | Supported |
|---|---|
| Latest `v0.x` release | Yes |
| Earlier releases | No |
| `main` | Yes, fixed before the next release |

## Scope

In scope:

- Authentication and authorization of the REST API, the dashboard, the Factory MCP server and the per-run orchestration MCP server.
- Webhook and integration ingress: signature verification, replay protection and payload handling.
- Secret storage and expansion (`OF_SECRET_KEY`, `{{SECRET}}` templates), and secrets that leak into logs, transcripts or delivery logs.
- Sandbox isolation in the `kubernetes` backend, the remote worker connection, runner session tokens and network policy enforcement.
- The container images and the Helm chart.

Out of scope:

- The `local` backend. It isolates workspaces only, not processes: commands run as the server user on the host. This is documented behavior.
- Prompt injection that stays inside the permissions you gave an agent. Limit agents with network policies, secret scoping and approvals. See [docs/security.md](docs/security.md).
- Findings that need a compromised administrator account or host.
- Vulnerabilities in third-party agent CLIs or model providers. Report these to their maintainers.

## Hardening checklist

- Set a strong `OF_SECRET_KEY` (`openssl rand -base64 32`) and keep it in a secret manager.
- Never set `OF_DEV=true` in production.
- Serve the server over HTTPS only, and set `OF_PUBLIC_URL` to the HTTPS URL.
- If the server is on the public internet, block `/metrics` at the reverse proxy.
- Run agents in the `kubernetes` backend or on remote workers. Use the `local` backend for development and tests only.
- Set `networkPolicy` on factories that handle untrusted input.
- Give each integration token the minimum permissions in [docs/integrations](docs/integrations/README.md).

More detail is in [docs/security.md](docs/security.md).
