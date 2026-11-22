# Quickstart

Start a local control plane with `wrangler dev`, connect GitHub, create a factory from an example, and send a first work item.

## Before you start

You need:

- Node.js 24 or later, Go (the version in `go.mod`), `git` and `curl`.
- The `claude` program (Claude Code) on your `PATH`. The example factory uses the `claude` harness, and in development mode a development worker runs it on your workstation.
- An Anthropic API key.
- A GitHub repository that you can write to, and a fine-grained personal access token with read and write access to contents, issues and pull requests.

The local setup is for one user on a workstation. The development worker runs agents with no isolation. To deploy for a team, see [Deploy to Cloudflare](self-hosting/cloudflare.md).

## 1. Start the control plane and a worker

```bash
git clone https://github.com/openfactory-org/openfactory.git
cd openfactory
npm ci
npm run build
```

Create `apps/control-plane/.dev.vars` with development keys and the development provider:

```text
OF_SECRET_KEY=<output of: openssl rand -base64 32>
OF_SIGNING_KEY=<output of: openssl rand -base64 32>
OF_R2_ACCESS_KEY_ID=dev
OF_R2_SECRET_ACCESS_KEY=dev
OF_SANDBOX_PROVIDER=local
OF_DEV_PROCESS_WORKERS=true
```

Start the control plane. It listens on <http://localhost:8787> and keeps its data in `.wrangler/`:

```bash
make dev
```

In a second terminal, install the CLI from the checkout, set the variables that the client commands and the `curl` examples in these docs read, and create the first team:

```bash
export OF_SERVER=http://localhost:8787
export OF_PUBLIC_URL=$OF_SERVER   # used by the curl examples on other pages
openfactory bootstrap --email you@example.com
export OF_API_KEY=of_...          # the key that the command printed, shown once
openfactory factory list          # checks that the key works
```

Start a development worker in a third terminal. It runs each session as a host process under `~/.openfactory/sandboxes`, with no isolation:

```bash
export OF_SERVER=http://localhost:8787 OF_API_KEY=of_...
make dev-worker
```

To use the dashboard, run `make web-dev`, or open <http://localhost:8787> after `npm run build`, and sign in with the API key.

## 2. Store the model credential

The example factory reads this secret in `agentDefaults.harness.auth`:

```bash
echo "sk-ant-..." | openfactory secret set ANTHROPIC_API_KEY --type anthropic_api_key
```

To use Codex instead, store an OpenAI key with `--type openai_api_key` and set the harness to `codex`. See [Harnesses](harnesses.md).

## 3. Connect GitHub

Store the token. The command reads the value from standard input:

```bash
echo "github_pat_..." | openfactory secret set GITHUB_TOKEN
```

Create a GitHub integration record that uses the secret. Replace `acme` with the user or organization that owns the repository. `org` is required: the server uses the record only for repositories of that owner.

```bash
curl -s -X POST "$OF_SERVER/api/v1/integrations" \
  -H "Authorization: Bearer $OF_API_KEY" -H "Content-Type: application/json" \
  -d '{"provider": "github", "secret_ref": "GITHUB_TOKEN", "config": {"org": "acme"}}'
```

The factory can now clone the repository, push branches and open pull requests. To start work from GitHub events, the control plane must also receive webhooks, so it needs a public URL. See [GitHub](integrations/github.md).

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
   openfactory validate /tmp/my-factory
   ```

4. Upload it. This creates a factory with a `managed` definition, named by `factory.yaml` (`quickstart`):

   ```bash
   openfactory factory create --from /tmp/my-factory
   ```

To change the definition later, edit the files and run `openfactory factory apply /tmp/my-factory --factory quickstart`.

## 5. Send a work item

Send a work item to the foreman. `-f` streams the transcript:

```bash
openfactory factory run quickstart -f \
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
- Deploy for a team to [Cloudflare](self-hosting/cloudflare.md), then turn on [OIDC single sign-on](self-hosting/oidc.md).

## Troubleshooting

| Symptom | Fix |
|---|---|
| `wrangler dev` reports a configuration error for `OF_SECRET_KEY` | Create `apps/control-plane/.dev.vars` as in step 1. |
| `openfactory bootstrap` fails because a team exists | The first key is shown only once. Create a key with `POST /api/v1/api-keys` and an existing key, or delete `.wrangler/` to start again. |
| The run stays `queued` | No worker is online. Start `make dev-worker`, and check `openfactory worker list`. |
| The run fails with `the agent has no harness credential` | Store the key as in step 2 and keep `harness.auth` in `agentDefaults`. Without `harness.auth`, keep exactly one secret of a model credential type. See [Default credential](harnesses.md#default-credential). |
| The run fails because `claude` is not found | Install Claude Code so that `claude` is on your `PATH`. |
| The run fails at checkout | Check that the GitHub integration record exists and that the token can read the repository. |

See [Troubleshooting](troubleshooting.md) for more.
