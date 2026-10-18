# Examples

## Factory definitions

Each directory holds a complete definition tree with a `factory.yaml`.
`go test ./internal/definition/` compiles every one of them.

| Example | Shows |
| --- | --- |
| [00-default-agents](00-default-agents/) | The agents that every new factory starts with. |
| [01-single-repo-quickstart](01-single-repo-quickstart/) | One repository, one foreman, one label automation. |
| [02-sdlc-issue-to-pr](02-sdlc-issue-to-pr/) | Issue to reviewed pull request: skills, machines, scorers, a benchmark. |
| [03-multi-harness](03-multi-harness/) | Claude Code and Codex agents in one factory. |
| [04-code-review-only](04-code-review-only/) | Pull request review without code changes. |
| [05-incident-response](05-incident-response/) | Custom webhooks, MCP servers and Slack reports. |
| [06-common-automations](06-common-automations/) | Slack, Linear, schedule and GitHub triggers. |
| [07-self-hosted-worker](07-self-hosted-worker/) | Runs on a self-hosted worker, with GitLab. |

To try one, copy it, change the repositories, then run:

```bash
openfactory validate <dir>
openfactory factory apply <dir>
```

## Go modules

These are separate Go modules. A `replace` directive in each `go.mod` builds
them against this checkout.

| Example | Shows | Build |
| --- | --- | --- |
| `custom-binary/` | A custom server binary on `pkg/openfactory`: a read-only role policy, a JSON audit sink and an extra HTTP route. | `make example-custom-binary` |
| `plugin-hello/` | A plugin on `pkg/plugin/sdk`: a responder and a webhook ingress. | `make example-plugin-hello` |

`make examples-go` builds both. Run the plugin with
`OF_PLUGIN_DIR=bin/example-plugins openfactory serve`.
