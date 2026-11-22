// GitLab REST v4 client for the forge operations (merge requests, notes,
// commit statuses, definition trees, review threads). Port of
// plugins/gitlab/internal/gitlab/{client,review,reviewcontext}.go.

import { OfError, encodeQuery, pathEscape } from "@openfactory/core";
import type {
  CommitStatus,
  FileDiff,
  PullRequestInput,
  PullRequestRef,
  ReviewInput,
  ReviewThread,
  ThreadComment,
} from "@openfactory/plugin-api";
import { ApiClient, ApiError, fromApiError, isNotFound, notSupported, truncate } from "@openfactory/plugin-api";

/** The code-forge identifier used in definitions. */
export const FORGE_NAME = "GITLAB";
export const DEFAULT_BASE_URL = "https://gitlab.com/api/v4";
export const MAX_TREE_FILES = 2000;
export const MAX_FILE_BYTES = 1 << 20;
/** Pages (of 100) that one read follows. */
const MAX_PAGES = 30;

export interface GitLabConfig {
  /** API root, for example https://gitlab.example.com/api/v4. */
  baseUrl?: string;
  /** Git host root; derived from baseUrl when empty. */
  webUrl?: string;
  token: string;
  /** "private" (PRIVATE-TOKEN header, default) or "oauth" (Bearer). */
  tokenType?: string;
  fetch?: typeof fetch;
}

interface MergeRequest {
  iid: number;
  web_url?: string;
  state?: string;
  draft?: boolean;
  work_in_progress?: boolean;
  source_branch?: string;
}

interface MrDetails {
  title?: string;
  draft?: boolean;
  diff_refs?: { base_sha?: string; head_sha?: string; start_sha?: string };
}

interface MrChange {
  old_path?: string;
  new_path?: string;
  diff?: string;
  collapsed?: boolean;
  too_large?: boolean;
}

/** The title prefixes that make a merge request a draft. */
const DRAFT_PREFIXES = ["draft:", "[draft]", "(draft)", "wip:", "[wip]"];

export function projectPath(owner: string, repo: string): string {
  const full = `${owner}/${repo}`.replace(/^\/+|\/+$/g, "");
  return "/projects/" + pathEscape(full);
}

export class GitLabClient {
  readonly baseUrl: string;
  readonly webUrl: string;
  private readonly api: ApiClient;
  private readonly token: string;

  constructor(cfg: GitLabConfig) {
    if (!cfg.token) throw new OfError("invalid_request", "gitlab: a token is required");
    this.token = cfg.token;
    let base = (cfg.baseUrl || DEFAULT_BASE_URL).replace(/\/+$/, "");
    if (!base.endsWith("/api/v4")) base += "/api/v4";
    this.baseUrl = base;
    this.webUrl = (cfg.webUrl ?? "").replace(/\/+$/, "") || base.slice(0, -"/api/v4".length);
    const tt = (cfg.tokenType ?? "").toLowerCase();
    const bearer = tt === "oauth" || tt === "bearer";
    this.api = new ApiClient({
      baseUrl: base,
      userAgent: "openfactory-gitlab",
      ...(cfg.fetch ? { fetch: cfg.fetch } : {}),
      authorize: (h) => {
        if (bearer) h.set("Authorization", "Bearer " + cfg.token);
        else h.set("PRIVATE-TOKEN", cfg.token);
      },
    });
  }

  cloneUrl(owner: string, repo: string): string {
    const u = new URL(this.webUrl);
    u.username = "oauth2";
    u.password = this.token;
    u.pathname = u.pathname.replace(/\/+$/, "") + "/" + `${owner}/${repo}`.replace(/^\/+|\/+$/g, "") + ".git";
    return u.toString();
  }

  async defaultBranch(owner: string, repo: string): Promise<string> {
    const p = await this.api.json<{ default_branch?: string }>("GET", projectPath(owner, repo));
    return p.default_branch ?? "";
  }

  private ref(m: MergeRequest, owner: string, repo: string): PullRequestRef {
    let state = m.state ?? "";
    if (state === "opened") state = m.draft || m.work_in_progress ? "draft" : "open";
    else if (state === "locked") state = "open";
    const ref: PullRequestRef = {
      forge: FORGE_NAME,
      repo: `${owner}/${repo}`.replace(/^\/+|\/+$/g, ""),
      number: m.iid,
      state: state as PullRequestRef["state"],
    };
    if (m.web_url) ref.url = m.web_url;
    if (m.source_branch) ref.branch = m.source_branch;
    return ref;
  }

  /** Opens a merge request. A draft gets the "Draft:" title prefix. */
  async createPullRequest(owner: string, repo: string, pr: PullRequestInput): Promise<PullRequestRef> {
    const base = pr.base || (await this.defaultBranch(owner, repo));
    let title = pr.title;
    if (pr.draft && !title.toLowerCase().startsWith("draft:")) title = "Draft: " + title;
    const mr = await this.wrap("create merge request", () =>
      this.api.json<MergeRequest>("POST", projectPath(owner, repo) + "/merge_requests", {
        source_branch: pr.head,
        target_branch: base,
        title,
        description: pr.body,
        remove_source_branch: true,
      }),
    );
    return this.ref(mr, owner, repo);
  }

  async getPullRequest(owner: string, repo: string, number: number): Promise<PullRequestRef> {
    const mr = await this.api.json<MergeRequest>("GET", `${projectPath(owner, repo)}/merge_requests/${number}`);
    return this.ref(mr, owner, repo);
  }

  async comment(owner: string, repo: string, number: number, body: string): Promise<void> {
    await this.wrap(`comment on merge request !${number}`, () =>
      this.api.send("POST", `${projectPath(owner, repo)}/merge_requests/${number}/notes`, { body }),
    );
  }

  async commentIssue(owner: string, repo: string, number: number, body: string): Promise<void> {
    await this.wrap(`comment on issue #${number}`, () =>
      this.api.send("POST", `${projectPath(owner, repo)}/issues/${number}/notes`, { body }),
    );
  }

  async setCommitStatus(owner: string, repo: string, sha: string, st: CommitStatus): Promise<void> {
    let state: string = st.state;
    if (state === "" || state === "pending") state = "pending";
    else if (state === "failure" || state === "error") state = "failed";
    const body: Record<string, string> = {
      state,
      name: st.context,
      description: truncate(st.description ?? "", 255),
    };
    if (st.targetUrl) body["target_url"] = st.targetUrl;
    await this.wrap("set commit status", () =>
      this.api.send("POST", `${projectPath(owner, repo)}/statuses/${pathEscape(sha)}`, body),
    );
  }

  /** Reads the files under path with the tree and raw file APIs. Keys are relative to path. */
  async getFiles(
    owner: string,
    repo: string,
    ref: string,
    path: string,
  ): Promise<{ files: Record<string, string>; commitSha: string }> {
    const pp = projectPath(owner, repo);
    const r = ref || (await this.defaultBranch(owner, repo));
    const commit = await this.wrap(`resolve ${r}`, () =>
      this.api.json<{ id: string }>("GET", `${pp}/repository/commits/${pathEscape(r)}`),
    );
    const dir = path.replace(/^\/+|\/+$/g, "");
    const files: Record<string, string> = {};
    let count = 0;
    for (let page = 1; ; page++) {
      const q: Record<string, string> = { ref: commit.id, recursive: "true", per_page: "100", page: String(page) };
      if (dir !== "") q["path"] = dir;
      const res = await this.wrap("read tree", () =>
        this.api.request<Array<{ path: string; type: string }>>(
          "GET",
          `${pp}/repository/tree?${encodeQuery(q)}`,
          undefined,
          "json",
        ),
      );
      const entries = res.data ?? [];
      for (const e of entries) {
        if (e.type !== "blob") continue;
        if (count >= MAX_TREE_FILES) {
          throw new OfError("invalid_request", `gitlab: more than ${MAX_TREE_FILES} files under "${path}"`);
        }
        const raw = await this.wrap(`read ${e.path}`, () =>
          this.api.bytes("GET", `${pp}/repository/files/${pathEscape(e.path)}/raw?${encodeQuery({ ref: commit.id })}`),
        );
        if (raw.byteLength > MAX_FILE_BYTES) {
          throw new OfError("invalid_request", `gitlab: ${e.path} exceeds ${MAX_FILE_BYTES} bytes`);
        }
        const rel = dir !== "" && e.path.startsWith(dir + "/") ? e.path.slice(dir.length + 1) : e.path;
        files[rel] = new TextDecoder().decode(raw);
        count++;
      }
      const next = res.headers.get("X-Next-Page") ?? "";
      if (next === "" || entries.length === 0) break;
    }
    return { files, commitSha: commit.id };
  }

  // ---------- Review operations. A thread reference is the ID of a merge request discussion. ----------

  private discussionPath(owner: string, repo: string, number: number, id: string): string {
    return `${projectPath(owner, repo)}/merge_requests/${number}/discussions/${pathEscape(id)}`;
  }

  private requireRef(threadRef: string): void {
    if (threadRef.trim() === "") throw new OfError("invalid_request", "gitlab: a discussion ID is required");
  }

  async replyInThread(owner: string, repo: string, number: number, threadRef: string, body: string): Promise<void> {
    this.requireRef(threadRef);
    await this.wrap(`reply in discussion ${threadRef} of !${number}`, () =>
      this.api.send("POST", this.discussionPath(owner, repo, number, threadRef) + "/notes", { body }),
    );
  }

  async resolveThread(owner: string, repo: string, number: number, threadRef: string): Promise<void> {
    this.requireRef(threadRef);
    await this.wrap(`resolve discussion ${threadRef} of !${number}`, () =>
      this.api.send("PUT", this.discussionPath(owner, repo, number, threadRef) + "?resolved=true"),
    );
  }

  private mrDetails(owner: string, repo: string, number: number): Promise<MrDetails> {
    return this.api.json<MrDetails>("GET", `${projectPath(owner, repo)}/merge_requests/${number}`);
  }

  /**
   * Each inline comment becomes a diff discussion and the body becomes a note.
   * An approval approves the merge request. GitLab has no "request changes" call.
   */
  async submitReview(owner: string, repo: string, number: number, input: ReviewInput): Promise<void> {
    const event = (input.event ?? "").toLowerCase();
    if (event === "request_changes") throw notSupported("gitlab: request changes");
    if (event !== "" && event !== "comment" && event !== "approve") {
      throw new OfError("invalid_request", `unknown review event "${input.event}"`);
    }
    const comments = input.comments ?? [];
    if (comments.length > 0) {
      const mr = await this.wrap(`read !${number}`, () => this.mrDetails(owner, repo, number));
      for (const cm of comments) {
        if (cm.path === "" || cm.line <= 0 || cm.body.trim() === "") {
          throw new OfError("invalid_request", "an inline comment needs a path, a line and a body");
        }
        const pos: Record<string, unknown> = {
          position_type: "text",
          base_sha: mr.diff_refs?.base_sha ?? "",
          head_sha: mr.diff_refs?.head_sha ?? "",
          start_sha: mr.diff_refs?.start_sha ?? "",
          new_path: cm.path,
          old_path: cm.path,
        };
        if ((cm.side ?? "").toUpperCase() === "LEFT") pos["old_line"] = cm.line;
        else pos["new_line"] = cm.line;
        await this.wrap(`inline comment on ${cm.path}:${cm.line} of !${number}`, () =>
          this.api.send("POST", `${projectPath(owner, repo)}/merge_requests/${number}/discussions`, {
            body: cm.body,
            position: pos,
          }),
        );
      }
    }
    if (input.body.trim() !== "") await this.comment(owner, repo, number, input.body);
    if (event === "approve") {
      await this.wrap(`approve !${number}`, () =>
        this.api.send("POST", `${projectPath(owner, repo)}/merge_requests/${number}/approve`),
      );
    }
  }

  /** GitLab keeps the draft state in the title: remove the draft prefix. */
  async markReady(owner: string, repo: string, number: number): Promise<void> {
    const mr = await this.wrap(`read !${number}`, () => this.mrDetails(owner, repo, number));
    if (!mr.draft) return;
    let title = (mr.title ?? "").trim();
    for (let changed = true; changed;) {
      changed = false;
      for (const p of DRAFT_PREFIXES) {
        if (title.length >= p.length && title.slice(0, p.length).toLowerCase() === p) {
          title = title.slice(p.length).trim();
          changed = true;
        }
      }
    }
    await this.wrap(`mark !${number} ready`, () =>
      this.api.send("PUT", `${projectPath(owner, repo)}/merge_requests/${number}`, { title }),
    );
  }

  // ---------- Review context ----------

  async reviewThread(owner: string, repo: string, number: number, threadRef: string): Promise<ReviewThread> {
    this.requireRef(threadRef);
    interface Note {
      id: number;
      body: string;
      created_at?: string;
      system?: boolean;
      resolved?: boolean;
      resolvable?: boolean;
      author?: { username?: string };
      position?: { new_path?: string; old_path?: string; new_line?: number; old_line?: number } | null;
    }
    const d = await this.wrap(`read discussion ${threadRef} of !${number}`, () =>
      this.api.json<{ id: string; notes?: Note[] }>("GET", this.discussionPath(owner, repo, number, threadRef)),
    );
    const th: ReviewThread = { ref: threadRef, resolved: false, comments: [] };
    let resolvable = 0;
    let resolved = 0;
    for (const n of d.notes ?? []) {
      if (n.system) continue;
      const tc: ThreadComment = { id: String(n.id), author: n.author?.username ?? "", body: n.body, createdAt: "" };
      const pos = n.position;
      if (pos) {
        tc.path = pos.new_path || pos.old_path || "";
        tc.line = pos.new_line || pos.old_line || 0;
        if (th.path === undefined) {
          th.path = tc.path;
          th.line = tc.line;
        }
      }
      if (n.created_at) {
        const t = new Date(n.created_at);
        if (!Number.isNaN(t.getTime())) tc.createdAt = t.toISOString();
      }
      if (n.resolvable) {
        resolvable++;
        if (n.resolved) resolved++;
      }
      th.comments.push(tc);
    }
    th.resolved = resolvable > 0 && resolved === resolvable;
    return th;
  }

  /** GitLab leaves the diff of a large or generated file empty: the error is resource_not_found. */
  async fileDiff(owner: string, repo: string, number: number, path: string): Promise<FileDiff> {
    const p = path.trim().replace(/^\/+/, "");
    if (p === "") throw new OfError("invalid_request", "gitlab: a file path is required");
    const mrPath = `${projectPath(owner, repo)}/merge_requests/${number}`;
    let hit: MrChange | undefined;
    for (let page = 1; page <= MAX_PAGES && !hit; page++) {
      let cs: MrChange[];
      let next = "";
      try {
        const res = await this.api.request<MrChange[]>(
          "GET",
          `${mrPath}/diffs?per_page=100&page=${page}`,
          undefined,
          "json",
        );
        cs = res.data ?? [];
        next = res.headers.get("X-Next-Page") ?? "";
      } catch (e) {
        if (!(isNotFound(e) && page === 1)) throw this.fail(`read the diff of !${number}`, e);
        // GitLab before 15.7 has only the "changes" call, which holds all files.
        const old = await this.wrap(`read the diff of !${number}`, () =>
          this.api.json<{ changes?: MrChange[] }>("GET", `${mrPath}/changes`),
        );
        cs = old.changes ?? [];
      }
      hit = cs.find((c) => c.new_path === p || c.old_path === p);
      if (next === "" || cs.length === 0) break;
    }
    if (!hit) throw new OfError("resource_not_found", `gitlab: ${p} is not changed in !${number}`);
    if ((hit.diff ?? "").trim() === "") {
      const reason =
        hit.too_large || hit.collapsed ? "GitLab left the diff out because the file is too large" : "the diff is empty";
      throw new OfError("resource_not_found", `gitlab: ${p}: ${reason}`);
    }
    return { path: p, patch: hit.diff!, truncated: false };
  }

  /** The username of the account of the token. */
  async login(): Promise<string> {
    const u = await this.wrap("read the user of the token", () => this.api.json<{ username?: string }>("GET", "/user"));
    const name = (u.username ?? "").trim();
    if (name === "")
      throw new OfError(
        "invalid_request",
        "gitlab: the token has no username (a project or group token is not a user)",
      );
    return name;
  }

  /** logins are usernames. GitLab replaces the reviewers on update, so the call keeps the present ones. */
  async requestReviewers(owner: string, repo: string, number: number, logins: string[]): Promise<void> {
    if (logins.length === 0) return;
    const mrPath = `${projectPath(owner, repo)}/merge_requests/${number}`;
    const mr = await this.wrap(`read !${number}`, () =>
      this.api.json<{ reviewers?: Array<{ id: number }> }>("GET", mrPath),
    );
    const ids: number[] = [];
    const seen = new Set<number>();
    for (const r of mr.reviewers ?? []) {
      ids.push(r.id);
      seen.add(r.id);
    }
    for (const raw of logins) {
      const login = raw.trim();
      const users = await this.wrap(`find user "${login}"`, () =>
        this.api.json<Array<{ id: number; username: string }>>("GET", `/users?${encodeQuery({ username: login })}`),
      );
      let id = 0;
      for (const u of users ?? []) if (u.username.toLowerCase() === login.toLowerCase()) id = u.id;
      if (id === 0) throw new OfError("resource_not_found", `gitlab: user "${login}" not found`);
      if (!seen.has(id)) {
        ids.push(id);
        seen.add(id);
      }
    }
    await this.wrap(`request review of !${number}`, () => this.api.send("PUT", mrPath, { reviewer_ids: ids }));
  }

  // Adds context to an error and keeps its code.
  private fail(what: string, e: unknown): unknown {
    if (e instanceof ApiError) {
      const o = fromApiError(e);
      return new OfError(o.code, `gitlab: ${what}: ${o.detail}`, { cause: e });
    }
    if (e instanceof OfError) return new OfError(e.code, `gitlab: ${what}: ${e.detail}`, { cause: e });
    if (e instanceof Error) return new Error(`gitlab: ${what}: ${e.message}`, { cause: e });
    return e;
  }

  private async wrap<T>(what: string, fn: () => Promise<T>): Promise<T> {
    try {
      return await fn();
    } catch (e) {
      throw this.fail(what, e);
    }
  }
}
