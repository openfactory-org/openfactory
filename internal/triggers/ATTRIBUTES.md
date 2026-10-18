# Event attribute contract for trigger matching

Integrations (forges, chat, trackers), the webhook ingress and the scheduler turn
inbound activity into a `core.Event` and publish it on `core.TopicInboundEvent`.
`triggers.Match` / `triggers.MatchTrigger` evaluate automation triggers against
that event. This file lists, per provider and event, what each producer must set.
Constants for every key are in `attributes.go` (`triggers.Attr*`).

## General rules

- `Event.Provider` is the canonical trigger provider: `github`, `gitlab`,
  `azure_devops`, `slack`, `teams`, `linear`, `jira`, `factory`, `webhook`,
  `schedule`. (`microsoft-teams`, `microsoft_teams` and `azure-devops` are
  tolerated and normalized, but emit the canonical name.)
- `Event.Type` is the exact event string from the tables below.
- Value types: **string**, **[]string** (`[]string` or `[]any` of strings), **int**
  (any Go integer, `float64` with an integral value, or `json.Number`), **bool**.
  Maps/structs as attribute values are an evaluation error (the trigger fails closed).
- Only set attributes you actually know. A missing attribute fails every `in`
  matcher on that key and passes every `not_in` matcher.
- Normalization done by the matcher (you don't need to): branch values lose a
  `refs/heads/` prefix, Slack channel names lose a leading `#`, emoji names lose
  colons and `::skin-tone-N` suffixes, paths lose a leading `/`.
- **Keyword filters** search `Event.Text` (falls back to `Attributes["text"]`),
  case-insensitive substring. Put *all* searchable text there (Slack: message text
  plus block and attachment text; reactions: the reacted-to message's visible text;
  GitHub: the issue/PR body for opened events, the comment body for comment and
  mention events, the review body for review events; Linear sessions: the content
  that started the session).
- **Bots.** Set `Event.Actor.IsBot` (or `actor_is_bot: true`) for bot-authored
  events. Set `actor_is_self: true` for activity by Open Factory's own bot or
  service accounts (any factory's). Self events never match Slack, Teams, Linear,
  Jira, GitLab or Azure DevOps triggers. On GitHub, `skip_bots` decides:
  `all` (default) skips every bot (self counts as a bot), `self` skips only self,
  `none` skips nothing.
- **Labels.** `labels` is always the *current* label set. On `*_labeled` events
  also set `added_labels` (the labels this event added; on creation, the labels
  present at creation). `labels: {in: [...]}` then checks `added_labels`, while
  `not_in` checks `labels`. If `added_labels` is absent, `in` checks `labels`.
- Factory-scoped producers (webhook, schedule) set `Event.FactoryID`; `Match`
  then only considers that factory's automations.

## GitHub (`github`)

| Attribute | Type | Meaning |
| --- | --- | --- |
| `repo` | string | `owner/name` (case-insensitive) |
| `labels` | []string | current labels of the issue/PR (CI events: of the single linked PR) |
| `added_labels` | []string | `issue_labeled`, `pull_request_labeled`: label(s) just added |
| `assignees` | []string | assignee logins (`*_assigned`: the newly assigned login(s)) |
| `author` | string | login of the issue/PR author (comment events: the commenter; review submitted: the reviewer; CI events: the linked PR's author) |
| `mentioned` | []string | logins, `org/team` slugs and app slugs mentioned in the new content (outside code blocks) |
| `base_branch` | string | PR base branch |
| `pr_number` | int | PR number |
| `paths` | []string | files changed by the PR / push (repository-relative) |
| `reviewers` | []string | `pull_request_review_requested`: requested reviewer logins |
| `reviewer_teams` | []string | `pull_request_review_requested`: requested team slugs |
| `review_state` | string | `approved`, `changes_requested`, `commented` |
| `branch` | string | `push`: pushed branch; CI events: head branch |
| `conclusion` | string | `check_suite_completed`, `workflow_run_completed` |
| `workflow` | string | `workflow_run_completed`: workflow name |
| `linked_pr_count` | int | CI events: number of linked PRs (label/author filters need exactly 1) |
| `commit_state` | string | `commit_status`: `success`, `failure`, `error` (`pending` never matches) |
| `context` | string | `commit_status`: status context, e.g. `ci/build` |

Events: `issue_created`, `issue_labeled`, `issue_assigned`, `issue_commented`,
`issue_mentioned`, `pull_request_opened`, `pull_request_ready`,
`pull_request_reopened`, `pull_request_closed`, `pull_request_merged`,
`pull_request_synchronized`, `pull_request_enqueued`, `pull_request_dequeued`,
`pull_request_assigned`, `pull_request_labeled`, `pull_request_commented`,
`pull_request_mentioned`, `pull_request_review_requested`,
`pull_request_review_submitted`, `push`, `check_suite_completed`,
`workflow_run_completed`, `commit_status`, `check_run_rerequested`,
`check_suite_rerequested`.

Filter keys → attributes: `repos`→`repo`, `labels`→`labels`/`added_labels`,
`assignees`→`assignees`, `authors`→`author`, `keywords`→text, `mentioned`→`mentioned`,
`base_branches` (alias `baseBranches`)→`base_branch`, `pr_numbers` (alias `prNumbers`)→`pr_number`,
`paths`→`paths` (globs, `**` = any number of segments), `reviewers`, `reviewer_teams`,
`review_states`→`review_state`, `branches`→`branch`, `conclusions`→`conclusion`,
`workflows`→`workflow`, `states`→`commit_state`, `contexts`→`context`, `skip_bots`.
Logins, labels, repos, states, contexts and conclusions compare case-insensitively.

## GitLab (`gitlab`)

| Attribute | Type | Meaning |
| --- | --- | --- |
| `repo` | string | full project path incl. subgroups, e.g. `acme/platform/api` |
| `action` | string | `merge_request`: `open`, `update`, `close`, `reopen`, `merge`, `approved`, ... |
| `base_branch` | string | `merge_request`: target branch |
| `branch` | string | `push`: pushed branch |
| `mentioned` | []string | `bot_mentioned`: usernames mentioned in the new comment |

Events: `bot_mentioned`, `merge_request`, `push`. `mentioned` is server-managed
(seeded with the factory bot username by the integration, rejected in definitions).

## Azure DevOps (`azure_devops`)

| Attribute | Type | Meaning |
| --- | --- | --- |
| `repo` | string | repository UUID |
| `source_repo` | string | PR events: UUID of the repository holding the source branch (server-managed filter) |
| `base_branch` | string | PR target branch (no `refs/heads/`) |
| `branch` | string | `push`: pushed branch |
| `mentioned` | []string | identity UUIDs mentioned (`*_mentioned`) |
| `work_item_type` | string | e.g. `Bug`, `User Story` |
| `labels` / `added_labels` | []string | work item tags (current / just added on `work_item_labeled`) |
| `assignees` | []string | assignee unique names (e-mail) |
| `author` | string | creator unique name (e-mail) |

Events: `pull_request_created`, `pull_request_updated`, `pull_request_commented`,
`pull_request_closed`, `pull_request_merged`, `pull_request_mentioned`,
`work_item_created`, `work_item_assigned`, `work_item_labeled`,
`work_item_mentioned`, `push` (matchable, not authorable).

## Slack (`slack`)

| Attribute | Type | Meaning |
| --- | --- | --- |
| `channel_id` | string | conversation ID (`C…`, `G…`, `D…`) |
| `channel_name` | string | channel name (alias `channels`), no `#` needed |
| `user_id` | string | acting user (reactions: the reacting user) |
| `user_name`, `user_email` | string | acting user's username and e-mail (alias `users`) |
| `emoji` | string | `reaction_added`: reaction name |
| `item_user_id` | string | `reaction_added`: author of the reacted-to message |
| `item_user_name`, `item_user_email` | string | same author's username / e-mail (alias `itemUsers`) |

Events: `app_mention`, `message_posted`, `message_im` (one-to-one DM),
`message_mpim` (group DM), `reaction_added`, `member_joined_channel`. Emit
`message_im` / `message_mpim` for DMs; a `message_dm` trigger matches both.
Only new messages (not edits/deletions). Mark the receiving app's own posts with
`actor_is_self: true`.

## Microsoft Teams (`teams`)

| Attribute | Type | Meaning |
| --- | --- | --- |
| `team_id` | string | Microsoft Graph team UUID |
| `channel_id` | string | channel conversation ID (`19:…@thread.tacv2`) |
| `channel_name` | string | channel name (alias `channels`); Teams sends it only for channels other than General |
| `user_id` | string | author's Entra object UUID |
| `user_name` | string | author's display name (alias `users`) |

Events: `app_mention`, `message_posted`. Triggers must declare exactly one
`team_ids` value and at least one `channel_ids` value (`in` only). The alias
`channels` can stand for `channel_ids`. Names are compared without case.

## Linear (`linear`)

| Attribute | Type | Meaning |
| --- | --- | --- |
| `team_id`, `team_name` | string | team UUID / exact name (alias `teams`) |
| `project_id`, `project_name` | string | project UUID / exact name (alias `projects`) |
| `labels` / `added_labels` | []string | label names (case-insensitive); `added_labels` on `issue_labeled` (incl. labels present at creation) |
| `state_id`, `state_name` | string | workflow state UUID / name (alias `states`); `issue_state_changed`: the new state |
| `assignees` | []string | assignee user UUIDs; `issue_assigned`: the new assignee |
| `mentioned` | []string | user UUIDs from mention markup and profile slugs from profile URLs in the issue description (issue events) or new comment body (`comment_created`) |
| `author` | string | issue creator / comment author / session creator UUID by event |
| `issue_id` | string | issue UUID |
| `issue_number` | int | issue number within its team (alias `issues`) |

Events: `issue_created`, `issue_labeled`, `issue_assigned`,
`issue_state_changed`, `comment_created`, `agent_session_created`.
On `comment_created` and `agent_session_created`, `labels` are the issue's current labels.

## Jira (`jira`)

| Attribute | Type | Meaning |
| --- | --- | --- |
| `project_key` | string | e.g. `ENG` |
| `project_name` | string | project name (alias `projects`, compared without case) |
| `labels` / `added_labels` | []string | case-sensitive label names (current / just added on `issue_labeled`) |
| `status_id` | string | `status_changed`: the new status ID |
| `status_name` | string | `status_changed`: the new status name (alias `statuses`, compared without case) |

Events: `issue_created`, `issue_labeled`, `status_changed`,
`agent_session_created`. `keywords` on agent sessions is accepted but never restricts.

## Custom webhooks (`webhook`) — produced by `internal/webhooks`

`received` with `Event.FactoryID`, `Event.DeliveryID` (delivery identity),
`Event.Payload` (raw JSON body), and attributes `webhook_id` (UID),
`webhook_name`, `delivery_record_id`. A `webhook_ids` entry matches the UID or the name. The trigger's `payload` pattern is matched
against `Event.Payload`.

## Schedules (`schedule`) — produced by `triggers.Scheduler`

`cron_fired` with `Event.FactoryID` and attributes `automation_id`,
`schedule_name` (empty for the unnamed schedule) and `scheduled_at` (RFC 3339 UTC).
`Match` binds the event to the automation whose ID equals `automation_id` and
whose schedule trigger has the same name.
