// Frozen contract (C0). Plugin Worker RPC: replaces proto/openfactory/plugin/{v1,v2}.
// Sources: appendix 05 §5.5 (shape) and 04 §8 (method signatures).
//
// A plugin is a Worker. The control plane binds it as a service binding
// (`env.PLUGIN_GITLAB`) and calls it with typed JS RPC (`WorkerEntrypoint`).
// Rules:
// - Payloads are plain JSON-shaped objects; Request/Response/ReadableStream are allowed.
// - At most 32 MiB per call; stream larger bodies.
// - Credentials are resolved by the control plane and passed per call in
//   ForgeTarget.token. Plugin Workers hold no team secrets.
// - Plugin settings are the plugin Worker's own vars and secrets.
// - An optional method that a plugin does not support throws an Error whose
//   message starts with "operation_not_supported" (core ErrOperationNotSupported).

import type { SandboxProvider } from "@openfactory/sandbox";

/** Bump on a breaking change; update plugins/*. */
export const PLUGIN_API_VERSION = 1;

export type PluginRole = "forge" | "responder" | "ingress" | "sandbox";

export interface PluginInfo {
  /** [a-z0-9_-]+ */
  name: string;
  version: string;
  apiVersion: number;
  roles: PluginRole[];
  /** Forge provider names served, for example ["gitlab"]. */
  forgeProviders?: string[];
  responderProviders?: string[];
  integrationProviders?: string[];
  /** JSON Schema text of the plugin configuration. */
  configSchema?: string;
}

// ---------- Common types (common.proto) ----------

export interface Actor {
  provider: string;
  externalId: string;
  handle?: string;
  isBot?: boolean;
  userId?: string;
}

export interface SourceRef {
  provider: string;
  kind: string;
  url?: string;
  repo?: string;
  number?: number;
  channel?: string;
  threadId?: string;
  externalId?: string;
  title?: string;
  threadRef?: string;
}

export interface PluginEvent {
  id?: string;
  teamId?: string;
  factoryId?: string;
  provider: string;
  type: string;
  deliveryId?: string;
  actor?: Actor;
  source?: SourceRef;
  attributes?: Record<string, unknown>;
  payload?: unknown;
  text?: string;
  /** RFC 3339. */
  receivedAt?: string;
  actionKey?: string;
}

export interface RepoRef {
  codeForge: string;
  owner: string;
  name: string;
}

export interface PullRequestRef {
  forge: string;
  repo: string;
  number: number;
  url?: string;
  branch?: string;
  state?: "open" | "draft" | "merged" | "closed";
}

export interface PullRequestInput {
  head: string;
  base: string;
  title: string;
  body: string;
  draft: boolean;
}

export interface CommitStatus {
  context: string;
  state: "pending" | "success" | "failure" | "error";
  description?: string;
  targetUrl?: string;
}

export interface IntegrationRecord {
  id: string;
  teamId: string;
  provider: string;
  name: string;
  config: Record<string, string>;
  secretRef?: string;
  factoryId?: string;
}

export type IssueState = "in_progress" | "in_review" | "done";

export interface OutboundMessage {
  workItemId: string;
  source: SourceRef;
  text: string;
  kind: "update" | "question" | "approval_request" | "handoff" | "error" | "issue_state" | "attach_pull_request";
  options?: string[];
  inboxId?: string;
  factoryId?: string;
  threadRef?: string;
  issueState?: IssueState;
  pullRequest?: PullRequestRef;
}

// ---------- Host API (host.proto), passed to calls that need callbacks ----------

/**
 * Implemented by the control plane as an RpcTarget and passed per call. It is
 * scoped to the team of the call; getSecret allows only allow-listed names.
 */
export interface HostApi {
  log(level: "debug" | "info" | "warn" | "error", message: string, attrs?: Record<string, string>): Promise<void>;
  getSecret(factoryId: string, name: string): Promise<string>;
  /** Replaces {{NAME}} references. */
  expandSecrets(factoryId: string, text: string): Promise<string>;
  listTeams(): Promise<Array<{ id: string; name: string }>>;
  listIntegrations(provider?: string): Promise<IntegrationRecord[]>;
  listFactories(): Promise<Array<{ id: string; teamId: string; alias: string; name: string }>>;
  getFactory(factoryId: string): Promise<{ id: string; teamId: string; alias: string; name: string }>;
  getFactoryRepos(factoryId: string): Promise<RepoRef[]>;
}

// ---------- Plugin base ----------

export interface PluginBase {
  info(): Promise<PluginInfo>;
  /** Optional extra configuration; plugin Worker vars are preferred. */
  configure(cfg: Record<string, string>): Promise<void>;
  ping(): Promise<void>;
}

// ---------- Forge (forge.proto, 20 RPCs) ----------

/** The forge account a call acts as. The token is resolved by the control plane. */
export interface ForgeTarget {
  provider: string;
  integrationId: string;
  teamId: string;
  config: Record<string, string>;
  token: string;
  userToken: boolean;
}

export interface ReviewComment {
  path: string;
  line: number;
  side?: "RIGHT" | "LEFT";
  body: string;
}

export interface ThreadComment {
  id: string;
  author: string;
  body: string;
  path?: string;
  line?: number;
  createdAt: string;
}

export interface ReviewThread {
  ref: string;
  path?: string;
  line?: number;
  resolved: boolean;
  comments: ThreadComment[];
}

export interface FileDiff {
  path: string;
  patch: string;
  truncated: boolean;
}

export interface ReviewInput {
  event?: "comment" | "approve" | "request_changes";
  body: string;
  comments?: ReviewComment[];
}

export interface ForgePlugin extends PluginBase {
  /** Display name of the forge, for example GITLAB. */
  name(t: ForgeTarget): Promise<string>;
  cloneUrl(t: ForgeTarget, owner: string, repo: string): Promise<string>;
  defaultBranch(t: ForgeTarget, owner: string, repo: string): Promise<string>;
  createPullRequest(t: ForgeTarget, owner: string, repo: string, pr: PullRequestInput): Promise<PullRequestRef>;
  getPullRequest(t: ForgeTarget, owner: string, repo: string, number: number): Promise<PullRequestRef>;
  comment(t: ForgeTarget, owner: string, repo: string, number: number, body: string): Promise<void>;
  commentIssue(t: ForgeTarget, owner: string, repo: string, number: number, body: string): Promise<void>;
  commentWorkItem(t: ForgeTarget, project: string, id: number, body: string): Promise<void>;
  setCommitStatus(t: ForgeTarget, owner: string, repo: string, sha: string, status: CommitStatus): Promise<void>;
  getFiles(
    t: ForgeTarget,
    owner: string,
    repo: string,
    ref: string,
    path: string,
  ): Promise<{ files: Record<string, string>; commitSha: string }>;
  replyInThread(
    t: ForgeTarget,
    owner: string,
    repo: string,
    number: number,
    threadRef: string,
    body: string,
  ): Promise<void>;
  resolveThread(t: ForgeTarget, owner: string, repo: string, number: number, threadRef: string): Promise<void>;
  submitReview(t: ForgeTarget, owner: string, repo: string, number: number, review: ReviewInput): Promise<void>;
  markReady(t: ForgeTarget, owner: string, repo: string, number: number): Promise<void>;
  setWorkItemState(t: ForgeTarget, project: string, id: number, state: string): Promise<void>;
  attachPullRequestToWorkItem(t: ForgeTarget, project: string, id: number, pr: PullRequestRef): Promise<void>;
  requestReviewers(t: ForgeTarget, owner: string, repo: string, number: number, logins: string[]): Promise<void>;
  login(t: ForgeTarget): Promise<string>;
  getReviewThread(
    t: ForgeTarget,
    owner: string,
    repo: string,
    number: number,
    threadRef: string,
  ): Promise<ReviewThread>;
  getFileDiff(t: ForgeTarget, owner: string, repo: string, number: number, path: string): Promise<FileDiff>;
}

// ---------- Responder (responder.proto) ----------

export interface ResponderPlugin extends PluginBase {
  reply(provider: string, teamId: string, message: OutboundMessage): Promise<void>;
  setIssueState(provider: string, teamId: string, issue: SourceRef, state: IssueState): Promise<void>;
  attachPullRequest(provider: string, teamId: string, issue: SourceRef, pr: PullRequestRef): Promise<void>;
}

// ---------- Ingress (ingress.proto) ----------

export interface WebhookResult {
  /** Sent back to the webhook sender. */
  status: number;
  headers?: Record<string, string>;
  body?: string;
  /** Delivered to IngressDO. */
  events: PluginEvent[];
}

export interface IngressPlugin extends PluginBase {
  /** The control plane forwards the raw Request of POST /plugins/{name}/webhook. */
  handleWebhook(req: Request, host: HostApi): Promise<WebhookResult>;
}

// ---------- Sandbox (replaces v2 sandbox.proto) ----------

export interface SandboxPlugin extends PluginBase, Omit<SandboxProvider, "id"> {}
