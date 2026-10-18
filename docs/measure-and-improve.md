# Measure and improve a factory

| Feature | What it tells you | Where |
|---|---|---|
| [Dashboard metrics](#dashboard-metrics) | How much work the factory did, how fast, with how much human help, and at what cost. | Factory **Dashboard** page, `GET /api/v1/factories/{uid}/metrics` |
| [Scorers](#scorers) | Whether finished runs of selected agents meet criteria that you write. | `scorers/<name>/scorer.md`, **Scorers** page |
| [Self-improvement](#self-improvement) | Investigates repeated scorer failures and proposes fixes for review. | `selfImprovement` in `factory.yaml`, **Scorers** page |
| [Benchmarks](#benchmarks) | How model or harness configurations of one agent perform on the same fixed tasks. | `benchmarks/<suite>/`, **Benchmarks** page |

Scorer judges, the benchmark Correctness Scorer and Self-improvement are all runs in a sandbox. They use the factory's harness and credentials, so no platform model key is necessary, and they add to the factory's cost.

## Dashboard metrics

The factory **Dashboard** page shows the last 7, 30 or 90 days. The API returns the same data:

```bash
curl -s "$OF_PUBLIC_URL/api/v1/factories/fac_123/metrics?since=30d" \
  -H "Authorization: Bearer $OF_API_KEY"
```

`since` accepts an RFC 3339 time, a date, or a relative duration such as `7d` (default 30 days). The response has `factory_id`, `since` and `metrics`, with `work_items`, `pull_requests`, `autonomy`, `cycle_time`, `human_wait`, `runs`, `cost`, `scorers` and `daily` (one point per UTC day).

Use the metrics to choose which runs to inspect, not alone to explain a change. For example, the run total includes evaluation, benchmark and Self-improvement runs, so a higher run count with a flat pull request count can mean harder tasks, retries or measurement.

### Metric definitions

All ranges are half-open, `[since, until)`, in UTC. Daily buckets are UTC calendar days.

| Metric | Definition |
|---|---|
| Work items by stage | Work items created in the window, by current stage. **Created** counts items created in the window. **Completed** and **Cancelled** count items whose completion time is in the window, with stage `complete` or `cancelled`. |
| PRs opened | Distinct pull requests linked to the factory's work items, identified by forge, repository and number, or by URL. Each counts once, in the period of its first observation: the earliest `pr_linked` event of the work item for it, or else the creation time of the work item. |
| PRs merged | Of the pull requests opened in the window, those whose state is now `merged`. It does not count merges in the window of older pull requests. |
| Cost per PR | The cost of a work item (the usage of the foreman run and every descendant), split evenly across its pull requests. The dashboard shows the median over the pull requests opened in the window, and the most expensive ones. A comparison metric, not a billing figure. |
| Cost by PR size | The cost of the pull requests opened in the window, grouped by changed lines (additions plus deletions, from pull request facts): `xs` (0–9), `s` (10–49), `m` (50–249), `l` (250–999), `xl` (1000 or more), or `unknown`. Each group has the count, the median cost and the total cost. |
| PR autonomy | Of the pull requests that merged in the window and have facts, the share that no person opened or pushed to before the merge. Pushes by the product's own bot or another bot do not count. Merged pull requests without facts are counted apart (`merged_prs_without_facts`). |
| Work item autonomy | The share of work items completed in the window that needed no human intervention other than required approvals. See the intervention rules below. |
| PR cycle time | For pull requests that merged in the window and have facts: the median time from work item creation to merge. Separate medians cover work item start to pull request, pull request to first review, and first review to merge. A review counts when it is by someone other than the opener and the product's own bot. The parts do not add up to the total. |
| Work item cycle time | For work items completed (stage `complete`) in the window: completion time minus creation time. Reports the median, the 90th percentile (nearest rank) and the mean. |
| Time to PR | The median of first pull request observation minus work item creation, over the pull requests opened in the window. |
| Stage medians | The median time that completed items spent in each stage, from `stage_changed` events. The medians are independent and do not add up to the cycle time. |
| Human wait time | Inbox items of kind `approval`, `question`, `handoff` or `failure` created in the window. The wait is resolution time minus creation time, or `until` minus creation time for pending items. Median and P90 cover resolved items. Total includes pending waits up to `until`. |
| Runs | Runs created in the window, by agent, state, agent and state, model, and source. The source is one of `API`, `MCP`, `DASHBOARD`, `CLI`, `AUTOMATION`, `SCHEDULE`, `WEBHOOK`, `MENTION`, `ORCHESTRATION`, `SCORER`, `BENCHMARK` and `SELF_IMPROVEMENT`. An unknown source counts as `API`. |
| Cost and tokens | The usage of runs created in the window, by agent and by model. Each total has `compute_seconds`: the lifetime of the runs' sandboxes. `cost.compute_by_shape` groups sandbox time by instance shape, for example `linux/x86_64 4 vCPU 8 GB`, or `default` for a session without a shape. Compute time is a duration, not a price. Only runs with sessions have compute time. |
| Scorer pass rates | Scores created in the window for each scorer, with the count of each label. |

**Pull request facts.** From forge events, the engine records who opened each pull request, each push to its branch with the actor kind (the product's own bot, another bot, or a person), the first review, the merge time and the changed lines. Facts start when the forge is connected, so older pull requests are not in the PR figures. `autonomy.facts_since` is the time of the first fact.

- GitHub sends all facts. A person's push through the factory's own account counts as a push by the product's bot.
- GitLab and Azure DevOps send no changed lines, so their pull requests are in size `unknown`. Azure DevOps sends no review event.
- Pushes before the pull request opened are not recorded. A pull request that a person opened counts as a push by a person.

**Intervention rules.** A work item needed intervention when one or more of these is true:

- It has an inbox item of kind `question`, `handoff` or `failure` (from `ask_human`, a handoff to a person, or a failure escalation).
- Its timeline has a `question` or `answer` event.
- A person sent another message (`message_in`) after the work started, that is, after the first `run_started` event or the initial request message.
- A person took it over locally ([Factory MCP](factory-mcp.md#continue-a-work-item-locally) `get_work_item` with `start_working=true`).

Required human checkpoints are not intervention: approval requests and decisions (`approval` inbox items, `approval_requested` and `approval_decided` events), review items, comments, pull request reviews and the merge.

## Scorers

A scorer classifies a finished run with exactly one of the labels that you declare. Each label has a score from 0 to 1. A run passes when its label's score is at least `passingScore`.

The judge is a run in a sandbox, like an agent run, with a harness, machine, secrets and MCP servers. It can read the repository and use its tools to check the work.

Keep each scorer focused on one question whose failures point to a cause, for example "Did the agent run the tests before it opened a pull request?".

### Define a scorer

Put each scorer in `scorers/<name>/scorer.md`. The frontmatter configures the scorer. The Markdown body is the rubric for the judge, and it must not be empty.

```markdown
---
name: tests-run
description: Checks whether implementation runs include test evidence.
agents:
  - implement
labels:
  - value: tests_run
    description: The transcript contains test commands and their passing results.
    score: 1
  - value: tests_partial
    description: Some checks ran, but tests for the changed behavior are missing.
    score: 0.5
  - value: tests_skipped
    score: 0
passingScore: 1
samplingRate: 25
model: claude-haiku-5-5
selfImprovement: true
---

Evaluate whether the agent added or updated tests for the behavior it changed
and ran the repository's checks before reporting completion. Return exactly
one declared label and cite the transcript lines that justify it.
```

| Field | Required | Rules |
|---|---|---|
| `name` | Yes | Unique in the factory. |
| `description` | No | Short description. |
| `output` | No | Only `classification`. |
| `agents` | Yes | At least one agent. Each item is an agent name, or `{name, includeDescendants}`. Each name must match `agents/<name>/agent.md` and appear once. With `includeDescendants: true`, the transcripts of all descendant runs (at most 200) are added as evidence. |
| `labels` | Yes | 1 to 20 labels. Each has a unique `value`, an optional `description`, and a `score` from 0 to 1. At least one label must meet `passingScore`, and at least one must be below it. |
| `passingScore` | Yes | 0 to 1. |
| `samplingRate` | No | Percentage of finished runs to score, 0 to 100, rounded to two decimals. Default `10`. `0` turns off automatic scoring. |
| `model` | Yes, unless `harness` is set | The model of the judge run. It replaces the model of the harness. |
| `harness` | No | The harness of the judge run. It applies over `scorerDefaults.harness`, which applies over the harness of `agentDefaults`. |
| `selfImprovement` | No | `true` sends failures of this scorer to [Self-improvement](#self-improvement). Default `false`. |
| `machine` | No | The machine of the judge run. It replaces `scorerDefaults.machine`. |
| `secrets`, `mcpServers` | No | Added to the lists of `scorerDefaults`. A server with the same name replaces the `scorerDefaults` server. |

For `scorerDefaults`, see the [Definition reference](definition-reference.md).

### The judge run

The judge run gets:

- the scorer's harness (scorer fields over `scorerDefaults` over `agentDefaults`), machine, secrets and MCP servers, plus the factory-wide secrets and MCP servers;
- the full transcript of the scored run (with child runs when `includeDescendants` is true), as `transcript.md` of the skill `scored-run` in its workspace;
- the repositories, checked out at the commits where the scored run started;
- the rubric and labels as instructions. Its final message must end with a JSON object: `{"label": "...", "rationale": "..."}`.

A judge run belongs to the factory but to no work item. It is not scored and does not count in the autonomy metric. Its `source` is `SCORER`, its `source_ref` is the scorer ID, and its metadata has `judge=score`. The session ends after the first turn. Its usage is stored on the score as `judge_usage` and adds to the cost of the scored run.

### Automatic scoring

A run is scored automatically when all of these are true:

1. The run reached `succeeded` or `failed`.
2. The run is of a factory agent that the scorer lists in `agents`.
3. The run is in the scorer's sample.
4. Neither the run nor an ancestor is a benchmark trial.

Sampling is deterministic. The service maps a SHA-256 hash of the scorer ID and the run ID onto 0 to 100, and the run is in the sample when that value is less than `samplingRate`. So repeated updates of a run give the same result, and scorers sample independently. Each scorer stores at most one automatic score per run.

The score is stored when the judge run finishes. The service accepts an exact label match first, then a case-insensitive match that ignores surrounding spaces, quotes and backticks. Any other answer, a failed judge run, or a final message without the JSON object stores no score. The transcript file is cut at 8 MB, and keeps the start and the end.

Each score has `label`, `score`, `passed`, `rationale`, `agent_name`, `judge_run_id`, `judge_usage` and `created_at`.

### Score a run on demand

On a run page in the dashboard, use **Score this run**: choose a scorer and click **Score**. On-demand scoring ignores sampling and the scorer's agent list. A new result replaces an earlier score of the same scorer. Use it to test a new rubric on a few runs before you raise `samplingRate`.

`POST /api/v1/runs/{runId}/score` (role `editor`) answers `202` with a score that has no `id` yet. Its `judge_run_id` names the judge run. The score appears in the list when the judge run finishes.

### Read scores

- The **Scorers** page shows recent results with run, scorer, agent, label, score and reasoning.
- The **Dashboard** page shows a card for each scorer with its pass rate and label breakdown.
- `GET /api/v1/factories/{uid}/scorers` lists the scorers.
- `GET /api/v1/factories/{uid}/scores` lists scores. Filters: `scorer` (slug or ID) or `scorer_id`, `passed` (`true` or `false`), `agent`, `since`, with [pagination](api.md#pagination).

## Self-improvement

Self-improvement groups the scorer failures of each agent. When an agent has enough failures, it starts a run that investigates the common root cause and proposes a fix. Nothing is adopted without human review.

### Turn it on

1. Set `selfImprovement: true` in each scorer whose failures you want investigated.
2. Optionally, configure the trigger and the reviewers in `factory.yaml`:

```yaml
selfImprovement:
  failedRunThreshold: 10
  reviewerType: custom
  reviewerEmails:
    - lead@acme.dev
    - sre@acme.dev
  analysisModel: claude-opus-5-5
```

| Field | Default | Rules |
|---|---|---|
| `failedRunThreshold` | `25` | 1 to 50. The number of distinct unreviewed failures of one agent that starts a run. |
| `reviewerType` | `admins` | `admins` (team admins), `team` (any team member), `custom` (members whose email is in `reviewerEmails`), or `none`. |
| `reviewerEmails` | None | Required when `reviewerType` is `custom`, and not allowed otherwise. At least one valid email, no duplicates. |
| `analysisModel` | The foreman's model | The model of the Self-improvement runs. |

### When a run starts

A failure is a non-passing score of a scorer with `selfImprovement: true`. Failures count per agent, one per run. A failure stays unreviewed until a Self-improvement run includes it.

The service checks an agent after each new failing score and every 15 minutes. It starts one Self-improvement run for the agent when:

- the agent has at least `failedRunThreshold` unreviewed failures, or
- its oldest unreviewed failure is at least 7 days old.

**Run now** in the **Self-improvement** section of the **Scorers** page starts a run for every agent with at least one unreviewed failure. You cannot choose the agents or the failures.

### What the run does

The Self-improvement run is a normal factory run of the foreman, with:

- the title `Self-improvement: <agent> (<n> failing runs)`;
- the `source` `SELF_IMPROVEMENT`;
- the metadata `self_improvement=true` and `self_improvement_agent=<agent>` (in the sandbox: `OF_METADATA_SELF_IMPROVEMENT` and `OF_METADATA_SELF_IMPROVEMENT_AGENT`).

Its prompt has the failing runs (at most the 25 most recent), the judge's labels and reasoning, transcript excerpts, the scorer rubrics and the relevant definition files. It asks for the smallest change that fixes the root cause:

- a change to the factory definition (instructions, skills, model, harness, or other `factory.yaml` settings), or
- a change to application code, when the repository causes the failures (missing scripts, broken tooling, unclear documentation).

The prompt forbids scorer changes that only hide failures. Delivery depends on the definition source:

| Source | Delivery |
|---|---|
| `git` | A pull request against the production branch of the definition repository. The `openfactory/definition` check validates it, and the merge applies it. |
| `managed` | The tool `propose_definition_change`, with the full new content of each changed file. The server stores a proposed version that a reviewer applies with one approval. |

Application-code changes are pull requests against the affected repository. Each pull request body has a `Regressions addressed` section that lists the failing runs and scorer results.

### Review

For a managed definition:

1. The agent calls `propose_definition_change` with the changed files, the files to delete and a message.
2. The server runs the compiler and the preflight on the full tree. On errors, the tool returns them, and the agent fixes the files and calls it again.
3. The server stores a proposed version in the state `proposed`, with a base version (the active version), the author run and the message.
4. The server creates an inbox item of kind `review` with the diff against the base version (three lines of context). The item goes to one eligible reviewer, chosen at random according to `reviewerType`. With `none`, or when nobody is eligible, it goes to the inbox of the factory's editors and admins.
5. The reviewer clicks **Approve and apply** or **Reject** in the inbox item, or in the **Self-improvement** section of the **Scorers** page, which also accepts a rejection reason. Both need the `editor` role.

An approval makes the proposed version the active version. If the base version is no longer active, the approval fails with `conflict`: reject the proposal and ask for a new one. A rejection sends the reason to the author run as a `user` message, so the agent can try again.

API: `GET /api/v1/factories/{uid}/definition/proposals` (`state=all` adds decided proposals), `POST .../proposals/{id}/approve`, and `POST .../proposals/{id}/reject` with `{"comment": "..."}`. `POST /api/v1/inbox/{id}/resolve` with the resolution `approve` or `reject: <reason>` does the same for the review item.

For a pull request (a git definition, or application code), the reviewer is chosen when the run starts. When the run ends a turn with a pull request linked to its work item, the service creates a `review` item with the link and asks that reviewer for a review on the pull request:

- This works on GitHub, GitLab (merge request reviewers) and Azure DevOps (a reviewer without a vote). The reviewer must have stored a [personal forge token](api.md#personal-forge-tokens), which the service uses to read the reviewer's account. On GitLab and Azure DevOps, the forge plugin does the work and must be installed.
- On GitLab, the reviewer is added to the existing reviewers. The personal token needs the `api` scope.
- If the reviewer has no linked account, or the forge or plugin cannot request a review, the item says so. Add the reviewer by hand.
- The service creates one item for each open pull request. A merged or closed pull request gets no item.

The **Self-improvement** section lists the pull requests of Self-improvement work items. Review them like human pull requests.

## Benchmarks

A benchmark compares configurations of one agent on the same fixed tasks. Use it to test a model or harness change before you apply it.

- A **suite** is a set of tasks for one agent.
- A **task** has a prompt, correctness criteria and, optionally, the commits to start from.
- A **configuration** pins the model or harness of one or more agents.
- A **trial** is one run of one task under one configuration. Repetitions create more trials.

### Define a suite

Put each suite in `benchmarks/<suite>/suite.yaml`, and each task in `benchmarks/<suite>/tasks/<task>.yaml`. From [`examples/02-sdlc-issue-to-pr`](../examples/02-sdlc-issue-to-pr/benchmarks/implement-quality):

```yaml
# benchmarks/implement-quality/suite.yaml
name: Implementation quality
description: Compare implement-agent configurations on representative web tasks.
agent: implement
configurations:
  - name: Baseline
    agents:
      - agent: implement
        model: claude-opus-5-5
  - name: Claude Code
    agents:
      - agent: implement
        harness:
          type: claude
          model: claude-opus-5-5
          auth:
            source: managedSecret
            secretName: ANTHROPIC_API_KEY
tasks:
  - fix-date-formatting
  - add-empty-state
```

```yaml
# benchmarks/implement-quality/tasks/add-empty-state.yaml
title: Add an empty state to the invoices list
prompt: When an account has no invoices, the list renders blank. Add an empty state with a call to action.
successCriteria: The empty state appears with no invoices, links to invoice creation, and has a snapshot test.
tags: [feature, frontend]
startingRepoRefs:
  - codeForge: GITHUB
    owner: acme
    repo: web-app
    ref: 89abcdef0123456789abcdef0123456789abcdef
```

**Suite fields**

| Field | Required | Rules |
|---|---|---|
| `name` | Yes | Unique in the factory. |
| `description` | No | |
| `agent` | Yes | The agent that runs every task. It must match `agents/<name>/agent.md`. |
| `configurations` | No | 1 to 6 configurations. Each has a unique `name`, an `agents` list and an optional `machine`. Each `agents` item names an `agent` (once per configuration) and sets exactly one of `model` or `harness`. Without `configurations`, the suite runs one configuration, `default`, with the current agent settings. |
| `tasks` | No | Task slugs. Each slug needs a file `tasks/<slug>.yaml`, and every task file must be listed. A suite needs at least one task to run. |

**Task fields**

| Field | Required | Rules |
|---|---|---|
| `title` | Yes | |
| `prompt` | Yes | The prompt of the trial run. |
| `successCriteria` | Yes | What a successful trial must do. The Correctness Scorer judges against it. |
| `sourceRunId` | No | The run that the task reproduces. |
| `tags` | No | Strings. |
| `startingRepoRefs` | No | Commits to start from: `github.com:OWNER/REPO@SHA`, `gitlab.com:OWNER/REPO@SHA`, or `{codeForge, owner, repo, ref}`. The commit must be a full 40-character lowercase SHA, not a branch or tag. |
| `trackerSeed` | No | The start state of the tracker for the trial, as any YAML value (for example issues and their states). The trial prompt shows it as the current tracker state. `linearSeed` is the older name. |

To run tasks against another repository, for example a sandbox fork, add `benchmarkRepoSubstitutions` to `factory.yaml`. Each item has a `source` (one of the factory's repositories) and a `target` (not one of them). The commits stay the same.

### Run a benchmark

In the dashboard, open **Benchmarks**, choose a suite, select the configurations, set the repetitions (1 to 10), and click **Run benchmark**. With the API:

```bash
curl -s -X POST "$OF_PUBLIC_URL/api/v1/factories/fac_123/benchmarks/implement-quality/runs" \
  -H "Authorization: Bearer $OF_API_KEY" -H "Content-Type: application/json" \
  -d '{"repetitions": 3, "scorers": ["tests-run"]}'
```

With the CLI (`--config` selects configurations of the suite; default all):

```bash
openfactory benchmark run payments implement-quality --repetitions 3 --scorer tests-run
openfactory benchmark list payments
```

| Body field | Meaning |
|---|---|
| `configurations` | Configurations to compare (each with `name`, `agents` and an optional `machine`). Empty means the suite's configurations. |
| `scorers` | Factory scorers (slug or ID) to apply to every trial, in addition to the Correctness Scorer. |
| `repetitions` | Trials per configuration and task. 1 to 10, default 1. |

Starting a benchmark needs the `editor` role. The number of trials is configurations × tasks × repetitions. More trials and scorers cost more inference.

Each trial is an isolated run of the suite agent, pinned to the model, harness and machine of its configuration. It starts from the task's commits (after substitutions) with the task prompt. Trial runs carry `benchmark_*` metadata, and production scorers skip them.

### Trial mode

A trial and its child runs run in trial mode (run metadata `trial_mode=true`):

- The write tools of the built-in integration servers (`linear_create_comment`, `jira_add_comment`, `slack_post_message`) do not call the provider. The call is recorded in the run transcript, where the judge sees it, and the tool answers as if the write worked. Read tools work as usual.
- `post_update` output stays on the trial work item. The trial source `benchmark` has no outbound service.
- A trial cannot propose a definition change.

Trial mode does not cover pull requests and pushes: a trial works on the repositories of its task. Use `benchmarkRepoSubstitutions` to point trials at sandbox forks.

### Cancel a benchmark

Click **Cancel benchmark** on the benchmark page, or call `POST /api/v1/benchmark-runs/{id}/cancel` (role `editor`). Unfinished trial and judge runs are cancelled, results scored before the stop are kept, and the state becomes `cancelled`. A finished benchmark cannot be cancelled (`409 conflict`).

### Make a task from a run

A finished run can become a benchmark task with fixed commits:

```bash
curl -s -X POST "$OF_PUBLIC_URL/api/v1/factories/fac_123/benchmarks/implement-quality/tasks" \
  -H "Authorization: Bearer $OF_API_KEY" -H "Content-Type: application/json" \
  -d '{"from_run_id": "run_abc", "success_criteria": "The empty state appears and has a test."}'
```

The task gets the run's prompt, `sourceRunId`, and the commits that the runner reported for each repository as `startingRepoRefs`. A run with a repository without a recorded commit cannot become a task. Optional fields: `slug`, `title` and `tags`.

For a managed definition, the server adds the task file and the suite entry, and applies the definition. For a git definition, the response has the files to commit, and nothing is applied.

### Trial and benchmark states

| Benchmark state | Meaning |
|---|---|
| `running` | Trials still run. |
| `scoring` | All trials finished. Judge runs still work. |
| `complete` | All trials finished and were scored. |
| `failed` | No trial could start. |
| `cancelled` | A person cancelled the benchmark. Only results scored before the stop are kept. |

A trial is `pending`, `running`, `finished`, `blocked`, `error` or `cancelled`. A trial whose run is idle and waits for a person is `blocked`. Its evidence is still scored, and each scorer decides whether it meets its criteria.

The benchmark stores its inputs (suite, configurations, repetitions, scorer snapshots) with its results, so later changes to the suite or scorers do not change past results.

### Correctness Scorer

The built-in Correctness Scorer scores every trial against the task's `successCriteria`, with the labels `correct` (score 1) and `incorrect` (score 0) and a passing score of 1. Its judge is a run, like the judge of a factory scorer. It reads the full trial transcript, including child runs, and the repositories at the trial's start commits. Its execution settings are `scorerDefaults` (harness, model, machine, secrets and MCP servers) over the harness of `agentDefaults`. Each trial score has `judge_run_id` and `judge_cost_usd`.

### Read the results

`GET /api/v1/benchmark-runs/{id}` returns the benchmark run. Its `results` hold the trials, a summary per configuration, and the recommendations. In `results`, `total_cost_usd` covers trials and judge runs, and `judge_cost_usd` is the judge part. The dashboard shows the same data.

| Summary field | Meaning |
|---|---|
| `trials`, `finished`, `blocked`, `errored` | Trial counts. |
| `scored`, `passed`, `pass_rate` | Correctness Scorer results. Judge errors are not counted. |
| `scorer_averages` | The mean label score for each scorer. |
| `overall` | The mean of `scorer_averages`. |
| `total_cost_usd`, `avg_cost_usd` | Trial cost, without judge runs. |
| `judge_cost_usd` | The cost of the judge runs of the trials. |
| `avg_duration_ms`, `median_duration_ms` | Total trial duration, not active agent time. |
| `input_tokens`, `output_tokens` | Token usage. |

| Recommendation | Rule |
|---|---|
| `overall` | The highest `overall` value. A tie goes to the lower cost. |
| `most_efficient` | The highest `overall` per dollar of average trial cost. |
| `lowest_cost` | The lowest average trial cost. |

A recommendation is empty when the results cannot support it, for example when fewer than two configurations are scored. `most_efficient` and `lowest_cost` are also empty when all configurations cost the same. Costs are estimates for comparison, not for invoice reconciliation.

### Apply a result

Change one setting at a time. If the evidence supports a candidate, change the agent's `model` or `harness` in the definition, and keep the relevant scorers active. Then compare new production scores with the baseline from before the change.

## A practical improvement loop

1. **Define a scorer.** Choose one agent and one observable failure. Write the rubric and labels. Score a few runs on demand and compare the results with your own review.
2. **Collect a baseline.** Set `samplingRate` and let automatic scoring run on normal work. Record the scorer settings, date range and costs.
3. **Inspect failures.** Read the judge's reasoning and the runs. Look for missing context, unclear instructions or missing tools. When a failure repeats, set `selfImprovement: true`.
4. **Benchmark a candidate.** Compare configurations on the same tasks, with enough repetitions to trust the difference.
5. **Review and adopt.** If the evidence supports the change, make it.
6. **Keep monitoring.** Compare new results with the baseline. When the criteria no longer fit, change the scorer or set `samplingRate: 0`.

## Related pages

- [Dashboard and inbox](dashboard.md)
- [Definition reference](definition-reference.md)
- [Agents](agents.md)
- [End-to-end SDLC use case](use-cases/end-to-end-sdlc.md)
