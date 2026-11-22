// The GitLab plugin Worker: the GitLab code forge (merge requests, notes,
// commit statuses, definition trees, review threads) and the GitLab webhook
// receiver. The control plane binds it as env.PLUGIN_GITLAB and calls the
// public methods of `Plugin` with typed RPC.
//
// All settings come from integration records of provider "gitlab" (config
// base_url, web_url, token_type, user_token_type, org, bot_username,
// factory_bots, service_accounts, webhook_secret_ref). The control plane
// resolves the token and passes it in ForgeTarget.token.
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
import { FORGE_NAME, GitLabClient } from "./client";
import { PROVIDER, handleWebhook } from "./webhook";

export interface Env {
  PLUGIN_VERSION?: string;
}

/** The JSON Schema of an integration record of provider "gitlab". */
export const CONFIG_SCHEMA = JSON.stringify({
  $schema: "https://json-schema.org/draft/2020-12/schema",
  type: "object",
  properties: {
    org: { type: "string", description: "Top-level group the record covers." },
    base_url: { type: "string", description: "API root, for example https://gitlab.example.com/api/v4." },
    web_url: { type: "string", description: "Git host root. Derived from base_url when empty." },
    token_type: { type: "string", enum: ["private", "oauth"], description: "Header style of the integration token." },
    user_token_type: { type: "string", enum: ["private", "oauth"], description: "Header style of personal tokens." },
    bot_username: { type: "string", description: "Team-wide bot account." },
    factory_bots: { type: "string", description: "Per-factory bot accounts: username=alias,username2=alias2." },
    service_accounts: { type: "string", description: "Other service accounts whose activity is ignored." },
    webhook_secret_ref: { type: "string", description: "Name of the secret that holds the X-Gitlab-Token value." },
  },
});

/** Builds the GitLab client of one integration (or user token). */
export function clientFor(t: ForgeTarget, fetchFn?: typeof fetch): GitLabClient {
  const tokenType = t.userToken ? t.config["user_token_type"] : t.config["token_type"];
  return new GitLabClient({
    ...(t.config["base_url"] ? { baseUrl: t.config["base_url"] } : {}),
    ...(t.config["web_url"] ? { webUrl: t.config["web_url"] } : {}),
    token: t.token,
    ...(tokenType ? { tokenType } : {}),
    ...(fetchFn ? { fetch: fetchFn } : {}),
  });
}

export class Plugin extends PluginWorker<Env> implements ForgePlugin, IngressPlugin {
  async info(): Promise<PluginInfo> {
    return pluginInfo(
      {
        name: "gitlab",
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

  commentIssue(t: ForgeTarget, owner: string, repo: string, number: number, body: string): Promise<void> {
    return rpc(() => clientFor(t).commentIssue(owner, repo, number, body));
  }

  commentWorkItem(_t: ForgeTarget, _project: string, _id: number, _body: string): Promise<void> {
    return rpc(() => {
      throw notSupported("gitlab: comment on a work item");
    });
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

  setWorkItemState(_t: ForgeTarget, _project: string, _id: number, _state: string): Promise<void> {
    return rpc(() => {
      throw notSupported("gitlab: set the state of a work item");
    });
  }

  attachPullRequestToWorkItem(_t: ForgeTarget, _project: string, _id: number, _pr: PullRequestRef): Promise<void> {
    return rpc(() => {
      throw notSupported("gitlab: attach a pull request to a work item");
    });
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
    return new Response("openfactory-plugin-gitlab: call this Worker through a service binding", { status: 404 });
  },
};

