# 04 - Code review only

A factory that never writes code. It reviews each pull request that is opened
or marked ready against `main`, and reviews again on request.

## What it shows

- `automations/pr-opened` targets the `reviewer` agent directly
  (`agent: reviewer`), not the foreman. It uses the native filter key
  `base_branches` in one trigger and the authoring alias `baseBranches` in the
  other. A `not_in` matcher excludes pull requests labeled `wip` or
  `no-review`.
- `automations/review-requested` sends `@reviewbot` mentions with the keyword
  `review` or `re-review` to the foreman. The foreman dispatches the reviewer
  and replies in the thread.
- `networkPolicy: {}` denies all outbound access except the services that the
  platform requires: the repository forge, configured MCP servers, the image
  registry and the control plane. Reviews need nothing else.
- GitHub triggers skip events from bots by default (`skip_bots: all`).

## Prerequisites

- A running server, with `OF_SERVER` and `OF_API_KEY` set
  (see [Quickstart](../../docs/quickstart.md)).
- The GitHub integration, with webhooks, for the repositories
  (see [GitHub](../../docs/integrations/github.md)).
- The model credential of the `claude` harness:

  ```bash
  openfactory secret set ANTHROPIC_API_KEY --type anthropic_api_key
  ```

## Apply

1. Set your repositories:
   - `factory.yaml`: under `repositories`, change the `owner` and `name` of
     both entries.
   - `automations/pr-opened/automation.md` (both triggers) and
     `automations/review-requested/automation.md`: in `filter.repos`, change
     `acme/api-service` and `acme/web-app` to `<owner>/<name>`.
2. Validate and apply:

   ```bash
   openfactory validate examples/04-code-review-only
   openfactory factory apply examples/04-code-review-only
   ```

## What to expect

`apply` creates the factory `@reviewbot`. When a pull request against `main`
opens or becomes ready, the reviewer posts its findings and verdict on it.
The reviewer stops on draft pull requests.
