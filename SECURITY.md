# Security policy

Open Factory runs AI agents that read code, run commands and push branches. Report security problems privately.

## Report a vulnerability

Do not open a public issue, discussion or pull request for a security problem.

1. Go to [github.com/openfactory-org/openfactory/security/advisories/new](https://github.com/openfactory-org/openfactory/security/advisories/new). This is GitHub private vulnerability reporting.
2. Give the affected version or commit, the component (for example `apps/control-plane/src/webhooks` or the sandbox image), the steps to reproduce and the impact.
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
- Sandbox isolation and egress control: the in-sandbox lock, the egress proxy, the capability gate, the microVM worker (jailer, network rules, the guest init) and the worker connection and enrollment.
- Runner session tokens, and the isolation of a run from other runs and from the provider account.
- The sandbox image and the install files for worker hosts.

Out of scope:

- The development process worker. It runs agents as host processes with no isolation. This is documented behavior.
- Egress control that a hosted provider does not offer. A provider with `egress: none` cannot enforce a network policy. This is documented behavior.
- The host owner of a self-hosted worker reading the secrets of the runs on that host. This is the trust model of a worker.
- Prompt injection that stays inside the permissions you gave an agent. Limit agents with network policies, secret scoping and approvals. See [docs/security.md](docs/security.md).
- Findings that need a compromised administrator account or host.
- Vulnerabilities in third-party agent CLIs, sandbox providers or model providers. Report these to their maintainers.

## Hardening checklist

- Set strong `OF_SECRET_KEY` and `OF_SIGNING_KEY` Worker secrets (`openssl rand -base64 32`) and keep copies in a secret manager.
- Never set `OF_DEV_PROCESS_WORKERS=true` in production.
- Serve the control plane over HTTPS only, and set `OF_PUBLIC_URL` to the HTTPS URL.
- Run agents on a hosted provider or on microVM workers. Leave `OF_ALLOW_UNENFORCED_NETWORK_POLICY` off.
- Enroll workers only on hosts that you trust with the secrets of their runs, and revoke workers that you no longer use.
- Set `networkPolicy` on factories that handle untrusted input, on a provider that enforces it.
- Give each integration token the minimum permissions in [docs/integrations](docs/integrations/README.md).

More detail is in [docs/security.md](docs/security.md).
