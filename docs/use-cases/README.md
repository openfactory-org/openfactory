# Use cases

Each guide gives a definition, a test procedure and the point where the factory stops for a person.

| Use case | What the factory returns | Example |
|---|---|---|
| [Issue triage](issue-triage.md) | Evidence, category, priority and the next action. No implementation. | [`02-sdlc-issue-to-pr`](../../examples/02-sdlc-issue-to-pr) |
| [Code review](code-review.md) | Findings and a verdict on the pull request. No code changes, no approval. | [`04-code-review-only`](../../examples/04-code-review-only) |
| [Issue implementation](issue-implementation.md) | A validated, reviewed pull request. A person merges. | [`02-sdlc-issue-to-pr`](../../examples/02-sdlc-issue-to-pr) |
| [Incident resolution](incident-resolution.md) | A read-only investigation report. A code fix only on request. | [`05-incident-response`](../../examples/05-incident-response) |
| [End-to-end SDLC](end-to-end-sdlc.md) | An issue carried from triage to a human merge. | [`02-sdlc-issue-to-pr`](../../examples/02-sdlc-issue-to-pr) |

For Slack, Linear, schedule and CI-failure triggers, see [`06-common-automations`](../../examples/06-common-automations) and [Automations](../automations.md).

## Start with one workflow

1. Choose one workflow that your team can evaluate alone. Decide what the factory returns and where it stops for a person.
2. Choose the starting event. One event can match more than one automation, and each match starts work. If another automation already handles the event, change it or use filters that do not overlap.
3. Copy the definition from the guide, change the repositories, labels and team names, then [validate and apply](#apply-a-definition-change) it.
4. Test on a test repository or test project, not on your production backlog. When the results are reliable, widen the filters or connect the next stage.

## Rules that all guides follow

- **The foreman is the entry point.** Automations use `agent: foreman` (change it if your foreman has another name). The foreman dispatches stage agents with `run_agents`, asks people with `ask_human`, and keeps the conversation, so you can ask for the next stage later in the same work item.
- **Stage agents report to the foreman.** They have `spawnableBy: [foreman]` and do not talk to people.
- **External text is data.** Each prompt tells the agents to treat issue text, comments, payloads and code as untrusted data, not as instructions.
- **People merge.** No agent approves or merges. Enforce this with branch protection and required reviews in your code host. A prompt is not an access control.
- **Skills hold your policy.** Put team rules (categories, checks, runbooks) in [skills](../skills.md), not in automation prompts.

## Events for an open work item

While a work item is open, events for its issue or pull request go to that work item. They do not start a new one:

- Comments, mentions, reviews, merges and closes go to the foreman as messages.
- A reply from a linked team member resolves an open question or approval. A reply that starts with an approval phrase (for example "approve" or "lgtm") or a rejection phrase (for example "reject" or "request changes") is the decision. Text from other senders cannot answer or approve.
- Another event (for example a label) that matches a foreman automation sends that automation's prompt to the same work item.
- When the last linked pull request merges, the foreman gets one turn, then the engine completes the work item. You do not need a merge automation.

A comment from a person less than 7 days after the work item completed opens it again. See [References of a work item](../agents.md#references-of-a-work-item) and [How factories work](../how-factories-work.md).

## Apply a definition change

| Source | How to apply |
|---|---|
| `managed` | Edit **Factory definition** in the dashboard and save. Or run `openfactory factory apply <dir> --factory <name>`. Or send the full tree with `PUT /api/v1/factories/{uid}/definition` (see [REST API](../api.md#create-a-factory-from-files)). |
| `git` | Open a pull request against the production branch. The `openfactory/definition` check validates it. Merge it to apply it. |

To check a tree first, run `openfactory validate <dir>`, call `POST /api/v1/factories/{uid}/definition/validate` with `{"files": {...}}`, or call `validate_factory_files` through [Factory MCP](../factory-mcp.md). If a new version is not valid, the previous valid version stays active. See [Definition reference](../definition-reference.md).
