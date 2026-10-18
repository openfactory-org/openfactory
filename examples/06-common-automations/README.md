# 06 - Common automations

A catalog of automations to copy. Each automation is one directory with an
`automation.md`. Its frontmatter holds the triggers and execution overrides.
Its Markdown body is the starting prompt.

## What it shows

| Automation | Trigger | Shows |
| --- | --- | --- |
| `slack-mention` | `slack` / `app_mention` | `channels` name alias, resolved through the Slack connection |
| `slack-incident-channel` | `slack` / `message_posted`, `reaction_added` | native `channel_ids` with an `in` matcher, `keywords`, `emojis`, two triggers |
| `linear-intake` | `linear` / `issue_labeled` | `teams` alias, `in` and `not_in` label matchers |
| `linear-comments` | `linear` / `comment_created` | comments need their own automation |
| `linear-agent-session` | `linear` / `agent_session_created` | native `team_ids` (UUIDs) |
| `weekly-digest` | `schedule` / `cron_fired` | named five-field cron (UTC), routed to a custom agent |
| `nightly-dependency-audit` | `schedule` / `cron_fired` | `@daily` and `@every` descriptors; at most one unnamed schedule |
| `ci-failure` | `github` / `workflow_run_completed`, `commit_status` | conclusions, workflows, states, contexts |
| `pr-review-requested` | `github` / `pull_request_review_requested` | `skip_bots: none` |

`spawnableBy: []` on the `reporter` agent means that no agent can dispatch it.
It runs only from automations.

### Matching rules

- Filter keys combine with AND. Values in one list match with OR.
- A plain list is shorthand for `in`. `not_in` excludes values. A value cannot
  be in both.
- An omitted, `null` or empty matcher adds no restriction. Provider defaults
  still apply: GitHub skips bot events unless `skip_bots` says otherwise.
- Slack and Linear name aliases (`channels`, `users`, `itemUsers`, `teams`,
  `projects`, `states`, `issues`) accept plain lists only. You cannot combine
  an alias with its native key.
- `schedule` triggers take a `schedule` object and no `filter`.
- Slack, Microsoft Teams, Linear and Jira triggers require the matching
  integration in `integrations`.

## Prerequisites

- A running server, with `OF_SERVER` and `OF_API_KEY` set
  (see [Quickstart](../../docs/quickstart.md)).
- The GitHub, Slack and Linear integrations (see
  [Integrations](../../docs/integrations/README.md)).
- The model credential of the `claude` harness:

  ```bash
  openfactory secret set ANTHROPIC_API_KEY --type anthropic_api_key
  ```

Each name in a filter alias must match exactly one object, or the server
rejects the definition. Here that is the Slack channels `#engineering` and
`#product`, and the Linear team `Engineering`.

## Apply

1. Set your repositories:
   - `factory.yaml`: under `repositories`, change the `owner` and `name` of
     both entries.
   - `automations/ci-failure/automation.md` and
     `automations/pr-review-requested/automation.md`: in `filter.repos`,
     change `acme/api` and `acme/web` to `<owner>/<name>`.
2. Change the Slack channel names, Linear team names and IDs in the
   automations to ones in your workspaces.
3. Validate and apply:

   ```bash
   openfactory validate examples/06-common-automations
   openfactory factory apply examples/06-common-automations
   ```

## What to expect

`validate` reports 2 agents and 9 automations. `apply` creates the factory
`@builder`. Each automation starts a run when its trigger matches. The two
schedule automations run without an outside event.
