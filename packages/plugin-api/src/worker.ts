// Base class of a plugin Worker. Import from "@openfactory/plugin-api/worker":
// it needs the Workers runtime ("cloudflare:workers").
//
// A plugin Worker exports `class Plugin extends PluginWorker`. Every public
// method of the class is an RPC method, so helpers must be module functions or
// #private members.

import { WorkerEntrypoint } from "cloudflare:workers";
import { notSupported, rpc } from "./errors";
import { PLUGIN_API_VERSION } from "./types";
import type {
  CommitStatus,
  FileDiff,
  ForgePlugin,
  ForgeTarget,
  PluginBase,
  PluginInfo,
  PullRequestInput,
  PullRequestRef,
  ReviewInput,
  ReviewThread,
} from "./types";

export type PluginInfoInput = Omit<PluginInfo, "apiVersion" | "version"> & { version?: string };

export abstract class PluginWorker<E = unknown> extends WorkerEntrypoint<E> implements PluginBase {
  abstract info(): Promise<PluginInfo>;

  /** Plugin Worker vars and secrets are the settings; the default ignores the call. */
  async configure(_cfg: Record<string, string>): Promise<void> {}

  async ping(): Promise<void> {}
}

/** Fills the API version and the default version string. */
export function pluginInfo(input: PluginInfoInput, version?: string): PluginInfo {
  return { ...input, version: input.version ?? version ?? "0.0.0-dev", apiVersion: PLUGIN_API_VERSION };
}

/**
 * The forge client of one ForgeTarget, as ForgePluginWorker calls it. Leave
 * out an optional method that the forge does not have: the RPC method then
 * throws operation_not_supported.
 */
export interface ForgeClient {
  cloneUrl(owner: string, repo: string): string | Promise<string>;
  defaultBranch(owner: string, repo: string): Promise<string>;
  createPullRequest(owner: string, repo: string, pr: PullRequestInput): Promise<PullRequestRef>;
  getPullRequest(owner: string, repo: string, number: number): Promise<PullRequestRef>;
  comment(owner: string, repo: string, number: number, body: string): Promise<void>;
  commentIssue?(owner: string, repo: string, number: number, body: string): Promise<void>;
  commentWorkItem?(project: string, id: number, body: string): Promise<void>;
  setCommitStatus(owner: string, repo: string, sha: string, status: CommitStatus): Promise<void>;
  getFiles(
    owner: string,
    repo: string,
    ref: string,
    path: string,
  ): Promise<{ files: Record<string, string>; commitSha: string }>;
  replyInThread(owner: string, repo: string, number: number, threadRef: string, body: string): Promise<void>;
  resolveThread(owner: string, repo: string, number: number, threadRef: string): Promise<void>;
  submitReview(owner: string, repo: string, number: number, review: ReviewInput): Promise<void>;
  markReady(owner: string, repo: string, number: number): Promise<void>;
  setWorkItemState?(project: string, id: number, state: string): Promise<void>;
  attachPullRequestToWorkItem?(project: string, id: number, pr: PullRequestRef): Promise<void>;
  requestReviewers(owner: string, repo: string, number: number, logins: string[]): Promise<void>;
  login(): Promise<string>;
  reviewThread(owner: string, repo: string, number: number, threadRef: string): Promise<ReviewThread>;
  fileDiff(owner: string, repo: string, number: number, path: string): Promise<FileDiff>;
}

/** The ForgePlugin methods that a forge may not have. */
export type OptionalForgeMethod =
  "commentIssue" | "commentWorkItem" | "setWorkItemState" | "attachPullRequestToWorkItem";

const UNSUPPORTED_TEXT: Record<OptionalForgeMethod, string> = {
  commentIssue: "issue comments",
  commentWorkItem: "comment on a work item",
  setWorkItemState: "set the state of a work item",
  attachPullRequestToWorkItem: "attach a pull request to a work item",
};

export interface ForgeSpec {
  /** The display name of the forge, for example GITLAB. */
  name: string;
  /** The plugin name at the start of operation_not_supported messages, for example gitlab. */
  label: string;
  /** Builds the client of one call. */
  client(t: ForgeTarget): ForgeClient;
  /** Optional methods that the forge does not have. They throw operation_not_supported before a client is built. */
  unsupported?: readonly OptionalForgeMethod[];
}

/** The client of t for an optional method. Throws operation_not_supported when the forge does not have the method. */
function optionalClient(spec: ForgeSpec, method: OptionalForgeMethod, t: ForgeTarget): ForgeClient {
  const err = (): Error => notSupported(`${spec.label}: ${UNSUPPORTED_TEXT[method]}`);
  if (spec.unsupported?.includes(method)) throw err();
  const c = spec.client(t);
  if (typeof c[method] !== "function") throw err();
  return c;
}

/**
 * Base class of a forge plugin Worker. It implements every ForgePlugin method
 * as one call of the forge client, with errors mapped by rpc. A subclass sets
 * `forge` as a class field (an instance field is not an RPC method) and adds
 * info() and its other roles.
 */
export abstract class ForgePluginWorker<E = unknown> extends PluginWorker<E> implements ForgePlugin {
  protected abstract readonly forge: ForgeSpec;

  async name(_t: ForgeTarget): Promise<string> {
    return this.forge.name;
  }

  cloneUrl(t: ForgeTarget, owner: string, repo: string): Promise<string> {
    return rpc(() => this.forge.client(t).cloneUrl(owner, repo));
  }

  defaultBranch(t: ForgeTarget, owner: string, repo: string): Promise<string> {
    return rpc(() => this.forge.client(t).defaultBranch(owner, repo));
  }

  createPullRequest(t: ForgeTarget, owner: string, repo: string, pr: PullRequestInput): Promise<PullRequestRef> {
    return rpc(() => this.forge.client(t).createPullRequest(owner, repo, pr));
  }

  getPullRequest(t: ForgeTarget, owner: string, repo: string, number: number): Promise<PullRequestRef> {
    return rpc(() => this.forge.client(t).getPullRequest(owner, repo, number));
  }

  comment(t: ForgeTarget, owner: string, repo: string, number: number, body: string): Promise<void> {
    return rpc(() => this.forge.client(t).comment(owner, repo, number, body));
  }

  /** Without client.commentIssue, the host falls back to comment. */
  commentIssue(t: ForgeTarget, owner: string, repo: string, number: number, body: string): Promise<void> {
    return rpc(() => optionalClient(this.forge, "commentIssue", t).commentIssue!(owner, repo, number, body));
  }

  commentWorkItem(t: ForgeTarget, project: string, id: number, body: string): Promise<void> {
    return rpc(() => optionalClient(this.forge, "commentWorkItem", t).commentWorkItem!(project, id, body));
  }

  setCommitStatus(t: ForgeTarget, owner: string, repo: string, sha: string, status: CommitStatus): Promise<void> {
    return rpc(() => this.forge.client(t).setCommitStatus(owner, repo, sha, status));
  }

  getFiles(
    t: ForgeTarget,
    owner: string,
    repo: string,
    ref: string,
    path: string,
  ): Promise<{ files: Record<string, string>; commitSha: string }> {
    return rpc(() => this.forge.client(t).getFiles(owner, repo, ref, path));
  }

  replyInThread(
    t: ForgeTarget,
    owner: string,
    repo: string,
    number: number,
    threadRef: string,
    body: string,
  ): Promise<void> {
    return rpc(() => this.forge.client(t).replyInThread(owner, repo, number, threadRef, body));
  }

  resolveThread(t: ForgeTarget, owner: string, repo: string, number: number, threadRef: string): Promise<void> {
    return rpc(() => this.forge.client(t).resolveThread(owner, repo, number, threadRef));
  }

  submitReview(t: ForgeTarget, owner: string, repo: string, number: number, review: ReviewInput): Promise<void> {
    return rpc(() => this.forge.client(t).submitReview(owner, repo, number, review));
  }

  markReady(t: ForgeTarget, owner: string, repo: string, number: number): Promise<void> {
    return rpc(() => this.forge.client(t).markReady(owner, repo, number));
  }

  setWorkItemState(t: ForgeTarget, project: string, id: number, state: string): Promise<void> {
    return rpc(() => optionalClient(this.forge, "setWorkItemState", t).setWorkItemState!(project, id, state));
  }

  attachPullRequestToWorkItem(t: ForgeTarget, project: string, id: number, pr: PullRequestRef): Promise<void> {
    return rpc(() =>
      optionalClient(this.forge, "attachPullRequestToWorkItem", t).attachPullRequestToWorkItem!(project, id, pr),
    );
  }

  requestReviewers(t: ForgeTarget, owner: string, repo: string, number: number, logins: string[]): Promise<void> {
    return rpc(() => this.forge.client(t).requestReviewers(owner, repo, number, logins));
  }

  login(t: ForgeTarget): Promise<string> {
    return rpc(() => this.forge.client(t).login());
  }

  getReviewThread(
    t: ForgeTarget,
    owner: string,
    repo: string,
    number: number,
    threadRef: string,
  ): Promise<ReviewThread> {
    return rpc(() => this.forge.client(t).reviewThread(owner, repo, number, threadRef));
  }

  getFileDiff(t: ForgeTarget, owner: string, repo: string, number: number, path: string): Promise<FileDiff> {
    return rpc(() => this.forge.client(t).fileDiff(owner, repo, number, path));
  }
}
