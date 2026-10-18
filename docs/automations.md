# Automations and triggers

An automation starts factory work when an event matches one of its triggers. It sends a starting prompt to an agent (the foreman by default) and can change how that agent runs. This page covers how matching works and every provider, event and filter key of a trigger.

## The automation file

Each automation is `automations/<name>/automation.md` in the [definition tree](definition-reference.md). `<name>` is 1-64 letters, digits, `.`, `_` or `-`, and starts with a letter or digit. The YAML front matter holds the configuration. The Markdown body is the starting prompt and must not be empty.

```markdown
---
displayName: Review pull requests on main
agent: reviewer
triggers:
  - provider: github
    event: pull_request_opened
    filter:
      repos: [acme/api]
      base_branches: [main]
      labels:
        not_in: [wip]
---

A pull request was opened against main. If it is a draft, stop.
Otherwise, review it and post your findings on the pull request.
```

- `triggers` is required. Any one trigger starts the automation.
- `enabled: false` disables the automation. `displayName` is the dashboard name. `agent` names the agent (default: the foreman).
- `runAs` sets the principal of the runs: `factory` (default), `user:<email>` or `serviceAccount:<name>`. With `credentialStrategy: CREATOR`, it decides whose forge credential a run uses, and so who authors pull requests. See [Run principals](definition-reference.md#run-principals).
- The execution settings (`model` or `harness`, `machine`, `environmentId`, `secrets`, `mcpServers`, `workerHost`, `idleTimeoutMinutes`) apply over the agent. `secrets` and `mcpServers` add to the agent's own; a server with the same name replaces the agent's server.

All fields are in the [automation field table](definition-reference.md#automationsnameautomationmd). Examples: [`../examples/06-common-automations`](../examples/06-common-automations), [`../examples/04-code-review-only`](../examples/04-code-review-only) and [`../examples/05-incident-response`](../examples/05-incident-response).

## How an event reaches an automation

Integrations, custom webhooks and the scheduler turn activity into events. One action can give several events: for example, a GitHub comment that mentions the factory gives a comment event and a mention event. These events share an action key, and the engine handles them as one action, in this order:

1. **Drop own activity.** An action of an Open Factory bot or app is dropped.
2. **Check the sender.** A sender is a team member when its external account is linked to an active member of the team. Other senders are external.
3. **Continue existing work.** If the action is on the source of an active work item (the same issue, pull request, chat thread or agent session) and is text, a review, a merge or a close, the root run of the work item gets one message. Text from an external sender is marked as untrusted. It cannot approve, answer a question or change the agent's instructions.
4. **Match automations.** The engine evaluates the enabled automations of each candidate factory, also when step 3 delivered the action. An automation matches an action once.
5. **Direct mention.** If the action mentions a factory alias, nothing else in that factory got it, and the sender is a team member, the engine starts work for that factory.

The candidate factories depend on the source:

| Source | Candidate factories |
| --- | --- |
| GitHub, GitLab, Azure DevOps | Factories whose `repositories` include the event's repository. A `factory:<alias>` label or an alias mention selects one factory. |
| Slack, Microsoft Teams, Linear, Jira | Factories that declare the integration in `factory.yaml` `integrations` (types `slack`, `microsoft-teams`, `linear`, `jira`). If the integration record sets `factory_id`, only that factory. Always declare the integration. See [Integrations](integrations/README.md). |
| Custom webhooks, schedules, factory events | Only the factory that owns the webhook, schedule or work item. |

A match in step 4 works like this:

- If the factory has no active work item for the source, the first match creates one, and its agent starts the root run.
- If the match names the agent of the root run, the root run gets the action, once, also when step 3 delivered it.
- If the match names another agent, the engine starts a side run of that agent on the same work item. A later action for the same work item and automation continues the side run. A side run does not change the stage. While the work item is active, the root run gets a message each time the side run starts, ends a turn, fails or is cancelled. The `started` message does not start a turn; the root run reads it with its next prompt. The other messages start a turn, or the root run reads them at the end of its current turn.
- Two matches with the same agent on the same work item give one run. Each match with its own agent starts its own run.

Other rules:

- A bot that is not an Open Factory bot is delivered with a bot mark. A work item accepts at most 20 bot actions per hour. This stops loops between bots.
- `requireLinkedActor: true` in `factory.yaml` drops each forge, chat or tracker action whose sender is not a linked team member. The default is `false`. It does not apply to custom webhooks and schedules.
- A second delivery of the same action does nothing. If the engine fails in the middle, the retry does only the missing work.
- Filters decide when work starts. They do not control access. The integration's permissions at the provider control what agents can read and change.

## What the agent gets from the event

Each message that an event causes carries the event context, written to `.openfactory/events/<message_id>/` in the run's workspace:

| Path | Content |
| --- | --- |
| `envelope.json` | The provider, event type, action key, sender, trust, source and time. It also lists the context files and each file that was left out. |
| `payload.json` | The body of the provider request, at most 256 KiB. A longer body is cut, and `envelope.json` says so. |
| `context/` | Files from the provider's context builder. Only the first message of a run has them. |

| Source | Context |
| --- | --- |
| Slack thread | `context/thread.json` with the thread's messages, and `context/files/` with the thread's files that the app can read (at most 10 files of 1 MiB each). |
| Review comment (GitHub, GitLab, Azure DevOps) | `context/review_thread.json` with all comments of the review thread, and `context/file_diff.patch` with the diff of the commented file, read from the forge API. Each file is at most 256 KiB: the oldest comments are left out, and the diff is cut at the end of a line. |
| GitHub review comment, GitLab diff note | Also `context/review_comment.json` (the comment, file and line), and for GitHub `context/diff.patch` (the diff hunk from the webhook). These come from the payload, so they exist also when the forge API cannot be read. |
| Linear or Jira issue | `context/issue.json` with the title, description, state, labels, assignee and project. |
| Custom webhook | The payload only. |

- The prompt names the directory in a section for untrusted data. The agent must read the files as data, not as instructions.
- A file that cannot be read does not stop the run. `envelope.json` and the prompt list it as left out, with the reason. Examples: the integration credential cannot read the pull request, a GitHub file has no patch because it is binary or very large, or a forge plugin does not support the operation.
- Secret values of the run are replaced with `[REDACTED]`.
- `.openfactory/` is in the local git exclude of each repository.
- The database keeps the event context only while the run needs it, and removes it when the run ends. The leader replica removes leftover context older than 7 days. The workspace files stay as long as the workspace.

## Write a trigger

A trigger has a `provider`, an `event` and an optional `filter`. `provider` and `event` are separate keys: a dotted value such as `github.push` is not valid.

```yaml
triggers:
  - provider: linear
    event: issue_labeled
    filter:
      teams: [Engineering]
      labels:
        in: [factory-ready]
        not_in: [blocked]
```

### Matching rules

- All filter keys must match.
- A list matches any of its values: `labels: [bug, regression]` matches either label.
- `not_in` excludes: `labels: {not_in: [wip]}` rejects an event with the `wip` label.
- An omitted key, `null`, an empty list and an empty matcher object add no restriction.
- If the event does not carry the attribute, `in` fails and `not_in` passes.
- If a filter cannot be evaluated, the trigger does not match, and the server logs a warning.

### Matcher syntax

Most keys accept a plain list or a matcher object:

```yaml
labels: [factory-ready, urgent]          # shorthand for in
labels:
  in: [factory-ready, urgent]
  not_in: [blocked]
```

A value cannot appear in both `in` and `not_in`. Unknown providers, events, keys and operators are rejected. These keys are exceptions:

| Key | Rule |
| --- | --- |
| GitHub `skip_bots` | A scalar: `all` (default), `self` or `none`. |
| Name aliases (Slack `channels`, `users`, `itemUsers`; Teams `channels`, `users`; Linear `teams`, `projects`, `states`, `issues`; Jira `projects`, `statuses`) | A plain list only. Do not combine an alias with its native key. |
| Webhook `payload` | A JSON pattern. See [Payload patterns](#payload-patterns). |
| Teams `team_ids`, `channel_ids`; webhook `webhook_ids` | `in` only. Required. See the provider sections. |
| Server-managed keys (GitLab `mentioned`, Azure DevOps `source_repos`, schedule `schedule_ids`) | Rejected in definition files. |

The server resolves alias names when you apply the definition (see [Preflight](definition-reference.md#preflight)). It caches each name list for 5 minutes. If a name is missing, it reads the list again, at most once every 30 seconds. After an error, it waits 30 seconds and uses the old list for up to one hour.

Lists are not glob patterns, except GitHub `paths`. `keywords` are case-insensitive substrings, not regular expressions. They search the event text: the message, the issue or pull request body for opened events, and the comment or review body for comment, mention and review events.

### Provider defaults

These rules apply before the filter:

- **GitHub bots.** `skip_bots` defaults to `all`, so bot events do not start work. `self` skips only Open Factory bots. `none` skips nothing.
- **Own activity.** Activity by Open Factory bots and service accounts never matches Slack, Teams, Linear, Jira, GitLab or Azure DevOps triggers.
- **Commit statuses.** GitHub `commit_status` matches only `success`, `failure` and `error`, never `pending`.
- **Linked pull request.** On GitHub `check_suite_completed` and `workflow_run_completed`, `labels` and `authors` refer to the linked pull request. With zero or several linked pull requests, a trigger with these keys does not match.
- **Label events.** On `*_labeled` events, `labels: {in: [...]}` checks only the labels that the event added; `not_in` checks all current labels. On creation, the labels present at creation count as added.

## Trigger reference

Provider names are `github`, `gitlab`, `azure_devops`, `slack`, `teams` (alias `microsoft_teams`), `linear`, `jira`, `schedule` and `webhook`.

### GitHub (`github`)

Set up the integration with [GitHub](integrations/github.md).

| Event | Filter keys |
| --- | --- |
| `issue_created` | `repos`, `labels`, `assignees`, `authors`, `keywords`, `skip_bots` |
| `issue_labeled`, `issue_assigned`, `pull_request_assigned` | `repos`, `labels`, `assignees`, `authors`, `skip_bots` |
| `issue_commented`, `pull_request_commented` | `repos`, `labels`, `authors`, `keywords`, `skip_bots` |
| `issue_mentioned`, `pull_request_mentioned` | `repos`, `mentioned`, `labels`, `authors`, `keywords`, `skip_bots` |
| `pull_request_opened`, `pull_request_ready`, `pull_request_reopened` | `repos`, `base_branches`, `pr_numbers`, `paths`, `assignees`, `authors`, `labels`, `keywords`, `skip_bots` |
| `pull_request_closed`, `pull_request_merged`, `pull_request_synchronized`, `pull_request_labeled`, `pull_request_enqueued`, `pull_request_dequeued` | `repos`, `base_branches`, `pr_numbers`, `paths`, `assignees`, `authors`, `labels`, `skip_bots` |
| `pull_request_review_requested` | `repos`, `reviewers`, `reviewer_teams`, `authors`, `skip_bots` |
| `pull_request_review_submitted` | `repos`, `mentioned`, `labels`, `review_states`, `authors`, `keywords`, `skip_bots` |
| `push` | `repos`, `branches`, `paths`, `skip_bots` |
| `check_suite_completed` | `repos`, `conclusions`, `branches`, `labels`, `authors`, `skip_bots` |
| `workflow_run_completed` | `repos`, `conclusions`, `branches`, `workflows`, `labels`, `authors`, `skip_bots` |
| `commit_status` | `repos`, `states`, `contexts`, `skip_bots` |
| `check_run_rerequested`, `check_suite_rerequested` | `repos`, `skip_bots` |

| Key | Values |
| --- | --- |
| `repos` | `owner/name`. Case-insensitive. |
| `labels`, `assignees`, `authors` | Label names and logins. Case-insensitive. `authors` is the commenter on comment events and the reviewer on `pull_request_review_submitted`. |
| `mentioned` | Logins, `org/team` slugs and app slugs mentioned in new content. Mentions in code blocks do not count. |
| `base_branches` (alias `baseBranches`) | The pull request base branch. A `refs/heads/` prefix is ignored. |
| `branches` | The pushed branch, or the head branch of a CI run. |
| `pr_numbers` (alias `prNumbers`) | Positive integers. |
| `paths` | Repository-relative globs. `**` matches any number of path segments. `*` does not cross `/`. Case-sensitive. |
| `reviewers`, `reviewer_teams` | Requested reviewer logins and team slugs. |
| `review_states` | `approved`, `changes_requested`, `commented`. |
| `conclusions` | The CI conclusion, such as `success` or `failure`. |
| `workflows` | The workflow name. Case-sensitive. |
| `states` | `success`, `failure`, `error`. |
| `contexts` | The commit status context, such as `ci/build`. |
| `skip_bots` | `all` (default), `self`, `none`. See [Provider defaults](#provider-defaults). |

```yaml
triggers:
  - provider: github
    event: workflow_run_completed
    filter:
      repos: [acme/api]
      branches: [main]
      workflows: [CI]
      conclusions: [failure]
```

### GitLab (`gitlab`)

Set up the integration with [GitLab](integrations/gitlab.md).

| Event | Filter keys |
| --- | --- |
| `bot_mentioned` | `repos` (`mentioned` is server-managed) |
| `merge_request` | `repos`, `actions`, `base_branches` |
| `push` | `repos`, `branches` |

- `repos` is the full project path with subgroups, such as `acme/platform/api`. Case-insensitive.
- `actions` is the merge request action: `open`, `update`, `close`, `reopen`, `merge`, `approved` and other GitLab actions.
- `base_branches` is the target branch. `branches` is the pushed branch.

### Azure DevOps (`azure_devops`)

Set up the integration with [Azure DevOps](integrations/azure-devops.md).

| Event | Filter keys |
| --- | --- |
| `pull_request_created`, `pull_request_updated`, `pull_request_commented`, `pull_request_closed`, `pull_request_merged` | `repos`, `base_branches` |
| `pull_request_mentioned` | `repos`, `mentioned` |
| `work_item_created`, `work_item_assigned`, `work_item_labeled` | `work_item_types`, `labels`, `assignees`, `authors` |
| `work_item_mentioned` | `mentioned` |

- `repos` takes repository UUIDs. `mentioned` takes identity UUIDs.
- `work_item_types` is case-sensitive, such as `Bug` or `User Story`.
- `labels` are work item tags. `assignees` and `authors` are unique names (e-mail addresses).
- `push` events are not available for automations.

### Slack (`slack`)

Set up the integration with [Slack](integrations/slack.md).

| Event | Filter keys |
| --- | --- |
| `app_mention`, `message_posted`, `message_dm`, `message_im`, `message_mpim` | `channel_ids`, `user_ids`, `keywords` |
| `reaction_added` | `channel_ids`, `user_ids`, `keywords`, `emojis`, `item_user_ids` |
| `member_joined_channel` | `channel_ids`, `user_ids` |

- `message_im` is a one-to-one direct message. `message_mpim` is a group direct message. `message_dm` matches both.
- Only new messages count. Edits and deletions do not.
- `channel_ids` takes conversation IDs (`C…`, `G…`, `D…`). Alias `channels` takes channel names. A leading `#` is ignored.
- `user_ids` takes member IDs. Alias `users` takes usernames or e-mail addresses (case-insensitive).
- `emojis` takes reaction names. Colons and skin-tone suffixes are ignored.
- `item_user_ids` is the author of the message that got the reaction. Alias `itemUsers` takes usernames or e-mail addresses.
- On `reaction_added`, `keywords` searches the text of the message that got the reaction.

```yaml
triggers:
  - provider: slack
    event: reaction_added
    filter:
      channels: ["#incidents"]
      emojis: [rotating_light]
```

### Microsoft Teams (`teams`)

Set up the integration with [Microsoft Teams](integrations/teams.md). Teams triggers require a filter with `team_ids` and `channel_ids`.

| Event | Filter keys |
| --- | --- |
| `app_mention`, `message_posted` | `team_ids` (required), `channel_ids` (required), `user_ids`, `keywords` |

- `team_ids` takes exactly one Microsoft Graph team ID, with `in` only.
- `channel_ids` takes one or more channel conversation IDs (`19:…@thread.tacv2`), with `in` only. Alias `channels` takes channel names (case-insensitive, a leading `#` is ignored) and counts as the required `channel_ids`. Teams does not send the name of the General channel, so use its ID.
- `user_ids` takes Microsoft Entra object IDs (UUIDs). Alias `users` takes display names. When the server resolves the names at apply time, it also accepts e-mail addresses.

```yaml
triggers:
  - provider: teams
    event: app_mention
    filter:
      team_ids: ["0f5c2a1e-8b4d-4c3a-9e2f-7a6b5c4d3e21"]
      channel_ids: ["19:a1b2c3d4e5f6@thread.tacv2"]
```

### Linear (`linear`)

Set up the integration with [Linear](integrations/linear.md).

| Event | Filter keys |
| --- | --- |
| `issue_created`, `issue_labeled`, `issue_assigned`, `issue_state_changed` | `team_ids`, `project_ids`, `labels`, `state_ids`, `assignee_ids`, `mentioned_user_ids`, `creator_ids` |
| `comment_created` | `team_ids`, `project_ids`, `labels`, `state_ids`, `issue_ids`, `mentioned_user_ids`, `creator_ids` |
| `agent_session_created` | `team_ids`, `project_ids`, `labels`, `creator_ids`, `keywords` |

| Key | Values | Name alias |
| --- | --- | --- |
| `team_ids` | Team UUIDs | `teams` (exact team names) |
| `project_ids` | Project UUIDs | `projects` (exact project names) |
| `state_ids` | Workflow state UUIDs. On `issue_state_changed`, the new state. | `states` (state names) |
| `issue_ids` | Issue UUIDs | `issues` (issue numbers, such as `42`) |
| `labels` | Label names. Case-insensitive. On comments and sessions, the issue's current labels. | |
| `assignee_ids`, `creator_ids`, `mentioned_user_ids` | User UUIDs | |
| `keywords` | Substrings of the content that started the session | |

### Jira (`jira`)

Set up the integration with [Jira](integrations/jira.md).

| Event | Filter keys |
| --- | --- |
| `issue_created`, `issue_labeled` | `project_keys`, `labels` |
| `status_changed` | `project_keys`, `status_ids` |
| `agent_session_created` | `project_keys`, `labels`, `keywords` |

- `project_keys` takes project keys such as `ENG`. Case-sensitive. Alias `projects` takes project names.
- `labels` takes label names. Case-sensitive.
- `status_ids` takes the new status ID. Alias `statuses` takes status names, such as `Done`. Only on `status_changed`.
- `keywords` on `agent_session_created` is accepted but never restricts.

### Schedules (`schedule`)

A schedule trigger uses `event: cron_fired` and a `schedule` object. It accepts no `filter`.

```yaml
triggers:
  - provider: schedule
    event: cron_fired
    schedule:
      cron: "@daily"
  - provider: schedule
    event: cron_fired
    schedule:
      name: monday-morning
      cron: "0 9 * * 1"
```

| Field | Required | Description |
| --- | --- | --- |
| `schedule.cron` | Yes | A five-field cron expression (minute, hour, day of month, month, day of week) or a descriptor. |
| `schedule.name` | No | A name that identifies the schedule in the automation. |
| `concurrency` | No | On the trigger, next to `schedule`. `allow` (default) starts a run for every slot. `skip` drops a slot while a run of the automation is not finished. |

Rules:

- Schedules run in UTC. `TZ=` and `CRON_TZ=` prefixes are rejected.
- Descriptors: `@yearly`, `@annually`, `@monthly`, `@weekly`, `@daily`, `@midnight`, `@hourly` and `@every <duration>`, such as `@every 1h`.
- An `@every` interval must be at least one minute. The scheduler skips a shorter interval and logs a warning.
- At most one schedule trigger of an automation can omit `schedule.name`. Names must be unique.
- A changed `cron` keeps the schedule and its last-fired time. A changed `name` replaces the schedule.
- The server writes the last-fired time and the slot's event in one transaction. A restart neither fires a slot twice nor loses it.
- Missed slots are not replayed. After downtime, a due schedule fires once.

#### Pause, resume and run now

These calls need no definition change, so they also work for a `git` factory without a commit.

| Action | Request | Role |
| --- | --- | --- |
| List the schedules | `GET /api/v1/factories/{uid}/schedules` | user |
| Pause a schedule | `POST /api/v1/factories/{uid}/schedules/{id}/pause` | editor |
| Resume a schedule | `POST /api/v1/factories/{uid}/schedules/{id}/resume` | editor |
| Start an automation now | `POST /api/v1/automations/{id}/run` | editor for the factory |

- The schedule `{id}` is the automation name, then `:` and the schedule name, for example `nightly-report:main`. A schedule without a name has the automation name only.
- The list shows `last_run_at`, `next_run_at`, `paused`, `concurrency` and `last_error` for each schedule.
- The pause is stored in the database, not in the definition. Slots due during the pause are not replayed after `resume`.
- `next_run_at` is `null` when the schedule is paused or the automation is disabled.
- A manual start uses the automation's prompt, agent and overrides. It also works for a disabled automation.

### Custom webhooks (`webhook`)

A webhook trigger uses `event: received`. The filter is required and must contain `webhook_ids`.

```yaml
triggers:
  - provider: webhook
    event: received
    filter:
      webhook_ids: [sentry-alerts]
      payload:
        action: [created]
        data:
          issue:
            level: [fatal]
```

- `webhook_ids` is a one-item list: the webhook UID or name (the `<name>` of `webhooks/<name>.yaml`, not the file name). For several webhooks, add one trigger per webhook.
- `payload` is optional. Without it, every accepted delivery matches.

To create webhooks and configure senders, see [Custom webhooks](webhooks.md).

## Payload patterns

A `payload` pattern is an object that matches the JSON body of a webhook delivery. Each key names a field of the body, and every key must match. Each key maps to one of these values:

| Value | Meaning |
| --- | --- |
| A list | The allowed values. This is shorthand for `{in: [...]}`. |
| An operator object | An object whose keys are only `in`, `not_in` and `exists`. All operators on the field must match. |
| Another object | A nested pattern for an object field. The field must be present. |

```yaml
payload:
  event: [deploy.finished]
  env:
    not_in: [dev, staging]
  service:
    exists: true
  deployment:
    target: [production]
```

Matching rules:

- Strings match exactly (case-sensitive). Numbers match by value (`1` equals `1.0`). Booleans and `null` can be matched.
- A missing field fails `in` and passes `not_in`. `exists: false` matches only a missing field.
- For an array of scalars, `in` matches if any element is in the set, and `not_in` requires that no element is. For an array of objects, a nested pattern matches if any element matches.
- An object value never matches a list of allowed values.
- If the body is not valid JSON or the pattern cannot be evaluated, the trigger does not match.

Limits and validation:

- At most 10 levels of nesting, 64 leaf fields, and 32 values per `in` or `not_in` set.
- Allowed values are strings, numbers, booleans or `null`.
- A value in both `in` and `not_in` of one field is rejected, and so is `exists: false` with a non-empty `in`.
- In a definition file, a nested object must not be empty. To test only that a field is present, use `exists: true`.

To test a pattern against a stored delivery, see [Test a filter](webhooks.md#test-a-filter).

## Validate and inspect automations

Validate a definition tree before you apply it:

```bash
openfactory validate ./my-factory
```

Validation checks provider and event names, filter keys per event, matcher shapes, value types, required keys, payload patterns and cron expressions. It does not check that a Slack channel or Linear team name exists. The apply and `POST /api/v1/factories/{uid}/definition/validate` do that when the integration is connected. See [Preflight](definition-reference.md#preflight).

List the automations of the applied definition with `GET /api/v1/factories/{uid}/automations`.

## Troubleshooting

| Problem | Check |
| --- | --- |
| An event does not start work | The automation is enabled and the definition applied. The integration delivers events (see its page). For forge events, the repository is in `repositories`. For chat and tracker events, `factory.yaml` declares the integration. |
| An event continues an old work item | The event is on the source of an active work item. Complete or cancel that work item to start new work from the same source. |
| A GitHub bot event does not start work | `skip_bots` defaults to `all`. Set `skip_bots: none` or `self`. |
| A label filter does not match on a label event | `in` checks only the labels that the event added. |
| A schedule does not fire | The cron expression is in UTC and an `@every` interval is at least one minute. Check `last_error` in the schedule list, or the server log for `schedule: invalid cron`. |
| A webhook delivery does not start work | See [Custom webhooks troubleshooting](webhooks.md#troubleshooting). |
