# 02 - SDLC: issue to pull request

A software-delivery factory for two repositories: a web app and an iOS app.

## What it shows

```text
factory.yaml                         repositories, network policy, defaults (Linux machine)
agents/{foreman,triage,spec,implement,review}/agent.md   default agents
agents/verify/agent.md               VERIFY agent on the macOS machine
agents/implement/skills/run-checks/  skill for one agent, with a supporting script
skills/repository-conventions/       skill shared by every agent
automations/issue-intake/            issue labeled factory-ready -> foreman
automations/review-feedback/         human "changes requested" reviews
machines/linux-build.yaml            factory default (must be Linux)
machines/macos-xcode.yaml            macOS 26, 8 vCPU / 14 GB
scorers/tests-run, scorers/spec-quality
benchmarks/implement-quality/        two configurations, two tasks
```

- `agentDefaults.machine` must be a Linux machine. The `verify` agent
  overrides it with `machine: macos-xcode`.
- The network policy extends `builtin/package-registries@^1` and allows the
  internal API `api.acme.dev`. A denial at factory or agent scope always wins.
- Factory-wide `secrets` give `NPM_TOKEN` to every agent.
- `selfImprovement` turns repeated `tests-run` failures into pull requests
  that propose definition or skill changes. An admin reviews them.

## Prerequisites

- A running server, with `OF_SERVER` and `OF_API_KEY` set
  (see [Quickstart](../../docs/quickstart.md)).
- The GitHub integration, with webhooks, for both repositories
  (see [GitHub](../../docs/integrations/github.md)).
- The managed secrets:

  ```bash
  openfactory secret set ANTHROPIC_API_KEY --type anthropic_api_key
  openfactory secret set NPM_TOKEN
  openfactory secret set OPENAI_API_KEY --type openai_api_key   # only for the benchmark's Codex configuration
  ```

- For the `verify` agent, a Mac worker. Open Factory creates only Linux
  sandboxes. The `macos-xcode` machine declares the platform; it does not
  create a Mac. Do these steps:
  1. On a Mac, start a worker:
     `openfactory worker --backend local --id mac-mini-01`.
  2. Add `workerHost: mac-mini-01` to `agents/verify/agent.md`.

  Without a Mac worker, the `verify` agent runs in a Linux sandbox and its
  Xcode commands fail. See
  [Machines and environments](../../docs/machines-and-environments.md).

## Apply

1. Set your repositories:
   - `factory.yaml`: under `repositories`, change the `owner` and `name` of
     both entries.
   - `automations/*/automation.md`: in `filter.repos` of both files,
     change `acme/web-app` and `acme/ios-app` to `<owner>/<name>`.
2. Validate and apply:

   ```bash
   openfactory validate examples/02-sdlc-issue-to-pr
   openfactory factory apply examples/02-sdlc-issue-to-pr
   ```

## What to expect

`validate` reports 6 agents, 2 automations, 2 scorers and 1 benchmark.
`apply` creates the factory `@sdlc`. When you label an issue
`factory-ready`, the foreman starts. It dispatches the stage agents, and
`verify` for iOS changes, until a reviewed pull request is ready. When a human
requests changes, the foreman continues the work item. When the pull request
merges, the work item completes.
