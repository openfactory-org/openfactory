# Azure DevOps

A factory starts work from pull requests, work items and mentions in Azure DevOps Services, and replies in pull request threads and work item comments. For Azure DevOps Server, see [Other code forges](other-forges.md).

Azure DevOps support is the `openfactory-plugin-azuredevops` plugin. The server image and the Helm chart load it; for other installs, see [Plugins](../self-hosting/plugins.md). To create service hooks, you need Project Collection Administrator access, or Project Administrator access to each project. For the shared setup steps and variables, see [Integrations](README.md#common-setup).

## Identities

The factory acts as an Azure DevOps user (the factory identity). Comments, branches and pull requests show that user as the author. Give the identity access to the projects and repositories of the factory. Activity by these identities never matches a trigger:

| Identity | Purpose | Config key |
| --- | --- | --- |
| Factory identities | One or more users that act for factories. A mention of one routes to its factory. | `factory_identities` |
| Service identities | Other automation users. | `service_identities` |

## Connect Azure DevOps

### 1. Create the access token

1. Sign in to `https://dev.azure.com/<org>` as the factory identity.
2. Open **User settings** > **Personal access tokens** > **New Token**.
3. Select the organization and an expiry date.
4. Under **Scopes**, select **Custom defined**, then:
   - **Code**: Read & write, and Status.
   - **Pull Request Threads**: Read & write.
   - **Work Items**: Read & write.
5. Click **Create** and copy the token.

### 2. Find the identity IDs

Mentions and the `mentioned` filter use identity IDs (UUIDs). The `id` field of this result is the identity ID:

```bash
curl -u ":$ADO_PAT" \
  "https://vssps.dev.azure.com/<org>/_apis/identities?searchFilter=General&filterValue=factory@acme.com&api-version=7.1"
```

### 3. Store the credentials and create the record

```bash
openfactory secret set ADO_PAT              # the access token
openfactory secret set ADO_HOOK_PASSWORD    # a long random value

curl -X POST "$OF_PUBLIC_URL/api/v1/integrations" \
  -H "Authorization: Bearer $OF_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "provider": "azure_devops",
    "name": "acme",
    "secret_ref": "ADO_PAT",
    "config": {
      "org": "acme",
      "hook_username": "openfactory",
      "hook_password_ref": "ADO_HOOK_PASSWORD",
      "factory_identities": "6b1f3c2a-1d4e-4f5a-9b8c-7d6e5f4a3b21=builder"
    }
  }'
```

### 4. Add the service hooks

Add one Web Hooks subscription for each event type, in each project:

1. Open **Project settings** > **Service hooks** > **+** (Create subscription).
2. Select **Web Hooks**, then click **Next**.
3. Select the event type and the filters (repository, branch), then click **Next**.
4. Set **URL** to `${OF_PUBLIC_URL}/plugins/azuredevops/webhook`.
5. Set **Basic authentication username** to `hook_username` and **Basic authentication password** to the value of `ADO_HOOK_PASSWORD`.
6. Set **Resource details to send** to **All**.
7. Click **Test**, then **Finish**.

| Service hook event | Open Factory events |
| --- | --- |
| Pull request created | `pull_request_created`, `pull_request_mentioned` |
| Pull request updated | `pull_request_updated`, `pull_request_merged` (completed), `pull_request_closed` (abandoned) |
| Pull request merge attempted | `pull_request_merged` (when the pull request is completed) |
| Pull request commented on | `pull_request_commented`, `pull_request_mentioned` |
| Work item created | `work_item_created`, `work_item_mentioned` |
| Work item updated | `work_item_assigned`, `work_item_labeled`, `work_item_commented`, `work_item_mentioned` |
| Work item commented on | `work_item_commented`, `work_item_mentioned` |
| Code pushed | `push` |

`work_item_commented` and `push` cannot start automations. A comment continues the active work item.

### 5. Connect a factory

```yaml
codeForges: [AZURE_DEVOPS]
repositories:
  - owner: Payments
    name: payments-api
```

`owner` is the project name and `name` is the repository name. The organization comes from the record.

Then add automations with `provider: azure_devops` triggers. The `repos` filter takes repository UUIDs:

```bash
curl -u ":$ADO_PAT" "https://dev.azure.com/<org>/<project>/_apis/git/repositories?api-version=7.1"
```

```markdown
---
triggers:
  - provider: azure_devops
    event: pull_request_created
    filter:
      repos: ["3f2a1b4c-5d6e-4f70-8a9b-0c1d2e3f4a5b"]
      base_branches: [main]
---

A pull request was opened against main. Review it and post your findings
in a pull request thread.
```

See [Automations](../automations.md#azure-devops-azure_devops) for all events and filter keys.

## Integration record reference

Provider: `azure_devops`. A record needs `org` and `secret_ref`. See [Record selection](README.md#record-selection).

| Key | Description |
| --- | --- |
| `config.org` | Necessary. The organization name. Deliveries from this organization use this record. |
| `secret_ref` | Necessary. The personal access token of the factory identity. |
| `config.base_url` | The service root. Default `https://dev.azure.com`. |
| `config.hook_username` | The basic authentication user name of the service hooks. If empty, any user name is accepted. |
| `config.hook_password_ref` | The team secret that holds the service hook password. |
| `config.factory_identities` | Factory identities as comma-separated `identityID=alias` pairs. |
| `config.service_identities` | Other identity IDs, comma-separated. |

A repository in `factory.yaml` (`project/name`) does not name the organization. For its own calls, the server uses the record of the factory, else the team record, when there is exactly one. If a team has records for several organizations, write the repository as `org/project/name`.

A token reaches every project that it can reach. To keep factories apart, see [Credential isolation between factories](README.md#credential-isolation-between-factories).

## Mention the factory

Mention an identity from `factory_identities` in a pull request or work item description or comment. The event goes to its factory. If no automation matches, the foreman starts work. Mentions by a factory or service identity are ignored.

## How work items are routed

Pull request events go to the factories whose `repositories` include the repository. Work item events have no repository. The server routes them in this order:

1. A mention of a factory identity, or an assignment to one, goes to the factory of that identity.
2. Else, if the record that receives the delivery has a `factory_id`, the event goes to that factory. Intake uses the team record when the team has one.
3. Else, the event goes to each factory that has a repository in the project of the work item. Each factory matches the event against its automations.

## How the factory responds

- The factory posts in a pull request thread or as a work item comment. A reply to a pull request comment goes into the thread of that comment.
- For a comment in a thread, the agent workspace gets the whole thread and the diff of the file (`context/review_thread.json`, `context/file_diff.patch`). The plugin builds the diff from the file at the merge base and at the head of the pull request. A binary file or a file over 1 MiB has no diff; the prompt then lists it as left out.
- When a Self-improvement change is a pull request, the plugin adds the chosen reviewer as an optional reviewer, without a vote. The reviewer needs a [personal forge token](../api.md#personal-forge-tokens).
- When the source of a work item is an Azure DevOps work item, or `link_issue` added one, the engine writes back to it. See [Write-back to the tracker](../agents.md#write-back-to-the-tracker).
- Branch policies and required reviewers still apply.

## Troubleshooting

| Problem | Check |
| --- | --- |
| Deliveries or service hook tests get `401` `unauthorized` | No record has `org` equal to the organization, or the user name or password differs from `hook_username` and `hook_password_ref`. |
| Deliveries get `409` `ambiguous_integration` | Records of several teams share the hook credentials. Give each team its own password. |
| A pull request event does not start work | The repository is in `repositories` (project and repository name), and `repos` uses the repository UUID. |
| A mention does not start work | The mentioned identity must be in `factory_identities`. |
| A work item event does not start work | Assign the work item to a factory identity, give the record a `factory_id`, or add a repository of the project to the factory. |
| The factory cannot comment or push | Check the token scopes and the project permissions of the identity. |

## Related pages

- [Automations and triggers](../automations.md)
- [Integrations](README.md)
- [Other code forges](other-forges.md)
