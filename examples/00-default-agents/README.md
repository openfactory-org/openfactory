# 00 - Default agents

The agents that every new factory starts with. `openfactory factory create`
without `--from`, and the dashboard, write these files. The binary embeds them
from `internal/definition/defaults/agents`.

## What it shows

| Agent | Type | Model | Responsibility |
| --- | --- | --- | --- |
| `foreman` | `FOREMAN` | `claude-opus-5-5` | Owns each work item. Dispatches stage agents with `run_agents`, asks humans, applies the spec-approval policy and hands back the pull request. Never merges. |
| `triage` | `TRIAGE` | `claude-sonnet-5-5` | Researches and reproduces. Reports scope, complexity, evidence and open questions. |
| `spec` | `SPEC` | `claude-opus-5-5` | Writes product and technical specs with validation criteria in a draft pull request. |
| `implement` | `IMPLEMENT` | `claude-opus-5-5` | Builds on the spec branch, adds tests, runs checks and captures visual evidence. Never merges. |
| `review` | `REVIEW` | `claude-sonnet-5-5` | Reviews independently and gives an advisory verdict: `accept`, `revise` or `needs-human-decision`. |

The review agent uses a different model from the implement agent, so the two
do not share blind spots. For more independence, give it a different harness
(see [03-multi-harness](../03-multi-harness/)).

## Prerequisites

- A running server, with `OF_SERVER` and `OF_API_KEY` set
  (see [Quickstart](../../docs/quickstart.md)).
- The model credential of the `claude` harness:

  ```bash
  openfactory secret set ANTHROPIC_API_KEY --type anthropic_api_key
  ```

- A repository that the factory can write to. In `factory.yaml`, under
  `repositories`, change `owner: acme` and `name: web-app` to your repository.

## Apply

```bash
openfactory validate examples/00-default-agents
openfactory factory apply examples/00-default-agents
```

## What to expect

`validate` reports 5 agents. `apply` creates the factory `@default-agents`, or
adds a new version when it exists. Send it work with
`openfactory factory run default-agents "<prompt>"`.

## Customize

| Change | Where |
| --- | --- |
| Spec approval. The foreman waits for human approval of every spec. | The "Spec approval policy" section of `agents/foreman/agent.md`. |
| Default model of the factory. | `agentDefaults.harness.model` in `factory.yaml`. |
| Model of one agent. | `model` in the frontmatter of its `agent.md`. |
| Fewer agents. | Delete the agent directories. Keep exactly one `FOREMAN`. |
