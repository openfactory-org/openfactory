# 02 - SDLC: issue to pull request

A software-delivery factory for two repositories: a web app and its API.

## What it shows

```text
factory.yaml                         repositories, network policy, defaults (Linux machine)
agents/{foreman,triage,spec,implement,review}/agent.md   default agents
agents/verify/agent.md               VERIFY agent on a second Linux machine, with a headless browser
agents/implement/skills/run-checks/  skill for one agent, with a supporting script
skills/repository-conventions/       skill shared by every agent
automations/issue-intake/            issue labeled factory-ready -> foreman
automations/review-feedback/         human "changes requested" reviews
machines/linux-build.yaml            factory default
machines/linux-verify.yaml           Playwright image, 4 vCPU / 8 GB
scorers/tests-run, scorers/spec-quality
benchmarks/implement-quality/        two configurations, two tasks
```

- Both machines are Linux machines. Sandboxes run Linux only. The `verify`
  agent overrides the default with `machine: linux-verify`.
- The machine sizes (4 vCPU, 8 GB) fit every hosted
  [sandbox provider](../../docs/sandbox-providers.md#machine-sizes).
- The network policy extends `builtin/package-registries@^1` and allows the
  internal API `api.acme.dev`. A denial at factory or agent scope always wins.
  Use a provider or worker that enforces policies (see
  [Network policy](../../docs/self-hosting/network-policy.md)).
- Factory-wide `secrets` give `NPM_TOKEN` to every agent.
- `selfImprovement` turns repeated `tests-run` failures into pull requests
  that propose definition or skill changes. An admin reviews them.

## Prerequisites

- A running control plane, with `OF_SERVER` and `OF_API_KEY` set
  (see [Quickstart](../../docs/quickstart.md)).
- The GitHub integration, with webhooks, for both repositories
  (see [GitHub](../../docs/integrations/github.md)).
- The managed secrets:

  ```bash
  openfactory secret set ANTHROPIC_API_KEY --type anthropic_api_key
  openfactory secret set NPM_TOKEN
  openfactory secret set OPENAI_API_KEY --type openai_api_key   # only for the benchmark's Codex configuration
  ```

- The machine image of `linux-verify` is a public Playwright image. A hosted
  provider uses the image that you built into it, so build the snapshot from
  an image that has a browser. See
  [Sandbox providers](../../docs/sandbox-providers.md#build-the-sandbox-image).

## Apply

1. Set your repositories:
   - `factory.yaml`: under `repositories`, change the `owner` and `name` of
     both entries.
   - `automations/*/automation.md`: in `filter.repos` of both files,
     change `acme/web-app` and `acme/api-service` to `<owner>/<name>`.
2. Validate and apply:

   ```bash
   openfactory validate examples/02-sdlc-issue-to-pr
   openfactory factory apply examples/02-sdlc-issue-to-pr
   ```

## What to expect

`validate` reports 6 agents, 2 automations, 2 scorers and 1 benchmark.
`apply` creates the factory `@sdlc`. When you label an issue
`factory-ready`, the foreman starts. It dispatches the stage agents, and
`verify` for changes to the user interface, until a reviewed pull request is
ready. When a human requests changes, the foreman continues the work item.
When the pull request merges, the work item completes.
