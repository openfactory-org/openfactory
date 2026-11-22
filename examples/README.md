# Examples

## Factory definitions

Each directory holds a complete definition tree with a `factory.yaml`.
`openfactory validate examples/*/` checks every one of them (CI runs it as
`npm run validate-examples`).

| Example | Shows |
| --- | --- |
| [00-default-agents](00-default-agents/) | The agents that every new factory starts with. |
| [01-single-repo-quickstart](01-single-repo-quickstart/) | One repository, one foreman, one label automation. |
| [02-sdlc-issue-to-pr](02-sdlc-issue-to-pr/) | Issue to reviewed pull request: skills, machines, scorers, a benchmark. |
| [03-multi-harness](03-multi-harness/) | Claude Code and Codex agents in one factory. |
| [04-code-review-only](04-code-review-only/) | Pull request review without code changes. |
| [05-incident-response](05-incident-response/) | Custom webhooks, MCP servers and Slack reports. |
| [06-common-automations](06-common-automations/) | Slack, Linear, schedule and GitHub triggers. |
| [07-self-hosted-worker](07-self-hosted-worker/) | Runs on a self-hosted microVM worker, with GitLab. |

To try one, copy it, change the repositories, then run:

```bash
openfactory validate <dir>
openfactory factory apply <dir>
```

## TypeScript packages

These are small npm packages in this repository.

| Example | Shows |
| --- | --- |
| [plugin-worker](plugin-worker/) | A minimal plugin Worker: a responder and a webhook ingress. See [Plugins](../docs/self-hosting/plugins.md). |
| [control-plane-builder](control-plane-builder/) | A custom control plane Worker that imports `@openfactory/control-plane` and adds a route and a Cron hook. |
