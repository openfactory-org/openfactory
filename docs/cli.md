# Command-line client

The `openfactory` command is a TypeScript program that you install from npm. It talks to the REST API of a control plane, checks factory definitions on your machine, and bridges the Factory MCP to standard input and output.

## Install

You need Node.js 24 or later.

```bash
npm install --global openfactory
openfactory version
```

## Connect to a control plane

Client commands read the control plane URL and an API key from flags or from the environment.

| Flag | Environment variable | Default | Description |
|---|---|---|---|
| `--server URL` | `OF_SERVER` | `http://localhost:8787` | Base URL of the control plane. |
| `--api-key KEY` | `OF_API_KEY` | none | A team API key (`of_...`). |
| `-o, --output` | none | `text` | `text` or `json`. |

A flag overrides its variable. The help text never shows the value of `OF_API_KEY`.

```bash
export OF_SERVER=https://factory.example.com
export OF_API_KEY=of_...
openfactory factory list
```

On a new control plane, `openfactory bootstrap --email you@example.com` creates the first team, the admin user and an API key. The command prints the key once and fails when the control plane already has a team.

## Commands

| Command | Purpose |
|---|---|
| `bootstrap` | Create the first team, admin user and API key. |
| `factory` | `list`, `get`, `create`, `apply`, `run`. |
| `run` | `list`, `get`, `logs`, `followup`, `cancel`. |
| `work-item` | `list`, `get`, `send`, `complete`. |
| `inbox` | `list`, `resolve`, `unresolve`. |
| `secret` | `list`, `set`, `delete`, `rewrap`. `rewrap` calls `POST /api/v1/secrets/rewrap` and wraps the data keys of all secrets again with the current `OF_SECRET_KEY`. |
| `webhook` | `list`, `create`, `deliveries`. |
| `benchmark` | `list`, `run`. |
| `mcp-registry` | `add`, `connect`, `disconnect`, `get`, `list`, `delete`. |
| `worker list` | List the workers of the team. |
| `mcp` | Bridge the Factory MCP to standard input and output. See [Factory MCP](factory-mcp.md). |
| `init`, `schema`, `validate` | Local tools. They need no control plane. |
| `version` | Print the version. `--short` prints only the number. |

Run `openfactory <command> --help` for the flags of a command.

## Local definition tools

```bash
openfactory init my-factory --name "Payments" --repo acme/payments
openfactory validate my-factory
openfactory schema factory
```

`validate` checks a definition tree against the v1 JSON Schemas and the semantic rules, and compiles it. Errors show as `file:line: message`. The exit status is 1 for an invalid definition.

## Relation to `openfactory-runtime`

Two programs exist:

| Program | Language | Runs on | Purpose |
|---|---|---|---|
| `openfactory` | TypeScript (npm) | Your workstation, CI | Client and local tools. |
| `openfactory-runtime` | Go | Sandboxes and worker hosts | The runner, the egress proxy (`netguard`), the development process worker and the microVM worker agent. |

The runtime commands `runner`, `netguard`, `worker` and `vm-init` belong to `openfactory-runtime`. If `openfactory-runtime` is on your `PATH`, `openfactory worker <arguments>` and the other runtime commands pass their arguments to it. `openfactory worker list` is a client command and always talks to the control plane.

The control plane runs on Cloudflare. Deploy it with `wrangler deploy`, and run it locally with `wrangler dev`. The CLI has no command that runs the control plane. See [Deploy to Cloudflare](self-hosting/cloudflare.md).
