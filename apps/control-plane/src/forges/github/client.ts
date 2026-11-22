// GitHub REST client implementing Forge: GitHub.com and GitHub Enterprise
// Server, GitHub App and personal-token authentication, check runs, the git
// trees and blobs content API for definitions, and the review operations.
// Port of internal/forge/github/{client,review,reviewcontext,reviewers}.go.
// App JWTs are RS256 with WebCrypto; installation tokens are cached per
// isolate (and optionally in a shared TokenCache) until 10 minutes before
// they expire.
import {
  OfError,
  pathEscape,
  queryEscape,
  type FileDiff,
  type PullRequestRef,
  type ReviewThread,
  type ThreadComment,
} from "@openfactory/core";
import type { CheckRunInput } from "../checkrun";
import type { CommitStatus, Forge, PullRequestInput, ReviewInput } from "../forge";
import { ApiClient, statusCode, type FetchFn } from "@openfactory/core";
import { parseRSAPrivateKey, signRS256 } from "../util/jwt";
import { truncate } from "@openfactory/core";
import { fromB64 } from "../../lib/bytes";

export const FORGE_NAME = "GITHUB";
export const DEFAULT_BASE_URL = "https://api.github.com";
export const MAX_TREE_FILES = 2000;
export const MAX_FILE_BYTES = 1 << 20;
/** The least life a cached installation token must have left. */
export const TOKEN_MIN_LIFE_MS = 10 * 60_000;
const MAX_REVIEW_PAGES = 30;
const MAX_OPEN_CHECK_RUNS = 256;

/** Shared installation-token cache (for example TeamDO storage). */
export interface TokenCache {
  get(key: string): Promise<{ token: string; expiresMs: number } | null> | { token: string; expiresMs: number } | null;
  set(key: string, token: string, expiresMs: number): Promise<void> | void;
}

const isolateTokens = new Map<string, { token: string; expiresMs: number }>();
const isolateInstallations = new Map<string, number>();

/** Clears the isolate caches (tests). */
export function resetGitHubCaches(): void {
  isolateTokens.clear();
  isolateInstallations.clear();
}

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

export function parseInstallationId(s: string | undefined): number {
  const t = (s ?? "").trim();
  if (t === "") return 0;
  if (!/^[+-]?\d+$/.test(t)) throw new Error(`strconv.ParseInt: parsing "${t}": invalid syntax`);
  return Number(t);
}

function repoPath(owner: string, repo: string): string {
  return "/repos/" + pathEscape(owner) + "/" + pathEscape(repo);
}

function prRef(p: GitHubPullRequest, owner: string, repo: string): PullRequestRef {
  let state = p.state;
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

function commentId(threadRef: string): number {
  const t = threadRef.trim();
  const id = /^\d+$/.test(t) ? Number(t) : NaN;
  if (!(id > 0))
    throw new OfError("invalid_request", `github: thread reference "${threadRef}" is not a review comment ID`);
  return id;
}

function wrap(msg: string, e: unknown): Error {
  if (e instanceof OfError)
    return new OfError(e.code, `${msg}: ${e.detail}`, { cause: e, ...(e.reason ? { reason: e.reason } : {}) });
  return new Error(`${msg}: ${(e as Error).message}`, { cause: e });
}

export class GitHubClient implements Forge {
  readonly api: ApiClient;
  private readonly cfg: GitHubConfig;
  private readonly keyPem: string | undefined;
  private key: Promise<CryptoKey> | undefined;
  private readonly web: string;
  private readonly now: () => number;
  /** lower-case owner -> repository names; undefined = no limit. */
  private readonly scope: Map<string, string[]> | undefined;
  private readonly scopedClients = new Map<string, GitHubClient>();
  private readonly openRuns = new Map<string, number>();

  constructor(cfg: GitHubConfig, scope?: Map<string, string[]>) {
    const base = (cfg.baseURL || DEFAULT_BASE_URL).replace(/\/+$/, "");
    this.cfg = { ...cfg, baseURL: base };
    this.now = cfg.now ?? Date.now;
    this.scope = scope;
    if (cfg.appId || cfg.privateKey) {
      if (!cfg.appId || !cfg.privateKey)
        throw new Error("github: app authentication requires both app_id and private key");
      this.keyPem = cfg.privateKey;
    } else if (!cfg.token) {
      throw new Error("github: a token or app credentials are required");
    }
    this.web = (cfg.webURL ?? "").replace(/\/+$/, "") || deriveWebURL(base);
    this.api = new ApiClient({
      baseUrl: base,
      userAgent: "openfactory-github",
      headers: { accept: "application/vnd.github+json", "x-github-api-version": "2022-11-28" },
      authorize: (h, { auth }) => {
        if (auth) h.set("authorization", auth);
      },
      ...(cfg.fetch ? { fetch: cfg.fetch } : {}),
    });
  }

  /** Builds a client and checks the App key. */
  static async create(cfg: GitHubConfig): Promise<GitHubClient> {
    const c = new GitHubClient(cfg);
    if (c.keyPem) {
      try {
        await c.appKey();
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
    return this.keyPem !== undefined;
  }

  webURL(): string {
    return this.web;
  }

  private appKey(): Promise<CryptoKey> {
    if (!this.keyPem) return Promise.reject(new Error("github: not configured as an app"));
    this.key ??= parseRSAPrivateKey(this.keyPem);
    return this.key;
  }

  /** A short-lived JWT that authenticates as the App itself. */
  async appJWT(): Promise<string> {
    const key = await this.appKey();
    const now = Math.floor(this.now() / 1000);
    return signRS256(key, { iat: now - 60, exp: now + 9 * 60, iss: this.cfg.appId });
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
    const n = new GitHubClient(this.cfg, scope);
    n.key = this.key;
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

  private authFailure(err: unknown): void {
    if (!this.cfg.onAuthFailure) return;
    const s = statusCode(err);
    if (s === 401 || s === 403 || s === 404) this.cfg.onAuthFailure(err);
  }

  private cacheKey(inst: number, owner: string): string {
    const scope = this.scope
      ? (this.scope.get(owner.toLowerCase()) ?? [])
          .map((s) => s.toLowerCase())
          .sort()
          .join(",")
      : "*";
    return `${this.cfg.baseURL}|${this.cfg.appId}|${inst}|${scope}`;
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
    if (!this.keyPem) return this.cfg.token!;
    let inst: number;
    try {
      inst = await this.installationId(owner, repo);
    } catch (e) {
      if (statusCode(e) === 401) this.authFailure(e);
      throw e;
    }
    const key = this.cacheKey(inst, owner);
    const fresh = (t: { token: string; expiresMs: number } | null | undefined) =>
      t && this.now() < t.expiresMs - TOKEN_MIN_LIFE_MS ? t.token : null;
    const local = fresh(isolateTokens.get(key));
    if (local) return local;
    if (this.cfg.tokenCache) {
      const shared = await this.cfg.tokenCache.get(key);
      const tok = fresh(shared);
      if (tok && shared) {
        isolateTokens.set(key, shared);
        return tok;
      }
    }
    const jwt = await this.appJWT();
    const body: Record<string, unknown> = {};
    if (this.scope) body["repositories"] = this.scope.get(owner.toLowerCase()) ?? [];
    let resp: { token?: string; expires_at?: string } | undefined;
    try {
      resp = await this.api.json("POST", `/app/installations/${inst}/access_tokens`, body, "Bearer " + jwt);
    } catch (e) {
      this.authFailure(e);
      throw wrap("github: create installation token", e);
    }
    if (!resp?.token) throw new Error("github: empty installation token");
    let expiresMs = resp.expires_at ? Date.parse(resp.expires_at) : NaN;
    if (!Number.isFinite(expiresMs)) expiresMs = this.now() + 55 * 60_000;
    const entry = { token: resp.token, expiresMs };
    isolateTokens.set(key, entry);
    if (this.cfg.tokenCache) await this.cfg.tokenCache.set(key, entry.token, entry.expiresMs);
    return resp.token;
  }

  private async installationId(owner: string, repo: string): Promise<number> {
    if (this.cfg.installationId) return this.cfg.installationId;
    const key = `${this.cfg.baseURL}|${this.cfg.appId}|${owner.toLowerCase()}`;
    const hit = isolateInstallations.get(key);
    if (hit !== undefined) return hit;
    const jwt = await this.appJWT();
    const path =
      repo === "" ? "/users/" + pathEscape(owner) + "/installation" : repoPath(owner, repo) + "/installation";
    let resp: { id?: number } | undefined;
    try {
      resp = await this.api.json("GET", path, undefined, "Bearer " + jwt);
    } catch (e) {
      throw wrap(`github: find app installation for ${owner}`, e);
    }
    const id = resp?.id ?? 0;
    isolateInstallations.set(key, id);
    return id;
  }

  /** An authenticated API call for owner/repo. */
  async call<T = unknown>(owner: string, repo: string, method: string, path: string, body?: unknown): Promise<T> {
    const tok = await this.token(owner, repo);
    const scheme = this.keyPem ? "Bearer " : "token ";
    try {
      return await this.api.json<T>(method, path, body as object | undefined, scheme + tok);
    } catch (e) {
      if (!this.keyPem && statusCode(e) === 401) this.authFailure(e);
      throw e;
    }
  }

  async cloneURL(owner: string, repo: string): Promise<string> {
    const tok = await this.token(owner, repo);
    const u = new URL(this.web);
    u.username = "x-access-token";
    u.password = tok;
    u.pathname = u.pathname.replace(/\/+$/, "") + "/" + owner + "/" + repo + ".git";
    return u.toString();
  }

  async defaultBranch(owner: string, repo: string): Promise<string> {
    const r = await this.call<{ default_branch?: string }>(owner, repo, "GET", repoPath(owner, repo));
    return r?.default_branch ?? "";
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

  /**
   * With an App it publishes a check run (the App holds checks:write); with a
   * token it uses the commit statuses API.
   */
  async setCommitStatus(owner: string, repo: string, sha: string, st: CommitStatus): Promise<void> {
    if (this.isApp()) {
      const cr: GitHubCheckRun = {
        name: st.context,
        headSha: sha,
        detailsUrl: st.targetUrl ?? "",
        title: st.description ?? "",
        summary: st.description ?? "",
      };
      switch (st.state) {
        case "pending":
        case "":
          cr.status = "in_progress";
          break;
        case "success":
          cr.status = "completed";
          cr.conclusion = "success";
          break;
        case "failure":
          cr.status = "completed";
          cr.conclusion = "failure";
          break;
        default:
          cr.status = "completed";
          cr.conclusion = "action_required";
      }
      await this.createCheckRun(owner, repo, cr);
      return;
    }
    const body: Record<string, string> = {
      state: st.state || "pending",
      context: st.context,
      description: truncate(st.description ?? "", 140),
    };
    if (st.targetUrl) body["target_url"] = st.targetUrl;
    try {
      await this.call(owner, repo, "POST", repoPath(owner, repo) + "/statuses/" + pathEscape(sha), body);
    } catch (e) {
      throw wrap("github: set commit status", e);
    }
  }

  async createCheckRun(owner: string, repo: string, cr: GitHubCheckRun): Promise<number> {
    try {
      const r = await this.call<{ id: number }>(
        owner,
        repo,
        "POST",
        repoPath(owner, repo) + "/check-runs",
        checkRunBody(cr, true, this.now()),
      );
      return r?.id ?? 0;
    } catch (e) {
      throw wrap("github: create check run", e);
    }
  }

  async updateCheckRun(owner: string, repo: string, id: number, cr: GitHubCheckRun): Promise<void> {
    try {
      await this.call(
        owner,
        repo,
        "PATCH",
        `${repoPath(owner, repo)}/check-runs/${id}`,
        checkRunBody(cr, false, this.now()),
      );
    } catch (e) {
      throw wrap("github: update check run", e);
    }
  }

  /**
   * Publishes a check run. A completed run updates the run in progress for the
   * same commit and name. A token client sets a commit status instead.
   */
  async publishCheckRun(owner: string, repo: string, cr: CheckRunInput): Promise<void> {
    if (!this.isApp()) {
      const st: CommitStatus = {
        context: cr.name,
        targetUrl: cr.detailsUrl ?? "",
        description: cr.title ?? "",
        state: "",
      };
      if (cr.status !== "completed") st.state = "pending";
      else if (cr.conclusion === "success") st.state = "success";
      else st.state = "failure";
      return this.setCommitStatus(owner, repo, cr.headSha, st);
    }
    const gr: GitHubCheckRun = { ...cr, status: cr.status || "in_progress" };
    const key = `${owner}/${repo}@${cr.headSha}#${cr.name}`.toLowerCase();
    if (gr.status !== "completed") {
      const id = await this.createCheckRun(owner, repo, gr);
      if (this.openRuns.size >= MAX_OPEN_CHECK_RUNS) this.openRuns.clear();
      this.openRuns.set(key, id);
      return;
    }
    let id = this.openRuns.get(key) ?? 0;
    this.openRuns.delete(key);
    if (id === 0) id = await this.findCheckRun(owner, repo, cr.headSha, cr.name);
    if (id !== 0) {
      try {
        await this.updateCheckRun(owner, repo, id, gr);
        return;
      } catch (e) {
        if (statusCode(e) !== 404) throw e;
      }
    }
    await this.createCheckRun(owner, repo, gr);
  }

  private async findCheckRun(owner: string, repo: string, sha: string, name: string): Promise<number> {
    try {
      const r = await this.call<{ check_runs?: { id: number; app?: { id?: number } }[] }>(
        owner,
        repo,
        "GET",
        `${repoPath(owner, repo)}/commits/${pathEscape(sha)}/check-runs?filter=latest&check_name=${queryEscape(name)}`,
      );
      const appId = Number(this.cfg.appId ?? "0") || 0;
      for (const x of r?.check_runs ?? []) if (appId === 0 || x.app?.id === appId) return x.id;
    } catch {
      /* create a new run */
    }
    return 0;
  }

  /** Reads every blob under path at ref with the git trees and blobs APIs. */
  async getFiles(
    owner: string,
    repo: string,
    ref: string,
    path: string,
  ): Promise<{ files: Record<string, string>; sha: string }> {
    const r = ref === "" ? await this.defaultBranch(owner, repo) : ref;
    let commit: { sha: string; commit: { tree: { sha: string } } };
    try {
      commit = await this.call(owner, repo, "GET", repoPath(owner, repo) + "/commits/" + pathEscape(r));
    } catch (e) {
      throw wrap(`github: resolve ${r}`, e);
    }
    let tree: { tree: { path: string; type: string; sha: string; size?: number }[]; truncated?: boolean };
    try {
      tree = await this.call(
        owner,
        repo,
        "GET",
        `${repoPath(owner, repo)}/git/trees/${commit.commit.tree.sha}?recursive=1`,
      );
    } catch (e) {
      throw wrap("github: read tree", e);
    }
    if (tree.truncated) throw new Error("github: repository tree too large (truncated by GitHub)");
    let prefix = path.replace(/^\/+|\/+$/g, "");
    if (prefix !== "") prefix += "/";
    const files: Record<string, string> = {};
    let n = 0;
    for (const e of tree.tree) {
      if (e.type !== "blob" || !e.path.startsWith(prefix)) continue;
      if (n >= MAX_TREE_FILES) throw new Error(`github: more than ${MAX_TREE_FILES} files under "${path}"`);
      if ((e.size ?? 0) > MAX_FILE_BYTES) throw new Error(`github: ${e.path} exceeds ${MAX_FILE_BYTES} bytes`);
      let blob: { content: string; encoding: string };
      try {
        blob = await this.call(owner, repo, "GET", `${repoPath(owner, repo)}/git/blobs/${e.sha}`);
      } catch (err) {
        throw wrap(`github: read ${e.path}`, err);
      }
      let content = blob.content;
      if (blob.encoding === "base64") {
        try {
          content = new TextDecoder().decode(fromB64(blob.content.replace(/\n/g, "")));
        } catch (err) {
          throw new Error(`github: decode ${e.path}: ${(err as Error).message}`, { cause: err });
        }
      }
      files[e.path.slice(prefix.length)] = content;
      n++;
    }
    return { files, sha: commit.sha };
  }

  // ---------- review operations ----------

  async replyInThread(owner: string, repo: string, number: number, threadRef: string, body: string): Promise<void> {
    const id = commentId(threadRef);
    try {
      await this.call(owner, repo, "POST", `${repoPath(owner, repo)}/pulls/${number}/comments/${id}/replies`, { body });
    } catch (e) {
      throw wrap(`github: reply in thread ${threadRef} of ${owner}/${repo}#${number}`, e);
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

  private async reviewThreadId(
    owner: string,
    repo: string,
    number: number,
    id: number,
  ): Promise<{ nodeId: string; resolved: boolean }> {
    let after: string | null = null;
    for (let page = 0; page < 20; page++) {
      type Page = {
        repository?: {
          pullRequest?: {
            reviewThreads?: {
              pageInfo: { hasNextPage: boolean; endCursor: string };
              nodes: { id: string; isResolved: boolean; comments: { nodes: { databaseId: number }[] } }[];
            };
          };
        };
      };
      const data: Page = await this.graphql<Page>(owner, repo, REVIEW_THREADS_QUERY, { owner, repo, number, after });
      const rt = data?.repository?.pullRequest?.reviewThreads;
      for (const t of rt?.nodes ?? []) {
        if (t.comments.nodes.some((c) => c.databaseId === id)) return { nodeId: t.id, resolved: t.isResolved };
      }
      if (!rt?.pageInfo.hasNextPage) break;
      after = rt.pageInfo.endCursor;
    }
    throw new OfError("resource_not_found", `github: no review thread of ${owner}/${repo}#${number} has comment ${id}`);
  }

  async resolveThread(owner: string, repo: string, number: number, threadRef: string): Promise<void> {
    const id = commentId(threadRef);
    let t: { nodeId: string; resolved: boolean };
    try {
      t = await this.reviewThreadId(owner, repo, number, id);
    } catch (e) {
      throw wrap("github: resolve thread", e);
    }
    if (t.resolved) return;
    const m = "mutation($id:ID!){resolveReviewThread(input:{threadId:$id}){thread{id isResolved}}}";
    try {
      await this.graphql(owner, repo, m, { id: t.nodeId });
    } catch (e) {
      throw wrap(`github: resolve thread ${threadRef} of ${owner}/${repo}#${number}`, e);
    }
  }

  async submitReview(input: ReviewInput): Promise<void> {
    let event = "COMMENT";
    switch ((input.event ?? "").toLowerCase()) {
      case "":
      case "comment":
        break;
      case "approve":
        event = "APPROVE";
        break;
      case "request_changes":
        event = "REQUEST_CHANGES";
        break;
      default:
        throw new OfError("invalid_request", `unknown review event "${input.event}"`);
    }
    const comments = (input.comments ?? []).map((c) => {
      if (!c.path || !(c.line > 0) || c.body.trim() === "") {
        throw new OfError("invalid_request", "an inline comment needs a path, a line and a body");
      }
      return { path: c.path, line: c.line, side: (c.side ?? "").toUpperCase() || "RIGHT", body: c.body };
    });
    try {
      await this.call(
        input.owner,
        input.repo,
        "POST",
        `${repoPath(input.owner, input.repo)}/pulls/${input.number}/reviews`,
        {
          event,
          body: input.body,
          comments,
        },
      );
    } catch (e) {
      throw wrap(`github: submit review on ${input.owner}/${input.repo}#${input.number}`, e);
    }
  }

  async markReady(owner: string, repo: string, number: number): Promise<void> {
    let pr: { node_id: string; draft: boolean };
    try {
      pr = await this.call(owner, repo, "GET", `${repoPath(owner, repo)}/pulls/${number}`);
    } catch (e) {
      throw wrap(`github: read ${owner}/${repo}#${number}`, e);
    }
    if (!pr.draft) return;
    const m = "mutation($id:ID!){markPullRequestReadyForReview(input:{pullRequestId:$id}){pullRequest{id isDraft}}}";
    try {
      await this.graphql(owner, repo, m, { id: pr.node_id });
    } catch (e) {
      throw wrap(`github: mark ${owner}/${repo}#${number} ready`, e);
    }
  }

  async requestReviewers(owner: string, repo: string, number: number, logins: string[]): Promise<void> {
    if (logins.length === 0) return;
    try {
      await this.call(owner, repo, "POST", `${repoPath(owner, repo)}/pulls/${number}/requested_reviewers`, {
        reviewers: logins,
      });
    } catch (e) {
      throw wrap(`github: request review of ${owner}/${repo}#${number}`, e);
    }
  }

  /** The login of the account of a personal token (not available for an App). */
  async login(): Promise<string> {
    if (this.keyPem) throw new Error("github: an app credential has no user login");
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

  /** The thread of the first comment threadRef: that comment and each reply to it. */
  async reviewThread(owner: string, repo: string, number: number, threadRef: string): Promise<ReviewThread> {
    const root = commentId(threadRef);
    const th: ReviewThread = { ref: String(root), comments: [] };
    let found = false;
    for (let page = 1; page <= MAX_REVIEW_PAGES; page++) {
      type C = {
        id: number;
        in_reply_to_id?: number;
        body?: string;
        path?: string;
        line?: number | null;
        original_line?: number | null;
        created_at?: string;
        user?: { login?: string };
      };
      let cs: C[];
      try {
        cs =
          (await this.call<C[]>(
            owner,
            repo,
            "GET",
            `${repoPath(owner, repo)}/pulls/${number}/comments?per_page=100&page=${page}`,
          )) ?? [];
      } catch (e) {
        throw wrap(`github: read review comments of ${owner}/${repo}#${number}`, e);
      }
      for (const x of cs) {
        if (x.id !== root && x.in_reply_to_id !== root) continue;
        found = true;
        const line = x.line || x.original_line || 0;
        const tc: ThreadComment = {
          id: String(x.id),
          author: x.user?.login ?? "",
          body: x.body ?? "",
          path: x.path ?? "",
          line,
        };
        const t = x.created_at ? Date.parse(x.created_at) : NaN;
        if (Number.isFinite(t)) tc.created_at = new Date(t).toISOString().replace(".000Z", "Z");
        if (x.id === root) {
          th.path = x.path ?? "";
          th.line = line;
        }
        th.comments!.push(tc);
      }
      if (cs.length < 100) break;
    }
    if (!found)
      throw new OfError("resource_not_found", `github: review thread ${threadRef} of ${owner}/${repo}#${number}`);
    return th;
  }

  /** The patch of one file of a pull request; not_found for binary or very large files. */
  async fileDiff(owner: string, repo: string, number: number, path: string): Promise<FileDiff> {
    const p = path.trim().replace(/^\//, "");
    if (p === "") throw new OfError("invalid_request", "github: a file path is required");
    for (let page = 1; page <= MAX_REVIEW_PAGES; page++) {
      let files: { filename: string; patch?: string }[];
      try {
        files =
          (await this.call(
            owner,
            repo,
            "GET",
            `${repoPath(owner, repo)}/pulls/${number}/files?per_page=100&page=${page}`,
          )) ?? [];
      } catch (e) {
        throw wrap(`github: read files of ${owner}/${repo}#${number}`, e);
      }
      for (const f of files) {
        if (f.filename !== p) continue;
        if (!f.patch)
          throw new OfError("resource_not_found", `github: ${p} has no patch (the file is binary or too large)`);
        return { path: p, patch: f.patch };
      }
      if (files.length < 100) break;
    }
    throw new OfError("resource_not_found", `github: ${p} is not changed in ${owner}/${repo}#${number}`);
  }
}

const REVIEW_THREADS_QUERY = `query($owner:String!,$repo:String!,$number:Int!,$after:String){
  repository(owner:$owner,name:$repo){pullRequest(number:$number){
    reviewThreads(first:100,after:$after){
      pageInfo{hasNextPage endCursor}
      nodes{id isResolved comments(first:100){nodes{databaseId}}}
    }}}}`;

/** A check run to create or update. */
export interface GitHubCheckRun extends CheckRunInput {
  externalId?: string;
}

export function checkRunBody(cr: GitHubCheckRun, create: boolean, nowMs: number): Record<string, unknown> {
  const b: Record<string, unknown> = {};
  if (create) {
    b["name"] = cr.name;
    b["head_sha"] = cr.headSha;
  }
  if (cr.status) b["status"] = cr.status;
  if (cr.conclusion) {
    b["conclusion"] = cr.conclusion;
    b["completed_at"] = new Date(nowMs).toISOString().replace(/\.\d{3}Z$/, "Z");
  }
  if (cr.detailsUrl) b["details_url"] = cr.detailsUrl;
  if (cr.externalId) b["external_id"] = cr.externalId;
  if (cr.title || cr.summary) {
    const out: Record<string, string> = {
      title: truncate(cr.title || cr.name, 255),
      summary: truncate(cr.summary ?? "", 65535),
    };
    if (cr.text) out["text"] = truncate(cr.text, 65535);
    b["output"] = out;
  }
  return b;
}
