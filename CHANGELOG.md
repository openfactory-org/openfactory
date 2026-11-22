# Changelog

All notable changes to this project are recorded in this file.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/). The project uses [Semantic Versioning](https://semver.org/spec/v2.0.0.html). Releases are `v0.x` until `v1.0.0` freezes the public contracts. Before `v1.0.0`, a minor release can contain breaking changes. See [GOVERNANCE.md](GOVERNANCE.md#breaking-changes).

## [Unreleased]

### Changed

- The control plane is rewritten in TypeScript and runs on Cloudflare: one Worker with Durable Objects (SQLite storage), an R2 bucket, a Queue, a Cron trigger and Static Assets for the dashboard. State moves from SQLite or PostgreSQL to Durable Objects. Deploy with `wrangler deploy`.
- The Go program is now `openfactory-runtime`: the runner, the egress proxy, the development process worker and the microVM worker agent. The name `openfactory` is the npm command-line client.
- Sandboxes run on hosted providers (`cloudflare-sandbox`, `daytona`, `boatdev`) and on self-hosted Linux hosts with Firecracker microVMs. Enrolled hosts are workers (`/api/v1/workers`, worker protocol v2). Egress control on hosted providers is best effort, and a provider that cannot enforce a policy declares `egress: none`.
- `workerHost` accepts `server` (the team's default provider), a provider ID, a worker ID or name, or `pool:<label>`.
- Plugins (GitLab, Azure DevOps) are separate Workers over service bindings, in place of gRPC executables.
- Sandboxes run Linux only.
- The Factory MCP bridge is `openfactory mcp` of the npm client. `POST /api/v1/secrets/rewrap` replaces the database command for key rotation.

### Added

- `docs/sandbox-providers.md`, `docs/self-hosted-machines.md`, `docs/cli.md` and `docs/self-hosting/cloudflare.md`.
- A git mirror cache and snapshot restore for microVM workers.
- The `@openfactory/api-client` TypeScript client and the `@openfactory/control-plane` builder package for custom control plane builds.

### Removed

- Kubernetes support, the Helm chart, AWS support (including the cloud identity provider `aws` and the Bedrock and ECR secret types), PostgreSQL and SQLite stores, the tool images, the Go SDK, the gRPC plugin host and `pkg/`.
- macOS agents and `openfactory serve`.

### Earlier history

- Factories declared as code in a definition tree rooted at `factory.yaml` (definition schema `v1`), with JSON Schemas and atomic apply.
- A foreman agent that sends each work item through triage, spec, implementation and review agents, and stops for human approval when a decision is needed.
- Harnesses: `claude` (Claude Code CLI) and `codex` (Codex CLI), with a harness and model per agent.
- Work sources: GitHub, Slack, Microsoft Teams, Linear and Jira built in; GitLab and Azure DevOps as plugins; custom webhooks with signature verification; cron schedules; the REST API; and the Factory MCP server.
- Security: OIDC single sign-on, user, editor and admin roles, API keys, encrypted secrets (AES-256-GCM), `{{SECRET}}` templates and network policies.
- Quality: scorers, benchmarks, self-improvement proposals, and dashboard metrics for autonomy, cycle time and cost.
- Interfaces: the dashboard and inbox, the versioned REST API (`/api/v1`) with an OpenAPI spec and the Factory MCP bridge for local coding agents.

[Unreleased]: https://github.com/openfactory-org/openfactory/commits/main
