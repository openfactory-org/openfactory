# Other code forges

A factory can use a repository on a forge with no built-in integration, such as Bitbucket, Azure DevOps Server, Gitea or a plain git server. The machine setup commands clone the repository, and a custom webhook can start work from forge events. This setup needs no integration record.

> **Note:** This setup gives agents repository access only. It adds no provider events, comments, pull requests or status checks. Agents can use the forge CLI or API if you give them a token.

Self-managed GitLab and Azure DevOps Services have full integrations: see [GitLab](gitlab.md) and [Azure DevOps](azure-devops.md).

You need an HTTPS clone URL, a token with read access (read and write if agents push commits), and an API key with the editor role in `OF_API_KEY`.

## 1. Create the factory without repositories

Set `codeForges` to an empty list. `repositories` must then be empty too.

```yaml
# factory.yaml
schemaVersion: v1
name: legacy-billing
alias: billing
codeForges: []
repositories: []
agentDefaults:
  model: claude-opus-5-5
  machine: linux
  secrets:
    - CODE_FORGE_TOKEN
```

## 2. Store the clone credential

```bash
openfactory secret set CODE_FORGE_TOKEN
```

Grant the secret with `secrets` in `agentDefaults` or in each agent. The agents and the machine setup commands then get it as the `CODE_FORGE_TOKEN` environment variable. See [Secrets](../secrets.md).

## 3. Clone the repository in the machine setup

Add an authenticated `git clone` to `setupCommands`. Setup commands run each time the runner prepares a workspace, so make them safe to run again.

```yaml
# machines/linux.yaml
platform:
  os: linux
  linux:
    image: ghcr.io/openfactory-org/sandbox:latest # container image
setupCommands:
  - |
    if [ ! -d "billing/.git" ]; then
      git clone "https://x-bitbucket-api-token-auth:${CODE_FORGE_TOKEN}@bitbucket.org/acme/billing.git" billing
    fi
```

- The image must have `git`.
- Clone into a subdirectory. The workspace root belongs to `root`, and `git` refuses to work in it for the agent user ("dubious ownership").
- The platform adds `AGENTS.md`, `AGENT.md` and `CLAUDE.md` of each cloned subdirectory to the system prompt. See [What each harness loads from the repository](../harnesses.md#what-each-harness-loads-from-the-repository).

| Forge | Clone command |
| --- | --- |
| Bitbucket Cloud | `git clone "https://x-bitbucket-api-token-auth:${CODE_FORGE_TOKEN}@bitbucket.org/WORKSPACE/REPO.git"` |
| Azure DevOps Server | ``git -c http.extraheader="Authorization: Basic $(printf ':%s' "$CODE_FORGE_TOKEN" \| base64 \| tr -d '\n')" clone "https://SERVER/COLLECTION/PROJECT/_git/REPO"`` |
| Gitea, Forgejo | `git clone "https://oauth2:${CODE_FORGE_TOKEN}@git.example.com/OWNER/REPO.git"` |
| Self-managed GitLab | `git clone "https://oauth2:${CODE_FORGE_TOKEN}@gitlab.example.com/GROUP/REPO.git"` |

For Azure DevOps Server, create the personal access token at `https://SERVER/COLLECTION` with the **Code** > **Read** scope (**Read & Write** if agents push commits).

For all machine fields, see [Machines and environments](../machines-and-environments.md).

## 4. Test repository access

Start a read-only run:

```bash
curl -X POST "$OF_PUBLIC_URL/api/v1/factories/$FACTORY_UID/runs" \
  -H "Authorization: Bearer $OF_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"prompt": "In the billing directory, list the top-level files and report the current branch. Do not change any files."}'
```

The run must report files from the repository. If the clone fails, check that the secret is granted to the agent, the token has read access, and the clone command reads `CODE_FORGE_TOKEN`.

## 5. Start work from forge events

Use a custom webhook:

1. Create a webhook. Use `token` mode if the forge can set an `Authorization` header, or `url_token` mode if it accepts only a URL. See [Custom webhooks](../webhooks.md).
2. In the forge, add a webhook that posts JSON to the ingress URL.
3. Add an automation with a `provider: webhook` trigger and a payload pattern:

   ```markdown
   ---
   triggers:
     - provider: webhook
       event: received
       filter:
         webhook_ids: [bitbucket-prs]
         payload:
           pullrequest:
             destination:
               branch:
                 name: [main]
   ---

   A pull request was opened in the billing repository. Review the
   change in the billing directory and report your findings.
   ```

Chat tools, trackers, schedules, the REST API and Factory MCP can also start work. See [Integrations](README.md).

## Related pages

- [Integrations](README.md)
- [Custom webhooks](../webhooks.md)
- [Machines and environments](../machines-and-environments.md)
