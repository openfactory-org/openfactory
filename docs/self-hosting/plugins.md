# Plugins

A plugin is a separate Cloudflare Worker that the control plane calls over a service binding. Plugins add integrations without a change to the control plane. GitLab and Azure DevOps ship as plugin Workers; GitHub, Slack, Microsoft Teams, Jira and Linear are built in.

## Plugin roles

A plugin serves one or more roles through typed RPC (`WorkerEntrypoint`):

| Role | What it does |
|---|---|
| `forge` | Reads and writes pull requests, reviews and commit statuses on a code forge. |
| `responder` | Writes results back to a tracker or chat. |
| `ingress` | Turns inbound webhooks into events. |
| `sandbox` | Starts sandboxes, as a [sandbox provider](../sandbox-providers.md#plugin-providers). |

The interfaces are in `packages/plugin-api`. A plugin reports its name, its plugin API version and its roles. The control plane refuses a plugin whose API version differs from its own.

## Bundled plugins

| Plugin | Binding | Deploy |
|---|---|---|
| GitLab | `PLUGIN_GITLAB` | `npx wrangler deploy -c plugins/gitlab/wrangler.jsonc` |
| Azure DevOps | `PLUGIN_AZUREDEVOPS` | `npx wrangler deploy -c plugins/azuredevops/wrangler.jsonc` |

Deploy a plugin Worker before the control plane, because the control plane config binds it. A plugin Worker is reached only through the service binding. It has no public route and holds no team secrets: the control plane gives it the integration settings and tokens for each call.

The bundled plugins need no variables. They read their settings from integration records (provider `gitlab` or `azure_devops`) and secrets.

## Webhooks

The control plane receives webhooks at `POST ${OF_PUBLIC_URL}/plugins/<name>/webhook` and hands the request to the plugin, which verifies and parses it. A plugin calls back into the control plane through a host object that each call carries.

## Write your own plugin

1. Create a Worker that exports a class extending `WorkerEntrypoint`, implementing the interfaces of `@openfactory/plugin-api` for its roles. `examples/plugin-worker/` is a minimal example.
2. Deploy it with `wrangler deploy`.
3. Add a service binding to your build of the control plane. A custom binding needs a custom control plane Worker. See [examples/control-plane-builder](../../examples/control-plane-builder/).

### Optional forge and responder methods

A plugin throws `operation_not_supported` for a method that it does not support. Plugins built before these methods existed keep working.

| Method | Role | Use | Without it |
|---|---|---|---|
| `ReplyInThread`, `ResolveThread`, `SubmitReview`, `MarkReady` | forge | Review actions. | A thread reply becomes a top-level comment. |
| `SetIssueState`, `AttachPullRequest` | responder | Tracker write-back. | The write-back is skipped. |
| `Login` | forge | Returns the reviewer name of a credential. Called with the personal token of a member. | |
| `RequestReviewers` | forge | Adds reviewers to a pull request, for example for a self-improvement change. | |
| `GetReviewThread`, `GetFileDiff` | forge | Give the thread and one file's diff to an agent that starts from a review comment. | |

### Action keys

An event has an optional `action_key`. All events from one provider action (for example one webhook delivery) must share the key. The engine handles them together, so one comment does not give two messages. If the key is empty, the engine builds it from the source.

See also [GitLab](../integrations/gitlab.md), [Azure DevOps](../integrations/azure-devops.md) and [Configuration](configuration.md#bindings).
