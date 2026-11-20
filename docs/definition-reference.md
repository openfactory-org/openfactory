# Definition reference

A factory is declared as code in a **definition tree**: YAML and Markdown files rooted at `factory.yaml`. This page lists every file kind and field, with types, defaults and validation rules. The definition compiler (`internal/definition`) is the source of truth: when this page and the compiler disagree, the compiler wins.

## Where the definition lives

Each factory has one definition source, selected when you create it.

| Source | Where the files are | How a change is applied |
|---|---|---|
| `managed` (default) | Stored in the Open Factory database. | Edit in the dashboard, or send the complete file tree to `PUT /api/v1/factories/{uid}/definition`. |
| `git` | A repository (`git_repo`), a production branch (`git_branch`, default `main`) and an optional subdirectory (`git_path`) that holds `factory.yaml`. | Merge to the production branch. The push (a GitHub push webhook) syncs the definition. |

Every apply is atomic:

1. The compiler checks the complete tree.
2. The [preflight](#preflight) checks the tree against the team.
3. On errors, the last valid version stays active:
   - Compiler errors in a `PUT` of a `managed` definition: the API returns the errors and stores nothing.
   - Preflight errors in a `PUT`, or any errors in a `git` sync: the version is stored as invalid, with its errors.
4. Without errors, one transaction writes the version, the agents, automations, scorers, webhooks, runtime settings, integrations and the factory record, and activates the version. If one write fails, nothing changes.

An apply updates each resource by its ID and deletes only the resources that are gone. A webhook gets a random ID, and so its URL, at its first apply, and keeps both while its file name stays. A webhook that you remove and add again gets a new ID and URL.

### Preflight

The compiler checks the files; the preflight checks them against the team. Apply, the validate endpoint and the pull request check run both.

| Check | Result if it fails |
|---|---|
| Each secret exists in the team or the factory: `secrets` lists, `harness.auth.secretName`, and webhook `secretName`. | error |
| Each `environment` exists in the team. | error |
| A machine sets `registryCredentialSecretName` (the `kubernetes` backend ignores it), or sets `os` to `macos` or `windows` (only Linux sandboxes exist). | warning |
| Each `workerHost` is a built-in host (`server`, `kubernetes`, `direct`, `managed`), a sandbox backend of the server, or the ID or name of a registered worker. | error |
| Each `managedId` of an MCP server names a registered MCP server of the team. | error |
| A webhook keeps `authMode` and `signatureScheme` after its first apply. To change them, remove the webhook, apply, then add a new one. | error |
| The secret of a `url_token` webhook is safe for a URL: letters, digits, `-`, `.`, `_` and `~`. | error |
| Each name in an automation filter (`channels`, `users`, `itemUsers`, `teams`, `projects`, `states`, `issues`, `statuses`) names exactly one object. | error |
| Each `runAs` of an automation names a principal of the team: `user:<email>` a person, `serviceAccount:<name>` a service account. | error |
| Each entry of `approvers` is the email of a person of the team, or the name of a service account of the team. | error |
| A person in `runAs` of an automation whose agent uses `credentialStrategy: CREATOR` has a linked forge account. | warning |
| Each `model` is in the model list of its harness. The lists are advisory: an unlisted name can still work. | warning |
| Each agent has a harness credential: `harness.auth`, or exactly one factory or team secret of a model credential type (see [Default credential](harnesses.md#default-credential)). | warning |

The server resolves filter names through the integration's API, with the integration record's credential:

| Provider | Names | Notes |
| --- | --- | --- |
| Slack | `channels`, `users`, `itemUsers` | The bot token needs the scopes `channels:read`, `groups:read`, `users:read` and `users:read.email`. |
| Linear | `teams`, `projects`, `states`, `issues` | See [Names in trigger filters](integrations/linear.md#names-in-trigger-filters). |
| Jira | `projects` (to `project_keys`), `statuses` (to `status_ids`) | The Jira account must be able to see the projects. See [Names in trigger filters](integrations/jira.md#names-in-trigger-filters). |
| Microsoft Teams | `channels`, `users` | The server looks in the team of `team_ids`. See [Names in trigger filters](integrations/teams.md#names-in-trigger-filters). |

Name matching ignores case. The server caches each list for a few minutes and rereads it for an unknown name at most once in 30 seconds. After an API error, it waits 30 seconds before the next call and uses the last list for up to one hour.

- When the names resolve, the apply stores the provider IDs (for example `team_ids`) in place of the names. Linear `projects`, `states` and `issues`, and Teams `channels` and `users`, are looked up within the team of the same trigger (`teams` or `team_ids`).
- When the integration is not connected or the list fails, the preflight gives a warning with the reason, and each event compares the names with the current names.
- When a check cannot run now (for example, the secret store is not available), the validate response lists it under `skipped` with the reason.

### Create a factory from a definition

```bash
curl -X POST "$OF_PUBLIC_URL/api/v1/factories" \
  -H "Authorization: Bearer $OF_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"definition": {"files": {"factory.yaml": "...", "agents/foreman/agent.md": "..."}}}'
```

The request body also accepts:

| Field | Meaning |
|---|---|
| `name`, `alias`, `description` | Factory identity. If you send a definition, empty values are taken from `factory.yaml`. |
| `source` | `managed` (default) or `git`. |
| `git_repo`, `git_branch`, `git_path` | Required `git_repo` for the `git` source. `git_branch` defaults to `main`. |
| `files` | A complete tree (path to content). It takes precedence over `definition`. |
| `definition` | The string `"defaults"`, or an object `{"files": {...}}`. |
| `repositories`, `integrations`, `agents`, `model` | Inputs for the generated default definition (see [Default definition](#default-definition)). |

From a local directory: `openfactory factory create --from ./my-factory`.

### Update a managed definition

```bash
curl -X PUT "$OF_PUBLIC_URL/api/v1/factories/$FACTORY_ID/definition" \
  -H "Authorization: Bearer $OF_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"files": {...}, "message": "Add docs agent", "base_version_id": "defv_..."}'
```

- `files` is required and replaces the complete tree.
- `message` is optional (default `Update definition`).
- `base_version_id` is optional. If it is not the active version, the request fails with `409 conflict`, which prevents lost updates.
- A factory with the `git` source rejects this request. Merge the change to the production branch instead.

From a local directory: `openfactory factory apply ./my-factory --factory <factory>`.

Other definition endpoints:

| Endpoint | Purpose |
|---|---|
| `GET /api/v1/factories/{uid}/definition` | The active files and version. |
| `POST /api/v1/factories/{uid}/definition/validate` | Compile `{"files": {...}}` and run the preflight, without applying it. The response holds `valid`, `errors`, `warnings`, `skipped` (checks that were not possible, with the reason) and a resource summary. |
| `GET /api/v1/factories/{uid}/definition/versions` | All versions, valid and invalid. Each version has a `state`: `active`, `superseded`, `invalid`, or `proposed`, `approved` and `rejected` for a [proposed change](measure-and-improve.md#review). |
| `GET /api/v1/factories/{uid}/definition/versions/{versionId}` | One version with its files and errors. |
| `POST /api/v1/factories/{uid}/definition/versions/{versionId}/restore` | Apply the files of an earlier version again (see [Restore a version](#restore-a-version)). |

### Restore a version

A restore applies the files of an earlier version as a new version, with the message `Restore version <id>`.

- It runs the compiler and the preflight like each apply. If the old files are no longer valid (for example, a secret was deleted), it fails and the active version stays.
- It works only for the `managed` source. For `git`, revert the change in the repository.

### Change the source

`PATCH /api/v1/factories/{uid}` with `source` changes where the definition lives.

- **`managed` to `git`:** send `"source": "git"` and `git_repo` (and `git_branch` and `git_path` if necessary). Open Factory first reads the branch and runs the compiler and the preflight. If the branch has errors or cannot be read, the request fails and the source stays `managed`. The next sync applies the branch; the managed files stay in the version history.
- **`git` to `managed`:** send `"source": "managed"`. The active version stays active and becomes editable in the API or the dashboard.
- The same branch check runs when you create a factory with `"source": "git"` (no factory is created on failure), and when you change `git_repo`, `git_branch` or `git_path`.

Git sync marks a commit as done only after a successful apply or a validation error. After another failure (for example, the database is not available), the next sync retries the same commit.

### Pull request check for git definitions

For a `git` factory in a GitHub repository, Open Factory publishes the check `openfactory/definition` (or `openfactory/definition (<git_path>)`) on pull requests. If the forge client can publish check runs, it is a check run that summarizes created, changed and deleted resources, warnings and skipped checks. Otherwise it is a commit status with a short description.

- It runs when a pull request is opened, reopened, synchronized or marked ready for review, and when its base branch changes.
- By default it runs only for pull requests to the production branch. [`github.factoryConfigChecks: all`](#github) checks every branch. This value is read from the **active** definition, not from the pull request.
- Without changes to definition files, it passes with `No factory definition changes`. Definition files are `factory.yaml`, `factory.yml` and everything under `agents/`, `automations/`, `machines/`, `benchmarks/`, `scorers/`, `webhooks/` and `skills/` (below `git_path`).
- With changes, it compiles the pull request's tree and runs the preflight. For example, a missing secret fails the check.

You can make it a required status check in branch protection.

### Default definition

A `managed` factory created without files gets a generated tree:

- `factory.yaml` with `schemaVersion`, `name`, the repositories you send and `agentDefaults.model` (default `claude-opus-5-5`).
- `codeForges` when the repositories use a code host other than GitHub, or more than one code host.
- The default agents `foreman`, `triage`, `spec`, `implement` and `review` (as in [`../examples/00-default-agents`](../examples/00-default-agents)). The `agents` field selects a subset; the foreman is always included.

## Directory layout

```text
factory.yaml                                  # required
agents/<name>/agent.md                        # at least one; exactly one foreman
agents/<name>/skills/<skill>/SKILL.md         # skills for one agent
agents/<name>/skills/<skill>/<other files>    # supporting files of that skill
skills/<skill>/SKILL.md                       # skills for every agent
skills/<skill>/<other files>
automations/<name>/automation.md
machines/<name>.yaml
webhooks/<name>.yaml                          # at most 20
scorers/<slug>/scorer.md
benchmarks/<suite>/suite.yaml
benchmarks/<suite>/tasks/<task>.yaml
```

| File kind | Format | Schema document | Section |
|---|---|---|---|
| `factory.yaml` | YAML | `factory` | [factory.yaml](#factoryyaml) |
| `agents/<name>/agent.md` | Markdown, optional frontmatter | `agent` | [Agents](#agentsnameagentmd) |
| `automations/<name>/automation.md` | Markdown, frontmatter required | `automation` | [Automations](#automationsnameautomationmd) |
| `skills/<skill>/SKILL.md`, `agents/<name>/skills/<skill>/SKILL.md` | Markdown, frontmatter required | none | [Skills](#skills) |
| `machines/<name>.yaml` | YAML | `machine` | [Machines](#machinesnameyaml) |
| `webhooks/<name>.yaml` | YAML | `webhook` | [Webhooks](#webhooksnameyaml) |
| `scorers/<slug>/scorer.md` | Markdown, frontmatter required | `scorer` | [Scorers](#scorersslugscorermd) |
| `benchmarks/<suite>/suite.yaml` | YAML | `benchmark_suite` | [Benchmarks](#benchmarks) |
| `benchmarks/<suite>/tasks/<task>.yaml` | YAML | `benchmark_suite_task` | [Benchmarks](#benchmarks) |

### Naming rules

- The resource name is the directory or file name: `<name>`, `<slug>`, `<suite>`, `<task>` and the skill directory `<skill>`. It must match `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$` (1–64 characters, starts with a letter or digit).
- A skill's `name` field has a stricter rule. See [Skills](#skills).
- YAML resource files must use the `.yaml` extension. A `.yml` file in `machines/`, `webhooks/` or `benchmarks/<suite>/tasks/` is an error. The root file must be `factory.yaml`, not `factory.yml`.
- `agents/<name>.md`, `automations/<name>.md` and `scorers/<slug>.md` are errors. Use the directory form.
- An `agents/<name>/skills/` directory without `agents/<name>/agent.md` is an error.
- A `benchmarks/<suite>/tasks/` directory without `suite.yaml` is an error.
- Other files (for example `README.md` or `agents/<name>/notes.md`) are ignored.
- Paths are normalized to forward slashes. A path that escapes the tree, or a path listed twice, is an error.

### General parsing rules

These rules apply to every YAML document and Markdown frontmatter block.

- **Unknown keys are errors.** The only exception is `SKILL.md` frontmatter, which can hold extra keys.
- Duplicate keys are errors. YAML merge keys (`<<`) are not supported.
- String fields must be YAML strings. Quote a value that looks like a number or a boolean (for example `version: "26"`); the error message says so.
- A "non-empty string" must contain a character other than whitespace.
- Lists of names (for example `secrets`, `codeForges`, `spawnableBy`) reject duplicate entries.
- An empty YAML document is an empty object.
- Markdown frontmatter starts on line 1 with a `---` line and ends with the next `---` line. The rest of the file is the body. A UTF-8 byte order mark and CRLF line endings are accepted.
- `null` and an absent key mean the same thing, except where this page states that `null` clears an inherited value.

### Validation errors

Each error has a `file`, an optional `line`, an optional `field` path (for example `repositories[1].owner`) and a `message`. All errors of a tree are reported together, sorted by file and line.

### Field tables

The field tables on this page are generated from the [JSON Schemas](#json-schemas). To change a table, change the schema in `internal/definition/schemagen_test.go`, then run `make schemas` and `make docs-gen`. `make docs-check` fails when a table is out of date.

- **Required**: `yes` means the key must be set; for a nested field (for example `gcp.projectNumber`), when its parent is set. Conditional requirements are in the description.
- **Default**: the value when the key is absent. A default that depends on other values is in the description.
- **Allowed values**: what the schema accepts. The description gives the extra compiler rules.
- `[]` in a field name (for example `labels[].value`) means each list entry.

## JSON Schemas

Open Factory publishes JSON Schemas (draft 2020-12) for schema version `v1`. Each document has the `$id` `https://openfactory-org.github.io/schemas/v1/<document>.schema.json`. The documents are `factory`, `agent`, `automation`, `scorer`, `machine`, `webhook`, `benchmark_suite`, `benchmark_suite_task` and `common`.

The schemas check structure only; the compiler does the semantic validation (references between files, inheritance, conflicts). For Markdown files, the schema applies to the frontmatter. Skills have no schema.

For editor completion, add a `$schema` comment on the first line:

```yaml
# yaml-language-server: $schema=https://openfactory-org.github.io/schemas/v1/factory.schema.json
schemaVersion: v1
```

The server serves the schemas without authentication:

| Endpoint | Response |
|---|---|
| `GET /api/v1/factory-files/schemas` | `{"current_version": "v1", "versions": ["v1"]}` |
| `GET /api/v1/factory-files/schemas/v1` | An object with every document, keyed by `<document>.schema.json`. |
| `GET /api/v1/factory-files/schemas/v1/{document}` | One document (`application/schema+json`). The `.schema.json` suffix is optional. |

From the CLI, `openfactory schema` lists the schema documents or prints one. `openfactory validate <dir>` compiles a local tree: it reads every regular file below `<dir>`, skips hidden files and directories and `node_modules`, and rejects files over 4 MiB.

### Schema versions

`v1` is the current schema version. Release `v1.0.0` freezes it: later releases accept `v1` trees unchanged. Until then, a `v0.x` release can change `v1`, and its release notes list each change. See [Upgrading](self-hosting/upgrading.md#versions).

## factory.yaml

`factory.yaml` declares the factory, its repositories, factory-wide grants and the defaults that every agent inherits.

```yaml
# yaml-language-server: $schema=https://openfactory-org.github.io/schemas/v1/factory.schema.json
schemaVersion: v1
name: sdlc
description: Turns labeled issues into reviewed pull requests.
alias: sdlc
repositories:
  - owner: acme
    name: web-app
secrets:
  - NPM_TOKEN
networkPolicy:
  extends: builtin/package-registries@^1
  allowedHosts:
    - api.acme.dev
agentDefaults:
  model: claude-opus-5-5
  machine: linux-build
```

<!-- BEGIN GENERATED: factory (make docs-gen) -->
| Field | Type | Required | Default | Allowed values | Description |
|---|---|---|---|---|---|
| `schemaVersion` | string | yes |  | `v1` | Schema version of the definition. |
| `name` | string | yes |  | non-empty | Name of the factory. |
| `description` | string |  |  |  | Short description of the factory. |
| `alias` | string |  |  | matches `^[A-Za-z0-9 ._-]{1,60}$` | Handle of the foreman in chat and on code hosts. It must not be blank and must be unique in the team. Default: the current alias of the factory, else a value made from name. |
| `credentialStrategy` | string |  |  | `EXECUTOR`, `CREATOR` | Whose code-forge credentials the runs use. Agents inherit it. Without a value at any level, the strategy is EXECUTOR. |
| `maxConcurrentRuns` | integer |  |  | 1 or more | Most open sessions that the factory can have at one time. A queued run waits while the factory is at its limit. Without a value, there is no limit. |
| `approvers` | list of strings |  |  | at least 1 entry; no duplicates; each entry: non-empty | Emails of users or names of service accounts. If set, only they can resolve an approval, from every tool. Other members can still answer questions. Without a value, each team member can approve. |
| `restrictFollowupsToStarter` | boolean |  | `false` |  | If true, only the user who started a work item can send it follow-up messages. Other members can still resolve its inbox items. |
| `requireLinkedActor` | boolean |  | `false` |  | If true, events from a forge, chat or tracker account that is not linked to an active team member are dropped. Custom webhooks, schedules, the API and MCP are not affected. |
| `allowAgentMerge` | boolean |  | `false` |  | If true, agents can call the merge tools of MCP servers through the control plane, and the audit log records each call. If false, the control plane refuses each merge call. |
| `spendLimit` | object |  |  |  | Model spend limit of the factory. Set daily, monthly or both. |
| `spendLimit.daily` | number |  |  | more than 0 | Most model spend in one day, in US dollars. |
| `spendLimit.monthly` | number |  |  | more than 0 | Most model spend in one month, in US dollars. |
| `codeForges` | list of strings |  |  | no duplicates; each entry: `GITHUB`, `GITLAB`, `AZURE_DEVOPS` | Code hosts of the repositories. If it is [], repositories must be empty. |
| `repositories` | list of objects | yes |  |  | Repositories that the factory works on. It can be []. A repository can be listed only once (case-insensitive). |
| `benchmarkRepoSubstitutions` | list of objects |  |  |  | Repositories that benchmark trials use in place of production repositories. |
| `secrets` | list of strings |  |  | [secret names](#secret-names), no duplicates | Factory-wide secret names. Every agent gets them, and no level can remove them. |
| `networkPolicy` | object or null |  |  | a [networkPolicy object](#networkpolicy-object) | Outbound network policy of every run. An agent policy merges with it. Without a policy at any level, outbound access is unrestricted. |
| `mcpServers` | map of objects |  |  | [MCP server objects](#mcp-server-object) by name; the name `factory` is reserved | Factory-wide MCP servers. Every agent gets them. A lower level must not declare the same name again. |
| `cloudProviders` | object |  |  |  | Cloud identity federation. Every run gets short-lived credentials for the declared clouds. |
| `integrations` | list of objects or null |  |  |  | Chat and tracker integrations of the factory. If absent, the factory keeps its integrations. null or [] removes them all. When it is set, each slack, teams, linear or jira trigger needs its integration in the list. |
| `automations` | object |  |  |  | Factory settings for automations. The automations themselves are files automations/&lt;name&gt;/automation.md. |
| `github` | object |  |  |  | GitHub settings of the factory. |
| `scorerDefaults` | object |  |  |  | Execution settings of the judge runs of every scorer and of the built-in Correctness Scorer of benchmarks. Without it, the judge uses the harness of agentDefaults. |
| `selfImprovement` | object |  |  |  | Settings of the Self-improvement runs, which examine failed runs and propose changes. |
| `agentDefaults` | object | yes |  |  | Execution settings that every agent inherits. idleTimeoutMinutes is not allowed here: set it per agent or per automation. |
<!-- END GENERATED: factory -->

More about the fields:

- `schemaVersion`: [Schema versions](#schema-versions).
- `credentialStrategy`: [credentialStrategy](#credentialstrategy).
- `approvers`: [Approvals and follow-ups](security.md#approvals-and-follow-ups).
- `restrictFollowupsToStarter`: other members get `403` from the API and the dashboard.
- `requireLinkedActor`: [How an event reaches an automation](automations.md#how-an-event-reaches-an-automation).
- `allowAgentMerge`: [Agents never merge](security.md#agents-never-merge).
- `spendLimit`: [Spend limits](security.md#spend-limits).
- `repositories`, `benchmarkRepoSubstitutions`, `cloudProviders`, `integrations`, `automations`, `github`, `scorerDefaults`, `selfImprovement` and `agentDefaults`: the sections below.
- `secrets` and `mcpServers`: [Secrets and MCP servers across levels](#secrets-and-mcp-servers-across-levels) and [MCP server object](#mcp-server-object).
- `networkPolicy`: [networkPolicy object](#networkpolicy-object).

### repositories

The repositories that the factory works on. Every run checks them out, unless the agent uses an [environment](machines-and-environments.md#environments).

<!-- BEGIN GENERATED: factory.repositories (make docs-gen) -->
| Field | Type | Required | Default | Allowed values | Description |
|---|---|---|---|---|---|
| `codeForge` | string |  |  | `GITHUB`, `GITLAB`, `AZURE_DEVOPS` | Code host of the repository. Required when codeForges lists more than one value. If codeForges is set, the value must be in it. Default: the only codeForges value, else GITHUB. |
| `owner` | string | yes |  | non-empty | Owner of the repository. No spaces or tabs, no leading or trailing /, no //. For GITHUB, no /. For GITLAB, it can be a group path (acme/platform). For AZURE_DEVOPS, it is organization/project. |
| `name` | string | yes |  | non-empty | Name of one repository: no /, space or tab. |
<!-- END GENERATED: factory.repositories -->

- A repository (code host, owner and name, case-insensitive) can be listed only once.
- If `codeForges` is an empty list, `repositories` must be empty.

### benchmarkRepoSubstitutions

Benchmark trials can run against a copy of a repository instead of the production repository.

```yaml
benchmarkRepoSubstitutions:
  - source: {codeForge: GITHUB, owner: acme, name: web-app}
    target: {codeForge: GITHUB, owner: acme-bench, name: web-app}
```

<!-- BEGIN GENERATED: factory.benchmarkRepoSubstitutions (make docs-gen) -->
| Field | Type | Required | Default | Allowed values | Description |
|---|---|---|---|---|---|
| `source` | object | yes |  |  | Repository to replace. It must be one of the repositories of the factory. |
| `source.codeForge` | string | yes |  | `GITHUB` | Code host of the repository. Only GITHUB. |
| `source.owner` | string | yes |  | non-empty | Owner of the repository. |
| `source.name` | string | yes |  | non-empty | Name of the repository. |
| `target` | object | yes |  |  | Repository that the trials use. It must not be one of the repositories of the factory. |
| `target.codeForge` | string | yes |  | `GITHUB` | Code host of the repository. Only GITHUB. |
| `target.owner` | string | yes |  | non-empty | Owner of the repository. |
| `target.name` | string | yes |  | non-empty | Name of the repository. |
<!-- END GENERATED: factory.benchmarkRepoSubstitutions -->

### cloudProviders

Cloud identity federation.

<!-- BEGIN GENERATED: factory.cloudProviders (make docs-gen) -->
| Field | Type | Required | Default | Allowed values | Description |
|---|---|---|---|---|---|
| `gcp` | object |  |  |  | Google Cloud workload identity federation. |
| `gcp.projectNumber` | string | yes |  | matches `^[0-9]+$` | Number of the Google Cloud project. |
| `gcp.workloadIdentityFederationPoolId` | string | yes |  | non-empty | ID of the workload identity pool. |
| `gcp.workloadIdentityFederationProviderId` | string | yes |  | non-empty | ID of the provider in the workload identity pool. |
| `gcp.serviceAccountEmail` | string |  |  |  | Service account that the credentials impersonate. Must be an email address. |
| `aws` | object |  |  |  | AWS web identity federation. |
| `aws.roleArn` | string | yes |  | matches `^arn:` | ARN of the IAM role that the runs assume. It must start with arn: and contain at least 5 : characters. |
| `audiences` | list of strings |  |  | no duplicates; each entry: non-empty | More audiences for which a run can get a token, for other services that accept OIDC tokens. |
<!-- END GENERATED: factory.cloudProviders -->

Every run gets short-lived credentials for the declared clouds. The control plane is an OpenID Connect issuer:

- The issuer URL is `OF_OIDC_ISSUER_URL`, else `OF_PUBLIC_URL`. The discovery document (`<issuer URL>/.well-known/openid-configuration`) and the signing keys (`<issuer URL>/.well-known/jwks.json`) need no authentication and must be reachable over HTTPS from the cloud provider.
- Tokens are ES256 JWTs, valid for at most 1 hour. The signing key derives from `OF_SECRET_KEY`, so all replicas publish the same key, and rotating `OF_SECRET_KEY` rotates it.
- The `sub` claim is `team/<team id>/factory/<factory id>/agent/<agent name>`. Other claims: `team_id`, `factory_id`, `agent`, `agent_type`, `run_id`, `work_item_id`, `run_as`, `creator` (the user who created the work item, if any) and `worker_host`.
- Tokens refresh while the run runs, so a turn over 1 hour keeps access. The GCP credentials file calls the runner's token helper on each SDK request; the runner rewrites the AWS token file every 20 minutes. Both files are in the runner's state directory, outside the workspace.
- For another service, the agent runs `$OF_CLOUD_TOKEN_COMMAND --audience <audience>`. The audience must be in `audiences`, or be the factory's AWS or Google Cloud audience.

| Cloud | Audience | Run environment |
|---|---|---|
| AWS | `sts.amazonaws.com` | `AWS_ROLE_ARN` (from `aws.roleArn`), `AWS_WEB_IDENTITY_TOKEN_FILE` and `AWS_ROLE_SESSION_NAME` (`openfactory-<run id>`). AWS SDKs and the AWS CLI call `sts:AssumeRoleWithWebIdentity` with them. |
| Google Cloud | `//iam.googleapis.com/projects/<projectNumber>/locations/global/workloadIdentityPools/<poolId>/providers/<providerId>` | `GOOGLE_APPLICATION_CREDENTIALS` and `CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE` point to an `external_account` credentials file. With `serviceAccountEmail`, the credentials impersonate that service account. `GOOGLE_CLOUD_PROJECT_NUMBER` is also set. |

To trust the issuer:

- **AWS:** create an IAM OIDC identity provider with the issuer URL and the audience `sts.amazonaws.com`. In the role trust policy, allow `sts:AssumeRoleWithWebIdentity` and match `<issuer host>:sub` and `<issuer host>:aud`.
- **Google Cloud:** create an OIDC workload identity pool provider with the issuer URL and map `google.subject` to `assertion.sub`.

The credentials go to the harness environment, not to setup commands. In a restricted network policy, the token exchange hosts are [required hosts](self-hosting/network-policy.md#required-hosts).

### integrations

The chat and issue-tracker integrations that the factory uses. The team-level connection is set up separately: see [Integrations](integrations/README.md).

```yaml
integrations:
  - type: slack
    slack:
      autoRespondToThreadReplies: true
  - type: linear
```

<!-- BEGIN GENERATED: factory.integrations (make docs-gen) -->
| Field | Type | Required | Default | Allowed values | Description |
|---|---|---|---|---|---|
| `type` | string | yes |  | `slack`, `microsoft-teams`, `linear`, `jira` | Integration type. Each type can occur only once. linear and jira are mutually exclusive. |
| `slack` | object |  |  |  | Settings of the slack entry. Valid only when type is slack. |
| `slack.autoRespondToThreadReplies` | boolean |  | `true` |  | If true, a reply in the thread of a work item continues the work item. If false, a reply without a mention does not. |
| `microsoft-teams` | object |  |  |  | Settings of the microsoft-teams entry. Valid only when type is microsoft-teams. |
| `microsoft-teams.autoRespondToThreadReplies` | boolean |  | `true` |  | If true, a reply in the thread of a work item continues the work item. If false, a reply without a mention does not. |
| `linear` | object |  |  |  | Settings of the linear entry. Valid only when type is linear. |
| `linear.teamIds` | list of strings |  |  |  | Accepted for compatibility and ignored. |
| `jira` | object |  |  |  | Settings of the jira entry. Valid only when type is jira. |
| `jira.projectKeys` | list of strings |  |  |  | Accepted and ignored. |
<!-- END GENERATED: factory.integrations -->

- A settings block is valid only on the entry of its own type.
- `linear` and `jira` are mutually exclusive.
- Absent: the factory keeps its previous integrations. `null` or `[]`: the factory has no integrations.
- When present, every trigger with the provider `slack`, `teams`, `linear` or `jira` requires that integration in the list.

### automations (factory settings)

<!-- BEGIN GENERATED: factory.automations (make docs-gen) -->
| Field | Type | Required | Default | Allowed values | Description |
|---|---|---|---|---|---|
| `automations.githubOnboardingNotices` | boolean |  | `true` |  | If true, Open Factory comments once on the issue or pull request when a GitHub account that cannot start work sends an event. If false, it ignores the event without a comment. |
<!-- END GENERATED: factory.automations -->

Factory settings only; automations are files in `automations/<name>/automation.md`.

New work from a GitHub event (an automation match or a mention) starts only for a GitHub account linked to a user of the factory's team. Bot accounts and events without an actor (for example CI events) are not checked, and comments on existing work items still continue them.

With `githubOnboardingNotices: true` on any factory that the event concerns, Open Factory comments once per account and team on the issue or pull request: an unlinked account gets the account-linking link, and a linked user outside the team is asked to get access. With `false`, the event is ignored silently.

### github

<!-- BEGIN GENERATED: factory.github (make docs-gen) -->
| Field | Type | Required | Default | Allowed values | Description |
|---|---|---|---|---|---|
| `github.factoryConfigChecks` | string |  | `main` | `main`, `all` | Pull requests that get the openfactory/definition check: main (pull requests to the production branch) or all (pull requests to every branch). Open Factory reads it from the active definition. |
<!-- END GENERATED: factory.github -->

The check is the [`openfactory/definition` check](#pull-request-check-for-git-definitions).

### credentialStrategy

Selects whose code-forge credentials a run uses for checkout, pushes and other code-host access. An agent's value overrides the factory's. The default is `EXECUTOR`.

| Value | Credentials |
|---|---|
| `EXECUTOR` | The factory's team integration for the forge (GitHub App installation or token, GitLab token, Azure DevOps token). Commits and pull requests are authored by that identity. |
| `CREATOR` | The personal token of the `run_as` principal of the run, when that principal is a person. See [Run principals](#run-principals). |

`CREATOR` uses the factory's team integration in these cases:

- The `run_as` principal is the factory or a service account (for example a schedule, a custom webhook, or an automation without `runAs`).
- The forge does not support per-user tokens. GitHub, GitLab and Azure DevOps support them. A generic `git` integration does not.
- The team integration sets `config.user_tokens` to `"false"`.

If the `run_as` person has no personal token for the forge, there is no fallback. An API, dashboard or Factory MCP request gets `401` and creates no run. A run started by an automation or a mention fails with `external_authentication_required` before it gets a sandbox.

Users store tokens with `PUT /api/v1/me/forge-tokens/{forge}` (`GITHUB`, `GITLAB` or `AZURE_DEVOPS`); see [Personal forge tokens](api.md#personal-forge-tokens). A personal token uses the API base URL of the team's integration (for example a GitHub Enterprise Server `base_url`). Tokens are encrypted with `OF_SECRET_KEY`. They are not team secrets, so `secrets` cannot request them.

### Run principals

Each run records two principals, shown in the API and the dashboard:

- `creator`: who caused the run. Empty for a schedule, a webhook, a scorer and a benchmark.
- `run_as`: who the run acts as. Never empty.

A principal is a user, a [service account](api.md#service-accounts) (a non-person principal for a CI job or a bot, managed by admins) or the factory.

| How the run started | `creator` | `run_as` | Author of the pull request with `CREATOR` | Author with `EXECUTOR` |
|---|---|---|---|---|
| A person, through the API, the dashboard or the Factory MCP server | that user | that user | that user | the factory's integration |
| A service account key | that service account | that service account | the factory's integration | the factory's integration |
| A mention from a linked account | that user | that user | that user | the factory's integration |
| An automation (event, schedule, webhook) | the linked user of the actor, or empty | the `runAs` of the automation (default: the factory) | the `runAs` user, else the factory's integration | the factory's integration |
| A child run (`run_agents`) | the creator of the parent | the `run_as` of the parent | as the parent | the factory's integration |
| A scorer, a benchmark, self-improvement | empty | the factory | the factory's integration | the factory's integration |

The pull request author is the identity of the credential that opened it. An automation sets its runs' principal with [`runAs`](#automationsnameautomationmd).

### scorerDefaults

Execution settings of the judge runs of every scorer and of the built-in Correctness Scorer of benchmarks. See [The judge run](measure-and-improve.md#the-judge-run).

<!-- BEGIN GENERATED: factory.scorerDefaults (make docs-gen) -->
| Field | Type | Required | Default | Allowed values | Description |
|---|---|---|---|---|---|
| `harness` | object or null |  |  | a sparse [harness object](#harness-and-model) | The harness of the judge runs, over the harness of agentDefaults. |
| `model` | string |  |  | non-empty | The model of the judge runs. It replaces the model of the harness. |
| `machine` | string |  |  | a [resource name](#naming-rules) | Machine of the judge runs: the name of a file machines/&lt;name&gt;.yaml. A scorer's machine replaces it. |
| `secrets` | list of strings |  |  | [secret names](#secret-names), no duplicates | Secret names of the judge runs. A scorer's secrets add to them. |
| `mcpServers` | map of objects |  |  | [MCP server objects](#mcp-server-object) by name; the name `factory` is reserved | MCP servers of the judge runs. A scorer's servers add to them. A scorer server with the same name replaces the server. |
<!-- END GENERATED: factory.scorerDefaults -->

Without `scorerDefaults`, the judge uses the harness of `agentDefaults`.

### selfImprovement

<!-- BEGIN GENERATED: factory.selfImprovement (make docs-gen) -->
| Field | Type | Required | Default | Allowed values | Description |
|---|---|---|---|---|---|
| `failedRunThreshold` | integer |  | `25` | 1–50 | Number of distinct unreviewed failures of one agent that starts a Self-improvement run. |
| `reviewerType` | string |  | `admins` | `admins`, `team`, `custom`, `none` | Who reviews a proposed change: admins (team admins), team (any team member), custom (the members in reviewerEmails) or none. |
| `reviewerEmails` | list of strings |  |  | at least 1 entry; no duplicates | Emails of the reviewers. Required when reviewerType is custom, and allowed only then. Each entry must be an email address. |
| `analysisModel` | string |  |  | non-empty | The model that examines failed runs. The default is the model of the foreman. |
<!-- END GENERATED: factory.selfImprovement -->

See [Measure and improve](measure-and-improve.md).

### agentDefaults

**Required.** The execution settings that every agent inherits.

<!-- BEGIN GENERATED: factory.agentDefaults (make docs-gen) -->
| Field | Type | Required | Default | Allowed values | Description |
|---|---|---|---|---|---|
| `model` | string |  |  | non-empty | Model shorthand for every agent. Set exactly one of model or harness. |
| `harness` | object |  |  | a complete [harness object](#harness-and-model) | Complete harness object for every agent: type and model are required. Set exactly one of model or harness. |
| `machine` | string |  |  | a [resource name](#naming-rules) | Default machine: the name of a file machines/&lt;name&gt;.yaml. It must be a Linux machine. |
| `environmentId` | string |  |  |  | ID of the default environment. |
| `secrets` | list of strings |  |  | [secret names](#secret-names), no duplicates | Default secret names for agents without their own secrets. |
| `mcpServers` | map of objects |  |  | [MCP server objects](#mcp-server-object) by name; the name `factory` is reserved | Default MCP servers for agents without their own mcpServers. A name must not also be in the factory-wide mcpServers. |
| `workerHost` | string or null |  |  |  | Default worker host. An empty value means the server default (server). |
<!-- END GENERATED: factory.agentDefaults -->

See [Harness and model](#harness-and-model), [environments](machines-and-environments.md#environments) and [worker hosts](machines-and-environments.md#worker-hosts). `idleTimeoutMinutes` is not accepted here; set it per agent or automation.

## Execution settings

Execution settings decide how and where an agent runs. The same keys appear at four levels; a lower level overrides a higher one, and an unset value inherits.

```text
agentDefaults (factory.yaml) → agent (agent.md) → automation (automation.md) → run_agents (per child run)
```

| Key | Type | Rules | agentDefaults | agent | automation | `run_agents` |
|---|---|---|---|---|---|---|
| `model` | string | Non-empty. | yes | yes | yes | `model` |
| `harness` | object | See [Harness and model](#harness-and-model). | yes (complete) | yes (sparse) | yes (sparse) | no |
| `machine` | string | Must name `machines/<name>.yaml`. | yes (Linux only) | yes | yes | `machine` |
| `environmentId` | string | `null` clears the inherited value. | yes | yes | yes | `environment_id` |
| `secrets` | list | [Secret names](#secret-names). | yes | yes | yes | no |
| `mcpServers` | map | [MCP servers](#mcp-server-object). | yes | yes | yes | no |
| `workerHost` | string | Trimmed. `null` clears the inherited value. | yes | yes | yes | `worker_host` |
| `idleTimeoutMinutes` | integer | 0–60. 0 ends the session after each turn. | no | yes | yes | no |

`model` and `harness` are mutually exclusive at every level. For the `run_agents` arguments, see [Agents](agents.md#start-child-runs-with-run_agents).

### Harness and model

See [Harnesses](harnesses.md) for the runtimes.

**The harness object:**

<!-- BEGIN GENERATED: harness (make docs-gen) -->
| Field | Type | Required | Default | Allowed values | Description |
|---|---|---|---|---|---|
| `type` | string | yes |  | `claude`, `codex`, `claude-code` | Harness that runs the agent. claude-code is an alias of claude. |
| `model` | string | yes |  | non-empty | Model name. The harness gets it as it is. |
| `reasoningLevel` | string |  |  | matches `^[a-z][a-z0-9_-]{0,31}$` | Reasoning level that the harness passes to the model. |
| `auth` | object or null |  |  | an [auth object](#harness-and-model) | Model credential of the harness. null clears the inherited credential. |
<!-- END GENERATED: harness -->

**The auth object:**

<!-- BEGIN GENERATED: harnessAuth (make docs-gen) -->
| Field | Type | Required | Default | Allowed values | Description |
|---|---|---|---|---|---|
| `source` | string | yes |  | `managedSecret`, `workerEnvironment` | Where the credential comes from. managedSecret reads a secret. workerEnvironment uses the environment of a self-hosted worker, so the effective workerHost must not be empty. |
| `secretName` | string |  |  | a [secret name](#secret-names) | Secret that holds the credential. Required for managedSecret. Not allowed for workerEnvironment. |
<!-- END GENERATED: harnessAuth -->

`workerEnvironment` requires a non-empty effective `workerHost` (a self-hosted worker) at every level, so an agent or automation that inherits `workerEnvironment` auth cannot clear `workerHost`.

**The `model` shorthand.** `model: X` selects the harness from the model name:

- A Claude model name (it starts with `claude-`, or it is `opus`, `sonnet` or `haiku`) selects `claude`.
- An OpenAI model name (it starts with `gpt-`, `chatgpt-` or `codex-`, or it is an `o` model such as `o3` or `o4-mini`) selects `codex`.
- Any other name is an error. Set `harness` with a `type` and a `model` instead.

If the selected harness is the inherited one, only the model changes and the inherited `reasoningLevel` and `auth` stay. Otherwise they do not carry over.

**The credential.** A factory needs a model credential (`auth`) before its first run; the harness CLI gets it in its environment. The preflight warns for each agent without `auth`. A generated factory uses the `claude` harness with `claude-opus-5-5`.

**Sparse harness overrides.** At the agent and automation levels, `harness` changes only the fields that you set:

- If `type` is absent or equal to the inherited type, the other fields merge field by field.
- If `type` changes, `model` is **required**, and the inherited `reasoningLevel` and `auth` are dropped.

Example: `agentDefaults` uses Claude Code with a managed key, `implement` switches to Codex, and `review` changes only the model.

```yaml
# factory.yaml
agentDefaults:
  harness:
    type: claude
    model: claude-opus-5-5
    reasoningLevel: high
    auth:
      source: managedSecret
      secretName: ANTHROPIC_API_KEY
```

```markdown
---
# agents/implement/agent.md
agentType: IMPLEMENT
harness:
  type: codex
  model: gpt-5.3-codex
  auth:
    source: managedSecret
    secretName: OPENAI_API_KEY
---
Implement the approved change.
```

```markdown
---
# agents/review/agent.md
agentType: REVIEW
harness:
  model: claude-sonnet-5-5
---
Review the pull request.
```

The full example is [`../examples/03-multi-harness`](../examples/03-multi-harness).

### Secrets and MCP servers across levels

**Secrets:**

- Factory-wide `secrets` (in `factory.yaml`) are added to every agent. No level can remove them.
- `agentDefaults.secrets` is the default list.
- An agent's `secrets` replaces the default list. `secrets: []` removes the defaults but keeps the factory-wide secrets.
- `secrets: null` is the same as an absent key: the agent inherits the default list.
- The effective list has no duplicates.

**MCP servers:**

- Factory-wide `mcpServers` are added to every agent.
- `agentDefaults.mcpServers` is the default map.
- An agent's `mcpServers` replaces the default map. `mcpServers: {}` removes the defaults but keeps the factory-wide servers.
- A server name that is in the factory-wide `mcpServers` must not be declared again in `agentDefaults`, an agent or an automation. This is an error: `MCP server "<name>" is already granted factory-wide by factory.yaml mcpServers`.
- The name `factory` is reserved in every `mcpServers` map. Open Factory injects the orchestration server under that name into every run.

**Automations** add to the agent's lists; see [Automation overrides at run time](#automation-overrides-at-run-time).

### Clearing inherited values

| Key | How to clear at the agent or automation level |
|---|---|
| `environmentId` | `environmentId: null` |
| `workerHost` | `workerHost: null` (the run uses the default, `server`) |
| `harness.auth` | `harness: {auth: null}` |
| `secrets` | At the agent level, `secrets: []` (factory-wide secrets stay). An automation cannot clear them, because its list adds to the agent's list. |
| `mcpServers` | At the agent level, `mcpServers: {}` (factory-wide servers stay). An automation cannot clear them, because its map adds to the agent's map. |

`model: null`, `harness: null`, `machine: null` and `networkPolicy: null` are the same as an absent key.

### Automation overrides at run time

When an automation starts a run, its execution settings apply on top of the target agent's effective configuration:

- `machine`, `environmentId`, `workerHost` and `idleTimeoutMinutes` replace the agent's values.
- `secrets` and `mcpServers` add to the agent's values (union without duplicates, agent entries first). An automation MCP server replaces an agent server with the same name. Factory-wide `secrets` and `mcpServers` are always added. An automation cannot remove an agent's secret or server; change the agent instead.
- A `harness` or `model` override replaces the agent's harness as a whole. The compiler resolves the sparse `harness` against the agent (see [Sparse harness overrides](#harness-and-model)). A `reasoningLevel` or `auth` that the automation clears with `null`, or drops by changing the type, stays cleared.

## `agents/<name>/agent.md`

Markdown: the optional frontmatter holds configuration, and the body is the instructions.

```markdown
---
description: Investigates incidents with Sentry, Datadog and the code
agentType: TRIAGE
secrets:
  - DATADOG_API_KEY
mcpServers:
  datadog:
    url: https://mcp.datadoghq.com/api/unstable/mcp-server/mcp
    headers:
      DD-API-KEY: "{{DATADOG_API_KEY}}"
spawnableBy: [foreman]
idleTimeoutMinutes: 30
---

Investigate the incident that the foreman describes...
```

<!-- BEGIN GENERATED: agent (make docs-gen) -->
| Field | Type | Required | Default | Allowed values | Description |
|---|---|---|---|---|---|
| `description` | string |  |  |  | Short description that list_agents shows to the foreman. |
| `agentType` | string |  | `CUSTOM` | `CUSTOM`, `FOREMAN`, `MAIN`, `TRIAGE`, `SPEC`, `IMPLEMENT`, `REVIEW`, `VERIFY` | Role of the agent. MAIN is an alias of FOREMAN. Exactly one agent must be the foreman. |
| `credentialStrategy` | string |  |  | `EXECUTOR`, `CREATOR` | Whose code-forge credentials the runs use. It overrides the factory value. |
| `spawnableBy` | list of strings or null |  |  | no duplicates; each entry: a [resource name](#naming-rules) | Agents that can start this agent with run_agents. Without a value, only the foreman can. [] means no agent. Each name must be an agent of the definition. |
| `networkPolicy` | object or null |  |  | a [networkPolicy object](#networkpolicy-object) | Network policy of the agent. It merges with the policy of the factory. |
| `snapshot` | boolean or null |  | `true` |  | If false, a snapshot stores only the harness state, not the workspace. |
| `model` | string |  |  | non-empty | Model shorthand. A Claude model selects the claude harness. An OpenAI model selects the codex harness. Do not set it together with harness. |
| `harness` | object or null |  |  | a sparse [harness object](#harness-and-model) | Sparse override of the inherited harness. It changes only the fields that you set. Set auth to null to clear the inherited credential. |
| `machine` | string or null |  |  | a [resource name](#naming-rules) | Machine of the runs: the name of a file machines/&lt;name&gt;.yaml. It replaces the inherited machine. |
| `environmentId` | string or null |  |  |  | ID of the environment of the runs. null clears the inherited value. |
| `secrets` | list of strings or null |  |  | [secret names](#secret-names), no duplicates | Secret names of the runs. They replace the secrets of agentDefaults. [] removes those defaults. The factory-wide secrets always stay. |
| `mcpServers` | map of objects or null |  |  | [MCP server objects](#mcp-server-object) by name; the name `factory` is reserved | MCP servers of the runs. They replace the mcpServers of agentDefaults. {} removes those defaults. The factory-wide servers always stay. |
| `workerHost` | string or null |  |  |  | Worker that runs the agent. The value is trimmed. null or empty clears an inherited host. |
| `idleTimeoutMinutes` | integer or null |  |  | 0–60 | Minutes that a finished run keeps its sandbox and session for follow-ups. 0 ends the session after each turn. Without a value, the server default OF_IDLE_TIMEOUT_DEFAULT applies (10 minutes) to every run. OF_IDLE_TIMEOUT_MAX (60 minutes) is the upper limit. |
<!-- END GENERATED: agent -->

For `model` to `idleTimeoutMinutes`, see [Execution settings](#execution-settings).

Rules:

- A definition must have at least one agent.
- Exactly one agent must have `agentType: FOREMAN` (or `MAIN`).
- A file without frontmatter is a `CUSTOM` agent. Its whole content is the instructions.
- The body is trimmed. It can be empty.
- `spawnableBy` controls which agents can start this agent with `run_agents`:
  - Not set (or `null`): only the foreman.
  - `[]`: no agent. Automations and the API can still start it.
  - A list: exactly the named agents. Each name must be an agent in the definition.
  - The foreman can never be started as a child run.
- `idleTimeoutMinutes` is how long a finished run keeps its sandbox and session for follow-ups. Unset, the server default `OF_IDLE_TIMEOUT_DEFAULT` (10 minutes) applies to root and child runs alike. `OF_IDLE_TIMEOUT_MAX` (60 minutes) caps it.
- The agent name is its identity. Renaming the directory creates a new agent with a new ID.
- Agent skills live in `agents/<name>/skills/` (see [Skills](#skills)).

See [Agents](agents.md) for roles, default agents and orchestration.

## `automations/<name>/automation.md`

Connects triggers to an agent. The frontmatter is **required**; the body is the starting prompt and must not be empty.

```markdown
---
displayName: Nightly dependency audit
agent: foreman
triggers:
  - provider: schedule
    event: cron_fired
    schedule:
      cron: "@daily"
  - provider: github
    event: issue_labeled
    filter:
      repos: [acme/web-app]
      labels: [factory-ready]
idleTimeoutMinutes: 5
---

Audit dependencies in every repository...
```

<!-- BEGIN GENERATED: automation (make docs-gen) -->
| Field | Type | Required | Default | Allowed values | Description |
|---|---|---|---|---|---|
| `enabled` | boolean |  | `true` |  | If false, the automation never matches an event. |
| `displayName` | string or null |  |  |  | Name that the dashboard shows. |
| `agent` | string |  |  | a [resource name](#naming-rules) | Agent of the runs. It must be an agent of the definition. Default: the foreman. |
| `runAs` | string |  | `factory` | matches `^(factory\|user:[^@\s]+@[^@\s]+\|serviceAccount:[^\s/:]+)$` | The principal of the runs that the automation starts: factory, user:&lt;email&gt; or serviceAccount:&lt;name&gt;. The principal must exist in the team when you apply the definition. |
| `triggers` | list of objects | yes |  | at least 1 entry; each entry: [trigger objects](#automationsnameautomationmd) | Events and schedules that start the automation. |
| `model` | string |  |  | non-empty | Model shorthand. A Claude model selects the claude harness. An OpenAI model selects the codex harness. Do not set it together with harness. |
| `harness` | object or null |  |  | a sparse [harness object](#harness-and-model) | Sparse override of the inherited harness. It changes only the fields that you set. Set auth to null to clear the inherited credential. |
| `machine` | string or null |  |  | a [resource name](#naming-rules) | Machine of the runs: the name of a file machines/&lt;name&gt;.yaml. It replaces the inherited machine. |
| `environmentId` | string or null |  |  |  | ID of the environment of the runs. null clears the inherited value. |
| `secrets` | list of strings or null |  |  | [secret names](#secret-names), no duplicates | Secret names that add to the secrets of the agent for the runs of this automation. |
| `mcpServers` | map of objects or null |  |  | [MCP server objects](#mcp-server-object) by name; the name `factory` is reserved | MCP servers that add to the servers of the agent for the runs of this automation. A server with the same name replaces the server of the agent. |
| `workerHost` | string or null |  |  |  | Worker that runs the agent. The value is trimmed. null or empty clears an inherited host. |
| `idleTimeoutMinutes` | integer or null |  |  | 0–60 | Minutes that a finished run keeps its sandbox and session for follow-ups. 0 ends the session after each turn. Without a value, the server default OF_IDLE_TIMEOUT_DEFAULT applies (10 minutes) to every run. OF_IDLE_TIMEOUT_MAX (60 minutes) is the upper limit. |
<!-- END GENERATED: automation -->

The execution settings (`model` to `idleTimeoutMinutes`) apply over the target agent; see [Automation overrides at run time](#automation-overrides-at-run-time) and [Run principals](#run-principals).

Each trigger has this shape:

<!-- BEGIN GENERATED: automation.trigger (make docs-gen) -->
| Field | Type | Required | Default | Allowed values | Description |
|---|---|---|---|---|---|
| `provider` | string | yes |  | `azure_devops`, `github`, `gitlab`, `jira`, `linear`, `microsoft_teams`, `schedule`, `slack`, `teams`, `webhook` | Source of the events. teams and microsoft_teams are the same provider. |
| `event` | string | yes |  | non-empty | Event of the provider. |
| `filter` | any |  |  |  | Filter of the event. The provider and the event decide the valid keys, and whether a filter is required, optional or not allowed. |
| `schedule` | object |  |  |  | Schedule of the trigger. Required for provider schedule (event cron_fired), and allowed only there. |
| `schedule.cron` | string | yes |  | non-empty | Five-field UTC cron expression or a descriptor such as @daily or @every 1h. No TZ= prefix. |
| `schedule.name` | string |  |  | non-empty | Name of the schedule. Names must be unique in the automation. At most one schedule trigger of an automation can omit its name. |
| `concurrency` | string |  | `allow` | `allow`, `skip` | Schedule triggers only. allow starts a run for every slot. skip drops a slot while a run of the automation is not finished. |
<!-- END GENERATED: automation.trigger -->

Providers, events, filter keys, schedules and `concurrency` are in [Automations](automations.md) (schedules: [Schedules](automations.md#schedules-schedule)).

## Skills

A skill is a directory with a `SKILL.md` file and optional supporting files.

| Location | Scope |
|---|---|
| `skills/<skill>/SKILL.md` | Every agent of the factory. |
| `agents/<agent>/skills/<skill>/SKILL.md` | Only that agent. It replaces a factory skill with the same name for that agent. |

```markdown
---
name: run-checks
description: Runs the full local validation suite and summarizes failures. Use before reporting an implementation as done.
---

# Run checks
...
```

| Field | Type | Rules |
|---|---|---|
| `name` | string | **Required.** At most 64 characters. Must match `^[a-z0-9]+(-[a-z0-9]+)*$` (lowercase letters, digits, single hyphens). Must be equal to the directory name. |
| `description` | string | **Required.** Non-empty. At most 1024 characters. |
| other keys | any | Allowed and ignored (for example `license` or `metadata`). |

- The frontmatter is **required**. A skill directory without `SKILL.md` is an error.
- Other files in the skill directory are supporting files, copied into the sandbox at the same relative path.

See [Skills](skills.md).

## `machines/<name>.yaml`

The compute and image of a sandbox.

```yaml
# yaml-language-server: $schema=https://openfactory-org.github.io/schemas/v1/machine.schema.json
description: Linux machine for web builds and tests
setupCommands:
  - corepack enable
  - pnpm install --frozen-lockfile
instanceShape:
  vcpus: 4
  memoryGb: 8
platform:
  os: linux
  arch: x86_64
  linux:
    image: node:22-bookworm
```

<!-- BEGIN GENERATED: machine (make docs-gen) -->
| Field | Type | Required | Default | Allowed values | Description |
|---|---|---|---|---|---|
| `description` | string |  |  |  | Short description of the machine. |
| `setupCommands` | list of strings |  |  | each entry: non-empty | Commands that run in order after the repositories are checked out. |
| `failureSessionRetentionMinutes` | integer |  |  | 1–60 | Minutes that the sandbox of a failed run is kept. Without a value, the sandbox is released at once. |
| `instanceShape` | object |  |  |  | CPU and memory of the sandbox. For linux and windows, both values must be powers of two. For macos, the shape must be 4/7, 6/14, 8/14, 12/28 or 12/56 (vCPUs/GB). |
| `instanceShape.vcpus` | integer | yes |  | 1 or more | Number of virtual CPUs. |
| `instanceShape.memoryGb` | integer | yes |  | 1 or more | Memory in GB. |
| `platform` | object | yes |  |  | Operating system, architecture and image of the sandbox. |
| `platform.os` | string |  | `linux` | `linux`, `macos`, `windows` | Operating system of the sandbox. |
| `platform.arch` | string |  |  | `x86_64`, `aarch64` | CPU architecture. macos allows only aarch64. windows allows only x86_64. Default: x86_64, or aarch64 for macos. |
| `platform.linux` | object |  |  |  | Linux settings. Required when os is linux, and allowed only then. |
| `platform.linux.image` | string | yes |  | non-empty | Container image of the sandbox. No spaces or tabs. |
| `platform.linux.registryCredentialSecretName` | string |  |  | a [secret name](#secret-names) | Secret with the credential to pull a private image. The kubernetes backend ignores it and pulls with the imagePullSecrets of the pod template. |
| `platform.linux.user` | integer or string |  | `1000` | 1 or more; or `image` | User of the sandbox on the kubernetes backend. A number sets runAsUser, runAsGroup and fsGroup. image keeps the user of the image. Root (0) is not allowed. |
| `platform.mac` | object |  |  |  | macOS settings. Allowed only when os is macos. |
| `platform.mac.version` | string |  | `"26"` | `"14"`, `"15"`, `"26"`, `"27"` | macOS version. Quote the value. |
<!-- END GENERATED: machine -->

For `platform.linux.user`, see [Sandbox user](self-hosting/kubernetes.md#sandbox-user).

- `platform.linux` is valid only when `os` is `linux`. `platform.mac` is valid only when `os` is `macos`.
- For `linux` and `windows`, `vcpus` and `memoryGb` must both be positive powers of two.
- For `macos`, the shape must be one of 4/7, 6/14, 8/14, 12/28 or 12/56 (vCPUs/GB).
- `agentDefaults.machine` must be a Linux machine.

See [Machines and environments](machines-and-environments.md).

## `webhooks/<name>.yaml`

An authenticated HTTP endpoint for inbound events. At most 20 per factory.

```yaml
# yaml-language-server: $schema=https://openfactory-org.github.io/schemas/v1/webhook.schema.json
authMode: signature
signatureScheme: pagerduty
secretName: PAGERDUTY_WEBHOOK_SECRET
deliveryIdHeader: X-Webhook-Id
```

<!-- BEGIN GENERATED: webhook (make docs-gen) -->
| Field | Type | Required | Default | Allowed values | Description |
|---|---|---|---|---|---|
| `authMode` | string |  | `token` | `token`, `url_token`, `signature` | How the sender authenticates: token (the secret in an Authorization: Bearer header), url_token (the secret as the last segment of the URL path) or signature (a provider signature). It cannot change after the first apply. |
| `signatureScheme` | string |  |  | `github`, `pagerduty`, `sentry`, `standard_webhooks`, `stripe`, `vercel` | Signature format of the provider. Required when authMode is signature, and allowed only then. It cannot change after the first apply. |
| `secretName` | string | yes |  | a [secret name](#secret-names) | Secret that authenticates the requests. |
| `deliveryIdHeader` | string |  |  | matches `` ^[!#$%&'*+.^_`\|~0-9A-Za-z-]{1,64}$ `` | Request header that holds the delivery ID. Open Factory uses the ID to find repeated deliveries. It must not be a credential header, for example Authorization, Cookie or a signature header. Stored in canonical form. |
| `enabled` | boolean |  | `true` |  | If false, the webhook never starts an automation. |
<!-- END GENERATED: webhook -->

See [Webhooks](webhooks.md).

## `scorers/<slug>/scorer.md`

Classifies sampled runs with a judge run in a sandbox. The frontmatter is **required**; the body is the rubric and must not be empty.

<!-- BEGIN GENERATED: scorer (make docs-gen) -->
| Field | Type | Required | Default | Allowed values | Description |
|---|---|---|---|---|---|
| `name` | string | yes |  | non-empty | Name of the scorer. Unique among the scorers. |
| `description` | string |  |  |  | Short description of the scorer. |
| `output` | string |  | `classification` | `classification` | Kind of output. |
| `agents` | list | yes |  | at least 1 entry; each entry: a [resource name](#naming-rules); or an object | Agents whose runs the scorer samples. Each entry is an agent name or {name, includeDescendants}. Each name must be an agent of the definition, listed once. |
| `labels` | list of objects | yes |  | 1–20 entries | Labels that the judge can give a run. |
| `labels[].value` | string | yes |  | non-empty | Name of the label. Unique in the scorer. |
| `labels[].description` | string |  |  |  | What the label means. |
| `labels[].score` | number | yes |  | 0–1 | Score of the label. |
| `passingScore` | number | yes |  | 0–1 | Lowest passing score. At least one label must have a score at or above it, and at least one label a score below it. |
| `samplingRate` | number |  | `10` | 0–100 | Percent of finished runs to score, rounded to two decimals. 0 turns off automatic scoring. |
| `model` | string |  |  | non-empty | Model of the judge run. Required unless harness is set. Without harness, the model name selects the harness. With harness, it replaces the model of that harness. |
| `harness` | object or null |  |  | a sparse [harness object](#harness-and-model) | The harness of the judge run, over scorerDefaults and agentDefaults. |
| `selfImprovement` | boolean |  | `false` |  | If true, the failures of this scorer go to Self-improvement. |
| `machine` | string |  |  | a [resource name](#naming-rules) | Machine of the judge run. It replaces scorerDefaults.machine. |
| `secrets` | list of strings |  |  | [secret names](#secret-names), no duplicates | Secret names of the judge run. They add to scorerDefaults.secrets. |
| `mcpServers` | map of objects |  |  | [MCP server objects](#mcp-server-object) by name; the name `factory` is reserved | MCP servers of the judge run. They add to scorerDefaults.mcpServers. A server with the same name replaces the server of scorerDefaults. |
<!-- END GENERATED: scorer -->

See [Measure and improve](measure-and-improve.md).

## Benchmarks

### `benchmarks/<suite>/suite.yaml`

<!-- BEGIN GENERATED: benchmark_suite (make docs-gen) -->
| Field | Type | Required | Default | Allowed values | Description |
|---|---|---|---|---|---|
| `name` | string | yes |  | non-empty | Name of the suite. Unique among the suites. |
| `description` | string |  |  |  | Short description of the suite. |
| `agent` | string | yes |  | a [resource name](#naming-rules) | Agent that the trials start. It must be an agent of the definition. |
| `configurations` | list of objects |  |  | 1–6 entries | Configurations that the suite compares. |
| `configurations[].name` | string | yes |  | non-empty | Name of the configuration. Unique in the suite. |
| `configurations[].agents` | list of objects |  |  |  | Agent pins of the configuration. |
| `configurations[].agents[].agent` | string | yes |  | a [resource name](#naming-rules) | Agent to pin. It must be an agent of the definition, once per configuration. |
| `configurations[].agents[].model` | string |  |  | non-empty | Model of the pinned agent. Set exactly one of model or harness. |
| `configurations[].agents[].harness` | object |  |  | a complete [harness object](#harness-and-model) | Complete harness of the pinned agent: type and model are required. Set exactly one of model or harness. |
| `configurations[].machine` | string |  |  | a [resource name](#naming-rules) | Machine of every agent in the trials of this configuration: the name of a file machines/&lt;name&gt;.yaml. |
| `tasks` | list of strings |  |  | no duplicates; each entry: a [resource name](#naming-rules) | Task names. Each name needs a file benchmarks/&lt;suite&gt;/tasks/&lt;name&gt;.yaml. Each task file must be in this list. |
<!-- END GENERATED: benchmark_suite -->

Every task file must be listed in `tasks`. An unlisted task file is an error.

### `benchmarks/<suite>/tasks/<task>.yaml`

<!-- BEGIN GENERATED: benchmark_suite_task (make docs-gen) -->
| Field | Type | Required | Default | Allowed values | Description |
|---|---|---|---|---|---|
| `title` | string | yes |  | non-empty | Title of the task. |
| `prompt` | string | yes |  | non-empty | Prompt of the trial. |
| `successCriteria` | string | yes |  | non-empty | What the trial must do to pass. |
| `sourceRunId` | string |  |  | non-empty | Run that the task reproduces. |
| `tags` | list of strings |  |  | each entry: non-empty | Free-form tags. |
| `trackerSeed` | any |  |  |  | The start state of the tracker for the trial. Any YAML value. |
| `linearSeed` | any |  |  |  | Older name of trackerSeed. Set only one of them. |
| `startingRepoRefs` | list |  |  | each entry: matches `^(github\.com\|gitlab\.com):[^@\s]+/[^@/\s]+@[0-9a-f]{40}$`; or an object | Commits that the trial starts from. Each entry is a string github.com:OWNER/REPO@SHA or gitlab.com:OWNER/REPO@SHA, or an object {codeForge, owner, repo, ref}. codeForge is GITHUB or GITLAB. ref is a full 40-character lowercase commit SHA. |
<!-- END GENERATED: benchmark_suite_task -->

An example is [`../examples/02-sdlc-issue-to-pr/benchmarks`](../examples/02-sdlc-issue-to-pr/benchmarks).

## MCP server object

`mcpServers` is a map from a server name to a server object. It is valid in `factory.yaml`, `agentDefaults`, agents, automations, `scorerDefaults` and scorers.

```yaml
mcpServers:
  internal-docs:
    url: https://docs-mcp.internal.acme.dev/mcp
    headers:
      Authorization: "Bearer {{INTERNAL_DOCS_TOKEN}}"
  playwright:
    command: npx
    args: ["-y", "@playwright/mcp@latest"]
  sentry:
    managedId: SENTRY_MCP_INSTALLATION_ID
```

<!-- BEGIN GENERATED: mcpServer (make docs-gen) -->
| Field | Type | Required | Default | Allowed values | Description |
|---|---|---|---|---|---|
| `managedId` | string |  |  | non-empty | Name or ID of a server in the MCP server registry of the team, or a built-in server: builtin/jira, builtin/linear, builtin/slack or builtin/teams. An unknown builtin/ server is an error. |
| `command` | string |  |  | non-empty | Executable that starts inside the sandbox. The server uses the stdio transport. |
| `args` | list of strings |  |  |  | Arguments of command. Valid only with command. |
| `env` | map of string to string |  |  |  | Environment variables of command. Valid only with command. Values can use {{SECRET}} placeholders. |
| `url` | string |  |  | matches `^https?://[^/\s]+` | Absolute http or https URL with a host. The server uses streamable HTTP, or legacy SSE when the path ends in /sse. |
| `headers` | map of string to string |  |  |  | HTTP headers of url. Valid only with url. Values can use {{SECRET}} placeholders. |
| `allowWrite` | boolean |  | `false` |  | Enables the write tools of a built-in integration server. Valid only with managedId builtin/&lt;provider&gt;. |
<!-- END GENERATED: mcpServer -->

For `managedId`, see the [MCP server registry](mcp-servers.md#the-mcp-server-registry). For `allowWrite`, see [Built-in integration servers](mcp-servers.md#built-in-integration-servers).

- Each server sets **exactly one** of `managedId`, `command` or `url`.
- An unknown `builtin/` server is a validation error.
- The server name must match `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`.
- The name `factory` is reserved.
- With `managedId`, `env` and `headers` are layered over the registered server's values. They cannot replace the `Authorization` header of a proxied or built-in server.
- The [preflight](#preflight) checks that every `managedId` that is not built-in names a registered server of the team (by ID or name), for `managed` and `git` definitions.

## networkPolicy object

Restricts the outbound hosts of a run. Valid in `factory.yaml` and in agents.

<!-- BEGIN GENERATED: networkPolicy (make docs-gen) -->
| Field | Type | Required | Default | Allowed values | Description |
|---|---|---|---|---|---|
| `extends` | string or list of strings |  |  | matches `^builtin/[a-z0-9-]+@(latest\|\^[0-9]+(\.[0-9]+){0,2}\|[0-9]+\.[0-9]+\.[0-9]+)$`; or no duplicates; each entry: matches `^builtin/[a-z0-9-]+@(latest\|\^[0-9]+(\.[0-9]+){0,2}\|[0-9]+\.[0-9]+\.[0-9]+)$` | Built-in policies to start from, for example builtin/package-registries@^1, builtin/claude-code@1.0.0 or builtin/codex@latest. Each reference must resolve. |
| `allowedHosts` | list of strings |  |  | each entry: host names or literal IP addresses | Hosts that the run can reach: host names or literal IP addresses. No URLs, ports, CIDR ranges or wildcards. Stored in lowercase. Duplicates are removed. |
| `deniedHosts` | list of strings |  |  | each entry: host names or literal IP addresses | Hosts that the run cannot reach. The rules of allowedHosts apply. A host that any level denies is removed. |
<!-- END GENERATED: networkPolicy -->

- If neither the factory nor the agent declares a policy, outbound access is unrestricted.
- Otherwise the run is restricted, even by a policy with only `deniedHosts`. Allowed hosts from both levels combine; a host denied at either level is removed.

Enforcement, the built-in policy catalog and version selectors are in [Network policy](self-hosting/network-policy.md).

## Secret names

A secret name in a definition must match `^[A-Za-z_][A-Za-z0-9_]{0,127}$`. The secret store is stricter: only uppercase letters, digits and `_`, and no `OF_` prefix. Use uppercase names, for example `NPM_TOKEN`. See [Secrets](secrets.md).

A definition holds secret **names** only. Never put a secret value in a definition file.

## {{SECRET}} templating

MCP server `env` values and `headers` values can reference secrets with `{{NAME}}` placeholders. Whitespace inside the braces is allowed (`{{ NAME }}`).

```yaml
headers:
  Authorization: "Bearer {{INTERNAL_DOCS_TOKEN}}"
```

- Placeholders are replaced when the run starts, from the factory's secrets first, then the team's.
- The secret does not need to be in the agent's `secrets` list.
- If a referenced secret does not exist, the run fails with `unknown secret(s) referenced: <names>`.
- Placeholders are not expanded in `command`, `args`, `url` or any other field.
- Quote a value that starts with `{{`. YAML otherwise reads it as a mapping.
