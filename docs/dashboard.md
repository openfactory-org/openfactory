# Dashboard and inbox

The dashboard is the web interface of Open Factory. Use it to send work, follow work items and runs, answer the inbox, edit managed definitions, and read metrics, scores and benchmarks.

The control plane serves the dashboard at the root of `OF_PUBLIC_URL`, for example `https://factories.example.com/`. It uses the same data and roles as the [REST API](api.md).

## Sign in

Open `${OF_PUBLIC_URL}/login` and enter one of your API keys (`of_<prefix>_<secret>`).

- If the server has OIDC single sign-on (`OF_OIDC_*`), the page also shows a single sign-on button. Your identity provider groups decide your role. See [OIDC login](self-hosting/oidc.md).
- If you have no key, bootstrap the server or ask an admin. See [Get the first key](api.md#get-the-first-key).
- For local development, `openfactory bootstrap` prints an admin API key once. See [Quickstart](quickstart.md).
- **Sign out** ends the session. See [Dashboard sign-in](security.md#dashboard-sign-in) for the cookie and its lifetime.

### Roles in the dashboard

The dashboard hides or disables each action that the API would refuse for your role. See [Roles and permissions](security.md#roles-and-permissions).

| Role | In the dashboard |
|---|---|
| `user` | Read each page. Send work, answer the inbox, send messages, and cancel or complete work items and runs. |
| `editor` | Also edit agents, automations, webhooks, definitions and factory settings, attach integrations to a factory, score runs and start benchmarks. A team-wide editor can also create factories and edit secrets, environments and MCP servers. |
| `admin` | Also delete factories and manage integrations. |

A factory role applies to the pages of that factory. Team pages (secrets, environments, MCP servers, integrations) use the team-wide role.

## Layout

The top of the sidebar has the team menu (theme, keyboard shortcuts, **About**, **Sign out**), the search button and **New work item**.

**Work pages** cover all factories of your team:

| Page | Path | Contents |
|---|---|---|
| New work item | `/chat` | Choose a factory and send a request to the foreman. The sidebar button opens the same form as a dialog. |
| Inbox | `/inbox` | Items that wait for you, in all factories. The badge shows the count. |
| Runs | `/runs` | All runs of the team. |
| Factories | `/factories` | All factories. **+** opens `/factories/new`. |

**Factory pages** are under each factory, at `/f/{id}/...`. Click a factory to show or hide them:

| Group | Page | Path | Contents |
|---|---|---|---|
| | Overview | `/f/{id}` | Metrics, scorer cards and recent work. A new factory shows the first steps. |
| | Work items | `/f/{id}/work` | Work items grouped by stage, with filters. |
| | Inbox | `/f/{id}/inbox` | This factory's items that wait for you. |
| | Runs | `/f/{id}/runs` | This factory's runs. |
| Configure | Agents | `/f/{id}/agents` | Agents and their effective configuration. |
| Configure | Automations | `/f/{id}/automations` | Automations and their triggers. |
| Configure | Webhooks | `/f/{id}/webhooks` | Custom webhooks and their deliveries. |
| Configure | Factory definition | `/f/{id}/definition` | Definition files and version history. Managed factories only. |
| Configure | Integrations | `/f/{id}/integrations` | The team integrations that this factory uses. |
| Configure | Settings | `/f/{id}/settings` | Identity, repositories, machines, definition source and deletion. |
| Measure | Scorers | `/f/{id}/scorers` | Scorers, recent results and self-improvement. |
| Measure | Benchmarks | `/f/{id}/benchmarks` | Benchmark suites and runs. |

**Team settings** is closed by default:

| Page | Path | Contents |
|---|---|---|
| Secrets | `/secrets` | Team and factory secrets. Values are write-only. See [Secrets](secrets.md). |
| Integrations | `/integrations` | Slack, Microsoft Teams, Linear, Jira and code-host connections. See [Integrations](integrations/README.md). |
| Environments | `/environments` | Environments for agent runs. See [Machines and environments](machines-and-environments.md). |
| MCP servers | `/mcp-servers` | The team MCP server registry and Factory MCP sign-ins. See [MCP servers](mcp-servers.md). |
| Workers | `/workers` | Workers, their status, slots in use, labels, version and last connection, with drain, resume and revoke. See [Self-hosted workers](self-hosted-machines.md). |

## Keyboard shortcuts

Press `?` to show the list. Shortcuts do not operate while you type in a field or while a dialog is open.

| Keys | Action |
|---|---|
| `Cmd+K` (macOS) or `Ctrl+K` | Open the command palette to find a page or an action. |
| `C` | Open the **New work item** dialog. |
| `/` | Go to the page's search field (or open the palette if there is none). |
| `G` then `I`, `R` or `F` | Go to the inbox, the runs or all factories. |
| `G` then `W` or `O` | Go to the work items or the overview of the current factory. |
| `J` and `K` | Move down and up in a list. `Enter` opens the selected row. |
| `Cmd+Enter` or `Ctrl+Enter` | Send the message or request that you typed. |
| `Cmd+S` or `Ctrl+S` | Save the file in the definition editor. |

## Send work

Open **New work item** (sidebar button, `C`, or the button on a factory's **Overview** and **Work items** pages). Choose the factory, type the request and click **Send to foreman**. The dashboard creates the work item and opens it. The first line of the request becomes the title (at most 80 characters).

## Overview page

A factory without work items shows **Get started**: connect a code host, review the agents and send the first request.

Then the page shows the factory's metrics for the last 7, 30 or 90 days (default 30): work items by stage, pull requests opened and merged, autonomy, cycle time, human wait time, runs, cost and tokens, and daily activity. Each scorer has a card with its pass rate and label breakdown.

- **Autonomy** is the share of merged pull requests that no person opened or pushed to before the merge. **Work item autonomy** is the share of completed work items that needed no person.
- **PR cycle time** is the median time from work item start to merge. The **Pull request cycle** card splits it: start to pull request, pull request to first review, first review to merge.
- **Cost by PR size** groups pull request cost by changed lines.
- **Compute time** is the sandbox time of the runs. **Compute by instance shape** groups it by machine size.
- Pull request figures come from forge events and start when the forge is connected. The page says how many earlier merged pull requests are not counted.

Use the metrics to choose which runs to look at, not to prove what caused a change. See [Metric definitions](measure-and-improve.md#metric-definitions).

## Work items

A work item page (`/f/{id}/work/{workItemId}`) shows:

- **Source**: issue, pull request, chat thread, webhook delivery, schedule, API or MCP request.
- The current stage.
- **Waiting on a person**: open inbox items.
- **Pull requests**: linked pull requests and their state.
- **Runs**: the foreman run and its child runs.
- **Activity**: stage changes, messages, approvals and other events, in order.

Actions: **Message the foreman** (the foreman continues its conversation), **Cancel** (the work item and its active runs) and **Mark complete**.

## Runs

A run page (`/runs/{runId}`) shows the prompt, details (agent, state, harness, model, usage), result, **Sub-agents** (child runs) and **Transcript**. The transcript updates live while the run is active.

Actions:

- **Follow up**: the agent continues the same session. See [Follow-ups and cancellation](api.md#follow-ups-and-cancellation).
- **Stop run**: cancel the run.
- **Score this run**: score the run now with a scorer of the factory. See [Scorers](measure-and-improve.md#scorers).

## Inbox

The inbox collects requests that wait for a person: `/inbox` for all factories, `/f/{id}/inbox` for one. The item kinds and API are in [Work items and the inbox](api.md#work-items-and-the-inbox).

### Resolve an item

| Kind | Label | How to resolve |
|---|---|---|
| `question` | Question | Type an answer and click **Answer**, or choose an option. The recommended option is marked "(recommended)". A multi-choice question shows check boxes. |
| `approval` | Approval | Click an option button, for example **approve** or **revise**. |
| `blocked` | Waits for input | Type an answer and click **Answer**. |
| `handoff` | Handoff | Review the pull request at the forge. The item closes when you submit a review or the pull request merges. Or click **Mark resolved**. |
| `failure` | Failure | Look at the run, then click **Mark resolved**. To retry, send a follow-up message to the run. |
| `review` | Review | For a definition change, read the diff and click **Approve and apply** or **Reject**. For a self-improvement pull request, review it at the forge, then click **Mark resolved**. |
| `answered`, `spend_limit` | For your information, Spend limit | Click **Dismiss**. |

When you resolve a question, blocked item or approval, or decide a definition change, the engine sends your answer to the waiting agent, which continues from where it stopped. The message names you by your name or email.

When you resolve a handoff, a failure or a self-improvement pull request review, or dismiss a "For your information" item, the engine sends nothing to the agent and starts no new agent turn. The resolve only closes the item. To retry after a failure or to give the agent more work, send a follow-up message to the run or a message on the work item.

**Undo** shows for 10 minutes after a resolve, while the agent has not read the answer. The agent then gets a message that withdraws the answer, and the item opens again. A handoff closed by a review or merge cannot be undone.

### Who sees an item

The inbox is a personal queue:

- An item appears in your inbox when you are a recipient. The default recipient is the creator of the work item. Self-improvement review items go to one reviewer (see [Self-improvement](measure-and-improve.md#self-improvement)).
- An item without recipients (for example from a webhook without a linked user) goes to the factory's editors and admins.

### Read state and filters

A new item is marked **New**, with a line on its left. Opening it or **Mark as read** marks it read for you. **Mark as unread** makes it new again.

| Filter | Values |
|---|---|
| Scope | **Waiting on me** (default) or **Whole team** (editors and admins). |
| Factory | One factory (team inbox only). |
| Kind | One kind. |
| Search | Titles and descriptions. |
| Show resolved | Include resolved items, with their resolution and resolver. |

Each item links to its run (**View run**) and work item (**View work item**). You can also resolve items from Slack when the Slack integration is connected.

## Agents, automations and webhooks

- **Agents** lists the agents. An agent page shows its instructions and effective configuration. In a managed factory, you can edit it.
- **Automations** lists automations and triggers. In a managed factory, you can create, edit, validate and delete them. **Test filter** checks a webhook payload filter against a stored delivery or a pasted body, without starting a run. See [Automations](automations.md).
- **Webhooks** lists the factory's custom webhooks (at most 20). You can create one, copy its ingress URL, enable, disable or delete it, and read **Recent deliveries**. See [Webhooks](webhooks.md).

> **Note:** If the factory definition lives in a git repository, Agents, Automations, Webhooks and Settings are read-only. Change them with a pull request to the production branch.

## Factory definition

Managed factories only. Browse, edit and validate the files, then save. Saving validates the whole tree and applies all changes as one new version. If the tree is not valid, the dashboard shows the errors with file and line, and the active version does not change.

**Version history** lists each version. Open one to see its diff, and click **Restore this version** to apply it again. See [Definition reference](definition-reference.md).

## Scorers and benchmarks

- **Scorers** shows each scorer and a **Recent results** table for the last 30 days: run, scorer, agent, classification, score and the judge's reasoning. The **Self-improvement** section lists proposed definition changes, self-improvement pull requests and improvement runs. Editors can click **Run self-improvement now**. See [Measure and improve](measure-and-improve.md).
- **Benchmarks** lists the definition's suites. Choose a suite, configurations and repetitions (1 to 10), then click **Start benchmark**. A benchmark run page shows the trials, per-configuration results and recommendations.

## Settings

- **Identity**: name, **Foreman name** (the alias for @-mentions) and description.
- **Repositories**: the repositories that the factory works on.
- **Pull request authorship**: the `credentialStrategy` (`EXECUTOR` or `CREATOR`).
- **Machines**: the machines of the definition.
- **Definition source**: `managed`, or `git` with a repository, production branch (default `main`) and optional path.
- **Delete factory**: type the factory name to confirm. You cannot undo a deletion.
