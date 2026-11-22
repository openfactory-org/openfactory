// The Azure DevOps plugin Worker: the Azure Repos code forge (pull requests,
// threads, statuses, work item comments and states, definition trees) and the
// service-hook receiver. The control plane binds it as env.PLUGIN_AZUREDEVOPS
// and calls the public methods of `Plugin` with typed RPC.
//
// All settings come from integration records of provider "azure_devops" (config
// org, base_url, hook_username, hook_password_ref, factory_identities,
// service_identities). The control plane resolves the token and passes it in
// ForgeTarget.token.
//
// Worker vars: PLUGIN_VERSION (optional version string).

import type {
  CommitStatus,
  FileDiff,
  ForgePlugin,
  ForgeTarget,
  HostApi,
  IngressPlugin,
  PluginInfo,
  PullRequestInput,
  PullRequestRef,
  ReviewInput,
  ReviewThread,
  WebhookResult,
} from "@openfactory/plugin-api";
import { notSupported, rpc } from "@openfactory/plugin-api";
import { PluginWorker, pluginInfo } from "@openfactory/plugin-api/worker";
import { AzureDevOpsClient, FORGE_NAME } from "./client";
import { PROVIDER, handleWebhook } from "./webhook";

export interface Env {
  PLUGIN_VERSION?: string;
}

/** The JSON Schema of an integration record of provider "azure_devops". */
export const CONFIG_SCHEMA = JSON.stringify({
  $schema: "https://json-schema.org/draft/2020-12/schema",
  type: "object",
  properties: {
    org: { type: "string", description: "Organization name." },
    base_url: { type: "string", description: "Azure DevOps Services root. Default https://dev.azure.com." },
    hook_username: { type: "string", description: "User name of the service hook basic authentication." },
    hook_password_ref: { type: "string", description: "Name of the secret that holds the service hook password." },
    factory_identities: {
      type: "string",
      description: "Factory identities: identityGUID=alias,identityGUID2=alias2.",
    },
    service_identities: { type: "string", description: "Other identity GUIDs whose activity is ignored." },
  },
  required: ["org"],
});

/** Builds the Azure DevOps client of one integration (or user token). */
export function clientFor(t: ForgeTarget, fetchFn?: typeof fetch): AzureDevOpsClient {
  return new AzureDevOpsClient({
    organization: t.config["org"] ?? "",
    ...(t.config["base_url"] ? { baseUrl: t.config["base_url"] } : {}),
    token: t.token,
    ...(fetchFn ? { fetch: fetchFn } : {}),
  });
}

export class Plugin extends PluginWorker<Env> implements ForgePlugin, IngressPlugin {
  async info(): Promise<PluginInfo> {
    return pluginInfo(
      {
        name: "azuredevops",
        roles: ["forge", "ingress"],
        forgeProviders: [PROVIDER],
        integrationProviders: [PROVIDER],
        configSchema: CONFIG_SCHEMA,
      },
      this.env.PLUGIN_VERSION,
    );
  }

  async name(_t: ForgeTarget): Promise<string> {
    return FORGE_NAME;
  }

  cloneUrl(t: ForgeTarget, owner: string, repo: string): Promise<string> {
    return rpc(() => clientFor(t).cloneUrl(owner, repo));
  }

  defaultBranch(t: ForgeTarget, owner: string, repo: string): Promise<string> {
    return rpc(() => clientFor(t).defaultBranch(owner, repo));
  }

  createPullRequest(t: ForgeTarget, owner: string, repo: string, pr: PullRequestInput): Promise<PullRequestRef> {
    return rpc(() => clientFor(t).createPullRequest(owner, repo, pr));
  }

  getPullRequest(t: ForgeTarget, owner: string, repo: string, number: number): Promise<PullRequestRef> {
    return rpc(() => clientFor(t).getPullRequest(owner, repo, number));
  }

  comment(t: ForgeTarget, owner: string, repo: string, number: number, body: string): Promise<void> {
    return rpc(() => clientFor(t).comment(owner, repo, number, body));
  }

  /** Azure DevOps has no separate issue comments: the host falls back to comment. */
  commentIssue(_t: ForgeTarget, _owner: string, _repo: string, _number: number, _body: string): Promise<void> {
    return rpc(() => {
      throw notSupported("azuredevops: issue comments");
    });
  }

  commentWorkItem(t: ForgeTarget, project: string, id: number, body: string): Promise<void> {
    return rpc(() => clientFor(t).commentWorkItem(project, id, body));
  }

  setCommitStatus(t: ForgeTarget, owner: string, repo: string, sha: string, status: CommitStatus): Promise<void> {
    return rpc(() => clientFor(t).setCommitStatus(owner, repo, sha, status));
  }

  getFiles(
    t: ForgeTarget,
    owner: string,
    repo: string,
    ref: string,
    path: string,
  ): Promise<{ files: Record<string, string>; commitSha: string }> {
    return rpc(() => clientFor(t).getFiles(owner, repo, ref, path));
  }

  replyInThread(
    t: ForgeTarget,
    owner: string,
    repo: string,
    number: number,
    threadRef: string,
    body: string,
  ): Promise<void> {
    return rpc(() => clientFor(t).replyInThread(owner, repo, number, threadRef, body));
  }

  resolveThread(t: ForgeTarget, owner: string, repo: string, number: number, threadRef: string): Promise<void> {
    return rpc(() => clientFor(t).resolveThread(owner, repo, number, threadRef));
  }

  submitReview(t: ForgeTarget, owner: string, repo: string, number: number, review: ReviewInput): Promise<void> {
    return rpc(() => clientFor(t).submitReview(owner, repo, number, review));
  }

  markReady(t: ForgeTarget, owner: string, repo: string, number: number): Promise<void> {
    return rpc(() => clientFor(t).markReady(owner, repo, number));
  }

  setWorkItemState(t: ForgeTarget, project: string, id: number, state: string): Promise<void> {
    return rpc(() => clientFor(t).setWorkItemState(project, id, state));
  }

  attachPullRequestToWorkItem(t: ForgeTarget, project: string, id: number, pr: PullRequestRef): Promise<void> {
    return rpc(() => clientFor(t).attachPullRequestToWorkItem(project, id, pr));
  }

  requestReviewers(t: ForgeTarget, owner: string, repo: string, number: number, logins: string[]): Promise<void> {
    return rpc(() => clientFor(t).requestReviewers(owner, repo, number, logins));
  }

  login(t: ForgeTarget): Promise<string> {
    return rpc(() => clientFor(t).login());
  }

  getReviewThread(
    t: ForgeTarget,
    owner: string,
    repo: string,
    number: number,
    threadRef: string,
  ): Promise<ReviewThread> {
    return rpc(() => clientFor(t).reviewThread(owner, repo, number, threadRef));
  }

  getFileDiff(t: ForgeTarget, owner: string, repo: string, number: number, path: string): Promise<FileDiff> {
    return rpc(() => clientFor(t).fileDiff(owner, repo, number, path));
  }

  handleWebhook(req: Request, host: HostApi): Promise<WebhookResult> {
    return rpc(() => handleWebhook(req, host));
  }
}

export default {
  fetch(): Response {
    return new Response("openfactory-plugin-azuredevops: call this Worker through a service binding", { status: 404 });
  },
};
