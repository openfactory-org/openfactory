# 01 - Single-repository quickstart

The smallest useful factory. The [Quickstart](../../docs/quickstart.md) uses it.

## What it shows

- One repository.
- One foreman agent that does the work itself.
- One automation that starts work when an issue gets the label
  `factory-ready`.

```text
factory.yaml
agents/foreman/agent.md
automations/labeled-issue/automation.md
```

## Prerequisites

- A running server, with `OF_SERVER` and `OF_API_KEY` set.
- The GitHub integration for your team, with webhooks
  (see [GitHub](../../docs/integrations/github.md)).
- The model credential of the `claude` harness:

  ```bash
  openfactory secret set ANTHROPIC_API_KEY --type anthropic_api_key
  ```

## Apply

1. Set your repository in two files:
   - `factory.yaml`: under `repositories`, change `owner: acme` and
     `name: payments-service`.
   - `automations/labeled-issue/automation.md`: in `filter.repos`, change
     `acme/payments-service` to `<owner>/<name>`.
2. Validate and apply:

   ```bash
   openfactory validate examples/01-single-repo-quickstart
   openfactory factory apply examples/01-single-repo-quickstart
   ```

## What to expect

`apply` creates the factory `quickstart`. When you label an issue
`factory-ready`, a run starts. The dashboard shows it on the **Work** page.

To grow past one agent, add the stage agents from
[00-default-agents](../00-default-agents/). Keep `agentType: FOREMAN` on
exactly one agent.
