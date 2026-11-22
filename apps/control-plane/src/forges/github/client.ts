// GitHub REST client implementing Forge: GitHub.com and GitHub Enterprise
// Server. GitHubClient is a thin facade: auth.ts picks the credential
// (personal token or GitHub App installation), and checks.ts, content.ts and
// reviews.ts hold the operations of each area.
// Port of internal/forge/github/{client,review,reviewcontext,reviewers}.go.
import {
  ApiClient,
  OfError,
  type FetchFn,
  type FileDiff,
  type PullRequestRef,
  type PullRequestState,
  type ReviewThread,
} from "@openfactory/core";
import type { CheckRunInput } from "../checkrun";
import type { CommitStatus, Forge, PullRequestInput, ReviewInput } from "../forge";
import { AppInstallationAuth, PatAuth, type GitHubAuthStrategy, type TokenCache } from "./auth";
import { GitHubChecks, type GitHubCheckRun } from "./checks";
import { defaultBranch, getFiles } from "./content";
import * as reviews from "./reviews";
import { repoPath, wrap, type GitHubTransport } from "./rest";

export { parseInstallationId, resetGitHubCaches, TOKEN_MIN_LIFE_MS, type TokenCache } from "./auth";
export { checkRunBody, type GitHubCheckRun } from "./checks";
export { MAX_FILE_BYTES, MAX_TREE_FILES } from "./content";

export const FORGE_NAME = "GITHUB";
export const DEFAULT_BASE_URL = "https://api.github.com";

export interface GitHubConfig {
  /** REST root: https://api.github.com or https://HOST/api/v3. */
  baseURL?: string;
  /** Web and git root (derived from baseURL when empty). */
  webURL?: string;
  /** Personal access token (when no App is configured). */
  token?: string;
  appId?: string;
  /** PEM, PKCS#1 or PKCS#8. */
  privateKey?: string;
  /** Pins the installation; 0 discovers it per repository owner. */
  installationId?: number;
  fetch?: FetchFn;
  now?: () => number;
  /** Called when GitHub refuses the credential itself (401, 403, 404 on token calls). */
  onAuthFailure?: (err: unknown) => void;
  tokenCache?: TokenCache;
}

export interface GitHubPullRequest {
  number: number;
  html_url: string;
  state: string;
  draft?: boolean;
  merged?: boolean;
  title?: string;
  body?: string | null;
  user?: { id?: number; login?: string; type?: string };
  labels?: { name: string }[];
  head?: { ref?: string; sha?: string };
  base?: { ref?: string; sha?: string };
  merged_at?: string | null;
  node_id?: string;
}

export function deriveWebURL(base: string): string {
  try {
    const u = new URL(base);
    if (u.host === "api.github.com") return "https://github.com";
    u.pathname = u.pathname.replace(/\/+$/, "").replace(/\/api\/v3$/, "");
    return u.toString().replace(/\/+$/, "");
  } catch {
    return "https://github.com";
  }
}

function prRef(p: GitHubPullRequest, owner: string, repo: string): PullRequestRef {
  // GitHub reports "open" or "closed"; any other value is not known ("").
  let state: PullRequestState = p.state === "open" || p.state === "closed" ? p.state : "";
  if (p.merged || (p.merged_at ?? "") !== "") state = "merged";
  else if (p.state === "open" && p.draft) state = "draft";
  return {
    forge: FORGE_NAME,
    repo: `${owner}/${repo}`,
    number: p.number,
    url: p.html_url,
    branch: p.head?.ref ?? "",
    state,
  };
}

/** What a scoped client shares with the client it comes from. */
interface Shared {
  api: ApiClient;
  auth: GitHubAuthStrategy;
}

export class GitHubClient implements Forge, GitHubTransport {
  readonly api: ApiClient;
  private readonly cfg: GitHubConfig;
  private readonly auth: GitHubAuthStrategy;
  private readonly checks: GitHubChecks;
  private readonly web: string;
  readonly now: () => number;
  /** lower-case owner -> repository names; undefined = no limit. */
  private readonly scope: Map<string, string[]> | undefined;
  private readonly scopedClients = new Map<string, GitHubClient>();

  constructor(cfg: GitHubConfig, scope?: Map<string, string[]>, shared?: Shared) {
    const base = (cfg.baseURL || DEFAULT_BASE_URL).replace(/\/+$/, "");
    this.cfg = { ...cfg, baseURL: base };
    this.now = cfg.now ?? Date.now;
    this.scope = scope;
    this.web = (cfg.webURL ?? "").replace(/\/+$/, "") || deriveWebURL(base);
    this.api =
      shared?.api ??
      new ApiClient({
        baseUrl: base,
        userAgent: "openfactory-github",
        headers: { accept: "application/vnd.github+json", "x-github-api-version": "2022-11-28" },
        authorize: (h, { auth }) => {
          if (auth) h.set("authorization", auth);
        },
        ...(cfg.fetch ? { fetch: cfg.fetch } : {}),
      });
    this.auth = shared?.auth ?? this.newAuth();
    this.checks = new GitHubChecks(this);
  }

  private newAuth(): GitHubAuthStrategy {
    const cfg = this.cfg;
    if (cfg.appId || cfg.privateKey) {
      if (!cfg.appId || !cfg.privateKey)
        throw new Error("github: app authentication requires both app_id and private key");
      return new AppInstallationAuth({
        api: this.api,
        baseURL: cfg.baseURL!,
        appId: cfg.appId,
        privateKey: cfg.privateKey,
        now: this.now,
        ...(cfg.installationId ? { installationId: cfg.installationId } : {}),
        ...(cfg.onAuthFailure ? { onAuthFailure: cfg.onAuthFailure } : {}),
        ...(cfg.tokenCache ? { tokenCache: cfg.tokenCache } : {}),
      });
    }
    if (!cfg.token) throw new Error("github: a token or app credentials are required");
    return new PatAuth(cfg.token, cfg.onAuthFailure);
  }

  /** Builds a client and checks the App key. */
  static async create(cfg: GitHubConfig): Promise<GitHubClient> {
    const c = new GitHubClient(cfg);
    if (c.auth instanceof AppInstallationAuth) {
      try {
        await c.auth.appKey();
      } catch (e) {
        throw new Error(`github: ${(e as Error).message}`, { cause: e });
      }
    }
    return c;
  }

  name(): string {
    return FORGE_NAME;
  }

  isApp(): boolean {
    return this.auth.kind === "app";
  }

  appId(): number {
    return Number(this.cfg.appId ?? "0") || 0;
  }

  webURL(): string {
    return this.web;
  }

  /** A short-lived JWT that authenticates as the App itself. */
  appJWT(): Promise<string> {
    if (!(this.auth instanceof AppInstallationAuth))
      return Promise.reject(new Error("github: not configured as an app"));
    return this.auth.appJWT();
  }

  /**
   * A client with the same credentials limited to repos ("owner/name"). For
   * an App, each installation token is requested with the repositories of its
   * owner. An empty list returns this client.
   */
  scoped(repos: readonly string[]): GitHubClient {
    if (repos.length === 0) return this;
    const key = [...new Set(repos.map((r) => r.toLowerCase()))].sort().join(",");
    const hit = this.scopedClients.get(key);
    if (hit) return hit;
    const scope = new Map<string, string[]>();
    for (const full of repos) {
      const i = full.indexOf("/");
      if (i <= 0 || i === full.length - 1) continue;
      const k = full.slice(0, i).toLowerCase();
      scope.set(k, [...(scope.get(k) ?? []), full.slice(i + 1)]);
    }
    const n = new GitHubClient(this.cfg, scope, { api: this.api, auth: this.auth });
    this.scopedClients.set(key, n);
    return n;
  }

  private inScope(owner: string, repo: string): boolean {
    if (!this.scope) return true;
    const names = this.scope.get(owner.toLowerCase());
    if (!names) return false;
    if (repo === "") return true;
    return names.some((n) => n.toLowerCase() === repo.toLowerCase());
  }

  /**
   * A credential for API and git operations on owner/repo: the token, or an
   * installation token for an App. A scoped client refuses a repository
   * outside its scope (not_authorized).
   */
  async token(owner: string, repo: string): Promise<string> {
    if (!this.inScope(owner, repo)) {
      throw new OfError("not_authorized", `github: ${owner}/${repo} is not a repository of this factory`);
    }
    return this.auth.token(owner, repo, this.scope ? (this.scope.get(owner.toLowerCase()) ?? []) : undefined);
  }

  /** An authenticated API call for owner/repo. */
  async call<T = unknown>(owner: string, repo: string, method: string, path: string, body?: unknown): Promise<T> {
    const tok = await this.token(owner, repo);
    try {
      return await this.api.json<T>(method, path, body as object | undefined, this.auth.scheme + tok);
    } catch (e) {
      this.auth.callFailed(e);
      throw e;
    }
  }

  /** The GraphQL endpoint for the API base URL (GHES: <host>/api/graphql). */
  graphqlPath(): string {
    const base = this.cfg.baseURL ?? "";
    if (base.endsWith("/api/v3")) return base.slice(0, -"/v3".length) + "/graphql";
    return "/graphql";
  }

  async graphql<T>(owner: string, repo: string, query: string, variables: Record<string, unknown>): Promise<T> {
    const resp = await this.call<{ data?: T; errors?: { message: string }[] }>(
      owner,
      repo,
      "POST",
      this.graphqlPath(),
      { query, variables },
    );
    if (resp?.errors && resp.errors.length > 0)
      throw new Error("github graphql: " + resp.errors.map((e) => e.message).join("; "));
    return resp?.data as T;
  }

  async cloneURL(owner: string, repo: string): Promise<string> {
    const tok = await this.token(owner, repo);
    const u = new URL(this.web);
    u.username = "x-access-token";
    u.password = tok;
    u.pathname = u.pathname.replace(/\/+$/, "") + "/" + owner + "/" + repo + ".git";
    return u.toString();
  }

  /** The login of the account of a personal token (not available for an App). */
  async login(): Promise<string> {
    if (this.isApp()) throw new Error("github: an app credential has no user login");
    let out: { login?: string } | undefined;
    try {
      out = await this.call("", "", "GET", "/user");
    } catch (e) {
      throw wrap("github: read the user of the token", e);
    }
    const l = (out?.login ?? "").trim();
    if (l === "") throw new Error("github: the token has no user login");
    return l;
  }

  // ---------- pull requests ----------

  defaultBranch(owner: string, repo: string): Promise<string> {
    return defaultBranch(this, owner, repo);
  }

  async createPullRequest(input: PullRequestInput): Promise<PullRequestRef> {
    const body: Record<string, unknown> = {
      title: input.title,
      head: input.head,
      base: input.base,
      body: input.body,
      draft: !!input.draft,
    };
    if (input.base === "") body["base"] = await this.defaultBranch(input.owner, input.repo);
    try {
      const pr = await this.call<GitHubPullRequest>(
        input.owner,
        input.repo,
        "POST",
        repoPath(input.owner, input.repo) + "/pulls",
        body,
      );
      return prRef(pr, input.owner, input.repo);
    } catch (e) {
      throw wrap("github: create pull request", e);
    }
  }

  getPullRequestDetails(owner: string, repo: string, number: number): Promise<GitHubPullRequest> {
    return this.call<GitHubPullRequest>(owner, repo, "GET", `${repoPath(owner, repo)}/pulls/${number}`);
  }

  async getPullRequest(owner: string, repo: string, number: number): Promise<PullRequestRef> {
    return prRef(await this.getPullRequestDetails(owner, repo, number), owner, repo);
  }

  async comment(owner: string, repo: string, number: number, body: string): Promise<void> {
    try {
      await this.call(owner, repo, "POST", `${repoPath(owner, repo)}/issues/${number}/comments`, { body });
    } catch (e) {
      throw wrap(`github: comment on ${owner}/${repo}#${number}`, e);
    }
  }

  commentIssue(owner: string, repo: string, number: number, body: string): Promise<void> {
    return this.comment(owner, repo, number, body);
  }

  async addLabels(owner: string, repo: string, number: number, ...labels: string[]): Promise<void> {
    await this.call(owner, repo, "POST", `${repoPath(owner, repo)}/issues/${number}/labels`, { labels });
  }

  /** Paths changed by a pull request (up to 3000, GitHub's limit). */
  async listPullRequestFiles(owner: string, repo: string, number: number): Promise<string[]> {
    const out: string[] = [];
    for (let page = 1; page <= 30; page++) {
      const files =
        (await this.call<{ filename: string; previous_filename?: string }[]>(
          owner,
          repo,
          "GET",
          `${repoPath(owner, repo)}/pulls/${number}/files?per_page=100&page=${page}`,
        )) ?? [];
      for (const f of files) {
        out.push(f.filename);
        if (f.previous_filename) out.push(f.previous_filename);
      }
      if (files.length < 100) break;
    }
    return out;
  }

  // ---------- checks (checks.ts) ----------

  setCommitStatus(owner: string, repo: string, sha: string, st: CommitStatus): Promise<void> {
    return this.checks.setCommitStatus(owner, repo, sha, st);
  }

  createCheckRun(owner: string, repo: string, cr: GitHubCheckRun): Promise<number> {
    return this.checks.createCheckRun(owner, repo, cr);
  }

  updateCheckRun(owner: string, repo: string, id: number, cr: GitHubCheckRun): Promise<void> {
    return this.checks.updateCheckRun(owner, repo, id, cr);
  }

  publishCheckRun(owner: string, repo: string, cr: CheckRunInput): Promise<void> {
    return this.checks.publishCheckRun(owner, repo, cr);
  }

  // ---------- content (content.ts) ----------

  getFiles(
    owner: string,
    repo: string,
    ref: string,
    path: string,
  ): Promise<{ files: Record<string, string>; sha: string }> {
    return getFiles(this, owner, repo, ref, path);
  }

  // ---------- review operations (reviews.ts) ----------

  replyInThread(owner: string, repo: string, number: number, threadRef: string, body: string): Promise<void> {
    return reviews.replyInThread(this, owner, repo, number, threadRef, body);
  }

  resolveThread(owner: string, repo: string, number: number, threadRef: string): Promise<void> {
    return reviews.resolveThread(this, owner, repo, number, threadRef);
  }

  submitReview(input: ReviewInput): Promise<void> {
    return reviews.submitReview(this, input);
  }

  markReady(owner: string, repo: string, number: number): Promise<void> {
    return reviews.markReady(this, owner, repo, number);
  }

  requestReviewers(owner: string, repo: string, number: number, logins: string[]): Promise<void> {
    return reviews.requestReviewers(this, owner, repo, number, logins);
  }

  reviewThread(owner: string, repo: string, number: number, threadRef: string): Promise<ReviewThread> {
    return reviews.reviewThread(this, owner, repo, number, threadRef);
  }

  fileDiff(owner: string, repo: string, number: number, path: string): Promise<FileDiff> {
    return reviews.fileDiff(this, owner, repo, number, path);
  }
}
