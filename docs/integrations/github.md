# GitHub

A factory starts work from GitHub issues, pull requests, reviews, pushes and CI results, and replies on GitHub. Agents use the same credential to clone repositories, push branches and open pull requests.

You need owner or admin access to the GitHub organization or repositories. For the shared setup steps and variables, see [Integrations](README.md#common-setup).

Use a GitHub App for an organization or many repositories: its tokens are short-lived and limited to the installation. Use a personal access token to test with a few repositories, or if you cannot create an App.

## Connect with a GitHub App

### 1. Create the App

1. Open **Settings** > **Developer settings** > **GitHub Apps** > **New GitHub App** (for an organization, in the organization settings).
2. Enter a name. The App slug becomes the bot login `<slug>[bot]`. The slug is in the URL of the App page.
3. Set **Homepage URL** to `${OF_PUBLIC_URL}`.
4. Under **Webhook**, select **Active**. Set **Webhook URL** to `${OF_PUBLIC_URL}/integrations/github/webhook`.
5. Set **Webhook secret** to a long random value.
6. Set the repository permissions and subscribe to the events in the tables below.
7. Click **Create GitHub App**. Write down the **App ID**.
8. Under **Private keys**, click **Generate a private key**. Keep the `.pem` file.

| Repository permission | Access | Used for |
| --- | --- | --- |
| Metadata | Read | Required by GitHub. |
| Contents | Read and write | Clone repositories, read definition files, push branches. |
| Pull requests | Read and write | Open pull requests, read changed files for `paths` filters, comment. |
| Issues | Read and write | Comment on issues and pull requests, add `factory:<alias>` labels. |
| Checks | Read and write | Receive check suite and check run events. Publish check runs, such as the `openfactory/definition` check. |
| Commit statuses | Read | Receive `status` events. |
| Actions | Read | Receive `workflow_run` events. Agents can read workflow results. |
| Workflows | Read and write | Optional. Lets agents push changes to `.github/workflows`. |

| GitHub event | Open Factory events |
| --- | --- |
| Issues | `issue_created`, `issue_labeled`, `issue_assigned`, `issue_mentioned` |
| Issue comment | `issue_commented`, `pull_request_commented`, `issue_mentioned`, `pull_request_mentioned` |
| Pull request | `pull_request_opened`, `pull_request_ready`, `pull_request_reopened`, `pull_request_synchronized`, `pull_request_labeled`, `pull_request_assigned`, `pull_request_closed`, `pull_request_merged`, `pull_request_enqueued`, `pull_request_dequeued`, `pull_request_review_requested`, `pull_request_mentioned`, definition checks |
| Pull request review | `pull_request_review_submitted`, `pull_request_mentioned` |
| Pull request review comment | `pull_request_commented`, `pull_request_mentioned` |
| Push | `push`, definition sync |
| Check suite | `check_suite_completed`, `check_suite_rerequested` |
| Check run | `check_run_rerequested` |
| Workflow run | `workflow_run_completed` |
| Status | `commit_status` |
| Installation (sent to every App) | Record status (see [Repository scope and status](#repository-scope-and-status)) |

### 2. Install the App

1. On the App page, click **Install App** and select the organization.
2. Select **All repositories** or **Only select repositories**.
3. Open the installation settings. The URL ends in `/installations/<id>`. Write down this installation ID.

### 3. Store the credentials and create the record

```bash
openfactory secret set GITHUB_APP_PRIVATE_KEY < app.private-key.pem
openfactory secret set GITHUB_WEBHOOK_SECRET

curl -X POST "$OF_PUBLIC_URL/api/v1/integrations" \
  -H "Authorization: Bearer $OF_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "provider": "github",
    "name": "acme",
    "config": {
      "org": "acme",
      "app_id": "123456",
      "installation_id": "45678901",
      "private_key_ref": "GITHUB_APP_PRIVATE_KEY",
      "webhook_secret_ref": "GITHUB_WEBHOOK_SECRET",
      "app_slug": "acme-factory"
    }
  }'
```

`org` is the account where the App is installed. Make one record for each installation. Without `app_slug`, a mention of the App starts no work.

### 4. Connect a factory

```yaml
repositories:
  - owner: acme
    name: api
  - owner: acme
    name: web
```

`codeForge` defaults to `GITHUB`. Then add automations with `provider: github` triggers. See [Automations](../automations.md#github-github).

### 5. Test the connection

1. In the App settings, open **Advanced** > **Recent Deliveries**. The first `ping` got `401`, because the record did not exist yet. Click **Redeliver**. It must now get `200` with `{"status":"pong"}`. The server checks the `ping` with the webhook secret of a record that has the same `app_id`.
2. Open a test issue in a connected repository that matches a trigger.
3. Make sure that a work item appears in the dashboard.

## Connect with a personal access token

1. Create a fine-grained personal access token for the repositories, with the repository permissions in the App table. Give it **Commit statuses** read and write. A fine-grained token cannot have **Checks**, so the server publishes commit statuses (such as `openfactory/definition`) in place of check runs.
2. Store the token and a webhook secret:

   ```bash
   openfactory secret set GITHUB_TOKEN
   openfactory secret set GITHUB_WEBHOOK_SECRET
   ```

3. Create the record:

   ```bash
   curl -X POST "$OF_PUBLIC_URL/api/v1/integrations" \
     -H "Authorization: Bearer $OF_API_KEY" \
     -H "Content-Type: application/json" \
     -d '{
       "provider": "github",
       "secret_ref": "GITHUB_TOKEN",
       "config": {"org": "acme", "webhook_secret_ref": "GITHUB_WEBHOOK_SECRET"}
     }'
   ```

4. In each repository (or the organization), open **Settings** > **Webhooks** > **Add webhook**. Set:
   - **Payload URL**: `${OF_PUBLIC_URL}/integrations/github/webhook`.
   - **Content type**: `application/json` (form-encoded is not supported).
   - **Secret**: the webhook secret.
   - **Let me select individual events**: the events in the App table.
5. Open **Recent deliveries**. The `ping` must get `200` with `{"status":"pong"}`.
6. Start a run on a repository. If the clone fails with `Invalid username or token`, the token is wrong, expired or revoked. Store a new one with `openfactory secret set GITHUB_TOKEN`.

Agents act as the token owner. Comments and pull requests show that user as the author.

## Integration record reference

Provider: `github`. A record needs `org` and a credential: a token in `secret_ref`, or an App (`app_id` with its key). The server has no global GitHub credential.

| Key | Description |
| --- | --- |
| `config.org` | Necessary. The organization or user that the record serves. See [Record selection](README.md#record-selection). |
| `secret_ref` | A personal access token. If `app_id` is set and `private_key_ref` is empty, the App private key. |
| `config.app_id` | The GitHub App ID. Turns on App authentication. |
| `config.private_key_ref` | The team secret that holds the App private key (PEM). |
| `config.installation_id` | The App installation ID. If empty, the server finds the installation for `org`. |
| `config.app_slug` | The App slug. Turns on `@<slug>` mentions, and lets `skip_bots: self` find the activity of the App. |
| `config.webhook_secret_ref` | The team secret that holds the webhook secret. |
| `config.base_url` | The REST API root. Default `https://api.github.com`. For GitHub Enterprise Server, `https://HOST/api/v3`. |
| `config.web_url` | The web and git host root. The default comes from `base_url`. |
| `config.oauth_client_id`, `config.oauth_client_secret_ref` | The App client ID and the team secret with its client secret. They turn on the GitHub sign-in for [account links](README.md#account-linking). Set the App callback URL to `${OF_PUBLIC_URL}/integrations/link-callback`. |

### Repository scope and status

- A run gets an installation token for the repositories of the run only. A personal access token cannot be limited this way. See [Credential isolation between factories](README.md#credential-isolation-between-factories).
- A delivery selects the record whose `org` is the repository owner. A delivery with no repository uses the records of its `installation_id`.
- When the App is removed or suspended (`installation` event), each record with that `installation_id` becomes `disabled`. When it is unsuspended, they become `active` again.
- When GitHub refuses the App key, the installation or the token, the record becomes `auth_failed`. See [Status](README.md#status).

## Route work to a factory

Each event goes to the factories whose `repositories` include the repository. To select one factory:

- **Label.** Add `factory:<alias>` to the issue or pull request. All its events then go to that factory. The match ignores case.
- **Alias mention.** Mention `@<alias>` in a new issue or pull request body, comment or review. If no automation matches, the foreman starts work.
- **App mention.** Mention `@<slug>` (needs `app_slug`). If the author is linked and the team has one factory, the server adds that factory's label and starts work. If the team has several factories, the server comments with the labels to choose from.

Only new content counts as a mention. Edits and mentions in code blocks do not count. One comment that mentions the factory gives the foreman one message, not one for each event type.

GitHub adds these rules to [Who can start work](README.md#who-can-start-work):

- **Onboarding notice.** A mention from an unlinked account, or from a linked user outside the team, starts no work. If `automations.githubOnboardingNotices` is `true` (the default), the server comments once per account with instructions: a link to connect the account, or a request for team access. See [automations (factory settings)](../definition-reference.md#automations-factory-settings).
- **Bots.** Comments of the Open Factory app are dropped. Comments of other bots go to the foreman with a bot mark.

## How the factory responds

- The factory comments on the issue or pull request. A reply to a review comment goes into that review thread.
- Reviews, merges and closes of a linked pull request reach the foreman, also a review without a body (such as an approval). An empty `commented` review does not: GitHub sends one with each inline comment, which already reaches the foreman. See [Merges and reviews](../agents.md#merges-and-reviews).
- For a review comment, the agent workspace gets the comment, its diff hunk, all comments of the review thread and the whole diff of the file, under `.openfactory/events/<message_id>/context/`. GitHub gives no diff for a binary or very large file; the prompt then lists the diff as left out. See [What the agent gets from the event](../automations.md#what-the-agent-gets-from-the-event).
- Branch protection and required reviews still apply.

Filters do not limit access: the App installation or the token does. A filter for `main` does not limit the token to `main`.

## Definition sync and checks

A factory can keep its definition in a GitHub repository:

```bash
openfactory factory create --name acme --source git \
  --git-repo acme/factory-config --git-branch main --git-path factory
```

- **Sync.** A push to the production branch (`--git-branch`, default `main`) applies the definition. If it is not valid, the last valid version stays active.
- **Check.** A pull request that changes definition files gets the check `openfactory/definition` (with `--git-path`: `openfactory/definition (<git_path>)`). It validates the change and lists the resources that the merge creates, updates and deletes. With an App it is a check run; with a token it is a commit status.
- **Unchanged definitions.** Other pull requests pass at once with `No factory definition changes`, so you can make the check required.
- **Branches.** Checks run only for pull requests to the production branch. To check all branches, set this in `factory.yaml` on the production branch:

  ```yaml
  github:
    factoryConfigChecks: all
  ```

## Troubleshooting

| Problem | Check |
| --- | --- |
| Deliveries get `401` `invalid_signature` | No record has `org` equal to the repository owner, or the webhook secret differs from the secret in `webhook_secret_ref`. |
| Deliveries get `409` `ambiguous_integration` | Records of several teams share the webhook secret. Give each team its own secret. |
| An action fails with `no github integration record for the owner "x"` | Add a record with `"org": "x"`. |
| An event does not start work | The repository is in `repositories`, the GitHub event is subscribed, and a trigger matches. Bot events are skipped unless `skip_bots` is `self` or `none`. |
| A `paths` filter does not match | The App or token needs the Pull requests permission. |
| A label or author filter on a CI event does not match | The CI run must have exactly one linked pull request. |
| A mention does not start work | Use `@<alias>` or the `factory:<alias>` label. For `@<slug>`, set `app_slug`. Link your GitHub account, and be a member of the factory's team. |
| An agent cannot push or open a pull request | Check the Contents and Pull requests permissions and the branch protection rules. |
| The definition check does not appear | The factory must use a `git` source on this repository. The App needs Checks write access; a token needs Commit statuses write access. |

## Related pages

- [Automations and triggers](../automations.md)
- [Integrations](README.md)
- [Custom webhooks](../webhooks.md) (`github` signature scheme for repositories outside a factory)
