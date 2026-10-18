# Changelog

All notable changes to this project are recorded in this file.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/). The project uses [Semantic Versioning](https://semver.org/spec/v2.0.0.html). Releases are `v0.x` until `v1.0.0` freezes the public contracts. Before `v1.0.0`, a minor release can contain breaking changes. See [GOVERNANCE.md](GOVERNANCE.md#breaking-changes).

## [Unreleased]

First public release.

### Added

- `openfactory`, one Go binary: the server (REST API, dashboard, engine and dispatcher), remote workers, local definition tools (`init`, `validate`, `schema`) and client commands.
- Factories declared as code in a definition tree rooted at `factory.yaml` (definition schema `v1`), with JSON Schemas and atomic apply.
- A foreman agent that sends each work item through triage, spec, implementation and review agents, and stops for human approval when a decision is needed.
- Harnesses: `claude` (Claude Code CLI) and `codex` (Codex CLI), with a harness and model per agent.
- Work sources: GitHub, Slack, Microsoft Teams, Linear and Jira built in; GitLab and Azure DevOps as gRPC plugins; custom webhooks with signature verification; cron schedules; the REST API; and the Factory MCP server.
- Sandbox backends: `kubernetes` (one Job per sandbox), remote workers over an outbound WebSocket, `local` for development, and plugin sandbox backends.
- Security: OIDC single sign-on, user, editor and admin roles, API keys, encrypted secrets (AES-256-GCM), `{{SECRET}}` templates and network policies.
- Operations: a Helm chart with SQLite (one replica) and PostgreSQL (many replicas) modes, run data in S3, GCS, Azure Blob or local disk, Prometheus metrics and health endpoints.
- Quality: scorers, benchmarks, self-improvement proposals, and dashboard metrics for autonomy, cycle time and cost.
- Interfaces: the dashboard and inbox, the versioned REST API (`/api/v1`) with an OpenAPI spec, a Go SDK and the Factory MCP bridge for local coding agents.

[Unreleased]: https://github.com/openfactory-org/openfactory/commits/main
