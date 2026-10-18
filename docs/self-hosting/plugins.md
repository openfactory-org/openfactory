# Plugins

A plugin is an executable that the server starts and talks to over gRPC. Plugins add integrations without a rebuild. GitLab and Azure DevOps ship as plugins; GitHub, Slack, Microsoft Teams, Jira and Linear are built in.

## Install plugins

The server starts every regular executable named `openfactory-plugin-<name>` in `OF_PLUGIN_DIR` (default `<data dir>/plugins`) and ignores other files. `OF_PLUGINS` adds executables by path, with any file name (comma-separated).

The server image ships the GitLab and Azure DevOps plugins in `/usr/local/lib/openfactory/plugins` and sets `OF_PLUGIN_DIR` to it. The release archives contain the same binaries.

Build the bundled plugins from a checkout:

```bash
make plugins          # bin/plugins/openfactory-plugin-gitlab, bin/plugins/openfactory-plugin-azuredevops
OF_PLUGIN_DIR=bin/plugins ./bin/openfactory serve
```

A plugin binary must match the server's OS and architecture (`GOOS=linux`, `GOARCH=amd64` or `arm64` for the container image).

### Helm

The chart mounts an empty directory at `/opt/openfactory/plugins` and sets `OF_PLUGIN_DIR` to it. With `plugins.bundled: true` (default), an init container copies the bundled plugins into it. Each `plugins.images` entry adds an init container that copies the executables from `path` in that image:

```yaml
plugins:
  images:
    - name: gitlab
      image: registry.example.com/factory/openfactory-plugin-gitlab:0.2.0
      path: /plugins
```

A plugin image needs `sh`, `cp` and the executables under `path`. A minimal Dockerfile:

```dockerfile
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /plugins/openfactory-plugin-gitlab ./plugins/gitlab

FROM busybox:1.37
COPY --from=build /plugins /plugins
```

## Configure plugins

A plugin does not get the server environment. It gets, when the server has them:

- `PATH`, `HOME`, `TMPDIR`, `LANG` and `TZ`;
- the variables of the plugin protocol;
- the variables in `OF_PLUGIN_ENV_ALLOW` (all plugins; Helm `plugins.envAllow`);
- the variables listed for this plugin in `OF_PLUGIN_ENV` (Helm `plugins.env`).

The bundled GitLab and Azure DevOps plugins need no variables. They read their settings from integration records (provider `gitlab` or `azure_devops`) and secrets, which the server serves to the plugin.

Put the values in the server environment (Helm `server.env` or `server.secretEnv`), then list the names:

```bash
OF_PLUGIN_ENV_ALLOW=ACME_API_URL,ACME_API_TOKEN ./bin/openfactory serve
```

`OF_PLUGIN_ENV` gives variables to one plugin. Each entry is the plugin name (the file name without `openfactory-plugin-`), `=`, and a comma-separated list. Entries are separated by `;`.

```bash
OF_PLUGIN_ENV='acme=ACME_API_URL,ACME_API_TOKEN;other=OTHER_URL' ./bin/openfactory serve
```

```yaml
# config file (Helm: plugins.env takes the same map)
plugin_env:
  acme: [ACME_API_URL, ACME_API_TOKEN]
```

The server never passes these, even when listed: `OF_DATABASE_URL`, `OF_SECRET_KEY*`, `OF_SIGNING_KEY*`, `OF_OIDC_CLIENT_SECRET`, `OF_OBJECT_STORE_*`, and every variable that ends with `_API_KEY`.

> **Caution:** `OF_PLUGIN_ENV_ALLOW` applies to all plugins. List only variables that every plugin may read. Use `OF_PLUGIN_ENV` for a variable that one plugin needs. A plugin can still read the data that the server gives it over the plugin protocol, such as the secrets of its own integrations.

## Sandbox backend plugins

A plugin can be the sandbox backend: it starts one sandbox per session, and the runner runs inside it. The plugin serves `openfactory.plugin.v2.SandboxBackend` next to the standard plugin service. This is the only backend service a plugin can serve.

```bash
OF_SANDBOX_BACKEND=acme ./bin/openfactory serve
```

The server fails to start if no loaded plugin has the name, if the plugin has no SandboxBackend service, or if its preflight call fails. Logs name the backend `plugin:<name>`.

> **Warning:** Each `StartSandbox` call carries the sandbox spec, with the session bootstrap token in `env`. The plugin must keep the spec out of its logs. The plugin controls the sandbox, so you must trust it with the secrets of the runs that it serves.

| gRPC status | Meaning |
|---|---|
| `NOT_FOUND` | Unknown sandbox. |
| `INVALID_ARGUMENT` | Invalid sandbox spec. |
| `RESOURCE_EXHAUSTED` | No free slot. The run stays queued. |
| `UNAVAILABLE` | Temporary fault. The server can retry. |

`WatchSandbox` sends the current status first, then one status per change until the sandbox exited. Implement `sdk.SandboxBackend` of `pkg/plugin/sdk` and set the `SandboxBackend` field of `sdk.Plugin`. The service is defined in `proto/openfactory/plugin/v2/sandbox.proto`.

## Operation

- Plugins receive webhooks at `POST ${OF_PUBLIC_URL}/plugins/<name>/webhook`.
- The server starts plugins at boot and checks the handshake (protocol version 1, mutual TLS on a local socket).
- A crashed plugin restarts with exponential backoff from 1 second to 1 minute.
- Every replica runs its own plugin processes.

### Optional forge and responder methods

A plugin returns `UNIMPLEMENTED` for a method that it does not support. The server reads that as `operation_not_supported`. Plugins built before these methods existed keep working.

| Method | Service | Use | Without it |
|---|---|---|---|
| `ReplyInThread`, `ResolveThread`, `SubmitReview`, `MarkReady` | forge | Review actions. | A thread reply becomes a top-level comment. |
| `SetIssueState`, `AttachPullRequest` | responder | Tracker write-back. | The write-back is skipped. |
| `Login` | forge | Returns the reviewer name of a credential (GitLab user name, Azure DevOps identity ID). Called with the member's personal token. | |
| `RequestReviewers` | forge | Adds reviewers to a pull request, for example for a self-improvement change. Called with the team credential. | |
| `GetReviewThread`, `GetFileDiff` | forge | Give the thread and one file's diff to an agent that starts from a review comment. Return the diff without the file header, or `NOT_FOUND` when the forge has no diff. | |

### Action keys

An event has an optional `action_key`. All events from one provider action (for example one webhook delivery) must share the key. The engine handles them together, so one comment does not give two messages. If the key is empty, the engine builds it from the provider name and `delivery_id`, without a `:<event type>` suffix. In the Go SDK, set `ActionKey` on the `Event`. The GitLab and Azure DevOps plugins use the delivery ID.

See also [GitLab](../integrations/gitlab.md), [Azure DevOps](../integrations/azure-devops.md) and [Configuration](configuration.md#plugins).
