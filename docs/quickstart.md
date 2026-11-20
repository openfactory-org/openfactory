# Quickstart

Start a local server in development mode, connect GitHub, create a factory from an example, and send a first work item.

## Before you start

You need:

- Go (the version in `go.mod`), `git` and `curl`.
- The `claude` program (Claude Code) on your `PATH`. The example factory uses the `claude` harness, and in development mode the server runs it on your workstation.
- An Anthropic API key.
- A GitHub repository that you can write to, and a fine-grained personal access token with read and write access to contents, issues and pull requests.

Development mode is for one user on a workstation. For a team server, see [Self-hosting](self-hosting/README.md).

## 1. Build and start the server

```bash
git clone https://github.com/openfactory-org/openfactory.git
cd openfactory
make build
OF_DEV=true ./bin/openfactory serve
```

On the first start in development mode, the server:

- stores data in SQLite at `~/.openfactory/openfactory.db`;
- creates a secret key at `~/.openfactory/secret.key`;
- creates a team and the admin `admin@localhost`, and prints an API key one time;
- uses the `local` sandbox backend: one host process per sandbox, under `~/.openfactory/sandboxes`, with no isolation.

Open a second terminal and set the variables that the client commands and the `curl` examples in these docs read:

```bash
export OF_SERVER=http://localhost:8080
export OF_API_KEY=of_...          # the key that the server printed
export OF_PUBLIC_URL=$OF_SERVER   # used by the curl examples on other pages
./bin/openfactory factory list    # checks that the key works
```

To use the dashboard, open <http://localhost:8080> and sign in with the printed API key.

## 2. Store the model credential

The example factory reads this secret in `agentDefaults.harness.auth`:

```bash
echo "sk-ant-..." | ./bin/openfactory secret set ANTHROPIC_API_KEY --type anthropic_api_key
```

To use Codex instead, store an OpenAI key with `--type openai_api_key` and set the harness to `codex`. See [Harnesses](harnesses.md).

## 3. Connect GitHub

Store the token. The command reads the value from standard input:

```bash
echo "github_pat_..." | ./bin/openfactory secret set GITHUB_TOKEN
```

Create a GitHub integration record that uses the secret. Replace `acme` with the user or organization that owns the repository. `org` is required: the server uses the record only for repositories of that owner.

```bash
curl -s -X POST "$OF_SERVER/api/v1/integrations" \
  -H "Authorization: Bearer $OF_API_KEY" -H "Content-Type: application/json" \
  -d '{"provider": "github", "secret_ref": "GITHUB_TOKEN", "config": {"org": "acme"}}'
```

The factory can now clone the repository, push branches and open pull requests. To start work from GitHub events, the server must also receive webhooks. See [GitHub](integrations/github.md).

## 4. Create a factory from an example

The example [01-single-repo-quickstart](../examples/01-single-repo-quickstart/) has one foreman agent and one automation. The automation starts work when an issue gets the label `factory-ready`.

1. Copy the example:

   ```bash
   cp -R examples/01-single-repo-quickstart /tmp/my-factory
   ```

2. Point it at your repository:

   | File | Change |
   |---|---|
   | `/tmp/my-factory/factory.yaml` | Under `repositories`, set `owner: acme` to your owner and `name: payments-service` to your repository name. |
   | `/tmp/my-factory/automations/labeled-issue/automation.md` | In `repos: [acme/payments-service]`, use `<owner>/<name>`. |

3. Validate the definition. On failure, each error shows its file, line and field (see the [definition reference](definition-reference.md)):

   ```bash
   ./bin/openfactory validate /tmp/my-factory
   ```

4. Upload it. This creates a factory with a `managed` definition, named by `factory.yaml` (`quickstart`):

   ```bash
   ./bin/openfactory factory create --from /tmp/my-factory
   ```

To change the definition later, edit the files and run `openfactory factory apply /tmp/my-factory --factory quickstart`.

## 5. Send a work item

Send a work item to the foreman. `-f` streams the transcript:

```bash
./bin/openfactory factory run quickstart -f \
  "Add a /healthz endpoint that returns 200 with a unit test. Open a draft pull request."
```

The command prints the run ID, the work item ID and the dashboard link. To follow the work:

- In the dashboard, select the factory. The **Work** page shows the work item, its stage and its runs.
- Use `openfactory run list` and `openfactory run logs <run> -f`.

If the foreman asks a question or requests an approval, the item appears in the **Inbox**. Answer in the dashboard, or with `openfactory inbox list` and `openfactory inbox resolve <id> <resolution>`. The foreman gets the answer as a follow-up message.

The foreman links the pull request to the work item. The work item completes when the pull request merges (this needs GitHub webhooks), or when you run `openfactory work-item complete <id>`.

## 6. Next steps

- Add the stage agents from [00-default-agents](../examples/00-default-agents/) (triage, spec, implement, review).
- Start work from GitHub, Slack, Linear or Jira: [Automations](automations.md) and [Integrations](integrations/README.md).
- Connect your local coding agent: [Factory MCP](factory-mcp.md).
- Deploy for a team on [Kubernetes](self-hosting/kubernetes.md), then turn on [OIDC single sign-on](self-hosting/oidc.md).

## Troubleshooting

| Symptom | Fix |
|---|---|
| `OF_SECRET_KEY is required` at start | Set `OF_DEV=true`, or set `OF_SECRET_KEY` to the output of `openssl rand -base64 32`. |
| The server printed no API key | The team exists already; the key prints only on the first start. Create a key with `POST /api/v1/api-keys` and an existing key, or delete `~/.openfactory` to start again. |
| The run fails with `the agent has no harness credential` | Store the key as in step 2 and keep `harness.auth` in `agentDefaults`. Without `harness.auth`, keep exactly one secret of a model credential type. See [Default credential](harnesses.md#default-credential). |
| The run fails because `claude` is not found | Install Claude Code so that `claude` is on your `PATH`. |
| The run fails at checkout | Check that the GitHub integration record exists and that the token can read the repository. |

See [Troubleshooting](troubleshooting.md) for more.
