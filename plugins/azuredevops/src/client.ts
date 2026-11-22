// Azure DevOps Services REST 7.1 client with personal-access-token basic
// authentication. Port of plugins/azuredevops/internal/azuredevops/{client,review,
// reviewcontext,tracker}.go.
//
// Repository addressing: owner is the Azure DevOps project, repo is the
// repository name (or ID). The organization comes from the client config.

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
import { ApiClient, ApiError, fromApiError, notSupported, splitRepo, truncate } from "@openfactory/plugin-api";
import { unifiedDiff } from "./diff";
import { pickState, type TypeState } from "./tracker";

export const FORGE_NAME = "AZURE_DEVOPS";
export const DEFAULT_BASE_URL = "https://dev.azure.com";
const API_VERSION = "7.1";
export const MAX_TREE_FILES = 2000;
export const MAX_FILE_BYTES = 1 << 20;

export interface AzureDevOpsConfig {
  organization: string;
  /** https://dev.azure.com (default). */
  baseUrl?: string;
  /** Personal access token. */
  token: string;
  fetch?: typeof fetch;
}

interface Repository {
  id?: string;
  name?: string;
  defaultBranch?: string;
  webUrl?: string;
  project?: { id?: string };
}

interface PullRequest {
  pullRequestId: number;
  status?: string;
  isDraft?: boolean;
  sourceRefName?: string;
  repository?: Repository;
}

interface WorkItem {
  fields?: Record<string, unknown>;
  relations?: Array<{ rel: string; url: string }> | null;
}

const SHA_RE = /^[0-9a-fA-F]{40}$/;

function repoAPI(project: string, repo: string): string {
  return `/${pathEscape(project)}/_apis/git/repositories/${pathEscape(repo)}`;
}

function withVersion(path: string, q: Record<string, string | string[]> = {}): string {
  return `${path}?${encodeQuery({ ...q, "api-version": API_VERSION })}`;
}

function headsRef(b: string): string {
  return b.startsWith("refs/") ? b : "refs/heads/" + b;
}

function threadId(threadRef: string): number {
  const t = threadRef.trim();
  const id = /^[+-]?\d+$/.test(t) ? Number(t) : NaN;
  if (!Number.isInteger(id) || id <= 0) {
    throw new OfError("invalid_request", `azuredevops: thread reference "${threadRef}" is not a thread ID`);
  }
  return id;
}

const stripSlash = (s: string) => s.replace(/^\/+/, "");

export class AzureDevOpsClient {
  readonly baseUrl: string;
  readonly organization: string;
  private readonly api: ApiClient;
  private readonly token: string;

  constructor(cfg: AzureDevOpsConfig) {
    if (!cfg.token) throw new OfError("invalid_request", "azuredevops: a personal access token is required");
    if (!cfg.organization) throw new OfError("invalid_request", "azuredevops: organization is required");
    this.token = cfg.token;
    this.organization = cfg.organization;
    this.baseUrl = (cfg.baseUrl || DEFAULT_BASE_URL).replace(/\/+$/, "");
    const auth = "Basic " + btoa(":" + cfg.token);
    this.api = new ApiClient({
      baseUrl: `${this.baseUrl}/${pathEscape(cfg.organization)}`,
      userAgent: "openfactory-azuredevops",
      ...(cfg.fetch ? { fetch: cfg.fetch } : {}),
      authorize: (h, { method, url }) => {
        h.set("Authorization", auth);
        if (method === "PATCH" && url.pathname.toLowerCase().includes("/_apis/wit/workitems/")) {
          // A work item is changed with a JSON Patch document.
          h.set("Content-Type", "application/json-patch+json");
        }
      },
    });
  }

  cloneUrl(project: string, repo: string): string {
    const u = new URL(this.baseUrl);
    u.username = "pat";
    u.password = this.token;
    u.pathname = `${u.pathname.replace(/\/+$/, "")}/${this.organization}/${project}/_git/${repo}`;
    return u.toString();
  }

  async defaultBranch(project: string, repo: string): Promise<string> {
    const r = await this.api.json<Repository>("GET", withVersion(repoAPI(project, repo)));
    return (r.defaultBranch ?? "").replace(/^refs\/heads\//, "");
  }

  private ref(p: PullRequest, project: string, repo: string): PullRequestRef {
    let state: PullRequestRef["state"] = "open";
    if (p.status === "completed") state = "merged";
    else if (p.status === "abandoned") state = "closed";
    else if (p.isDraft) state = "draft";
    const ref: PullRequestRef = { forge: FORGE_NAME, repo: `${project}/${repo}`, number: p.pullRequestId, state };
    if (p.repository?.webUrl) ref.url = `${p.repository.webUrl}/pullrequest/${p.pullRequestId}`;
    const branch = (p.sourceRefName ?? "").replace(/^refs\/heads\//, "");
    if (branch !== "") ref.branch = branch;
    return ref;
  }

  async createPullRequest(project: string, repo: string, pr: PullRequestInput): Promise<PullRequestRef> {
    const base = pr.base || (await this.defaultBranch(project, repo));
    const created = await this.wrap("create pull request", () =>
      this.api.json<PullRequest>("POST", withVersion(`${repoAPI(project, repo)}/pullrequests`), {
        sourceRefName: headsRef(pr.head),
        targetRefName: headsRef(base),
        title: pr.title,
        description: truncate(pr.body, 4000),
        isDraft: pr.draft,
      }),
    );
    return this.ref(created, project, repo);
  }

  async getPullRequest(project: string, repo: string, number: number): Promise<PullRequestRef> {
    const pr = await this.api.json<PullRequest>("GET", withVersion(`${repoAPI(project, repo)}/pullrequests/${number}`));
    return this.ref(pr, project, repo);
  }

  /** Opens a new pull request comment thread. */
  async comment(project: string, repo: string, number: number, body: string): Promise<void> {
    await this.wrap(`comment on pull request ${number}`, () =>
      this.api.send("POST", withVersion(`${repoAPI(project, repo)}/pullRequests/${number}/threads`), {
        comments: [{ parentCommentId: 0, content: body, commentType: 1 }],
        status: 1, // active
      }),
    );
  }

  async commentWorkItem(project: string, id: number, body: string): Promise<void> {
    const path = `/${pathEscape(project)}/_apis/wit/workItems/${id}/comments?api-version=7.1-preview.4`;
    await this.wrap(`comment on work item ${id}`, () => this.api.send("POST", path, { text: body }));
  }

  /** The context "genre/name" is split at the last slash. */
  async setCommitStatus(project: string, repo: string, sha: string, st: CommitStatus): Promise<void> {
    let state: string = st.state;
    switch (state) {
      case "":
      case "pending":
        state = "pending";
        break;
      case "success":
        state = "succeeded";
        break;
      case "failure":
        state = "failed";
        break;
      default:
    }
    let genre = "";
    let name = st.context;
    const i = st.context.lastIndexOf("/");
    if (i > 0) {
      genre = st.context.slice(0, i);
      name = st.context.slice(i + 1);
    }
    const body: Record<string, unknown> = {
      state,
      description: truncate(st.description ?? "", 255),
      context: { genre, name },
    };
    if (st.targetUrl) body["targetUrl"] = st.targetUrl;
    await this.wrap("set commit status", () =>
      this.api.send("POST", withVersion(`${repoAPI(project, repo)}/commits/${pathEscape(sha)}/statuses`), body),
    );
  }

  /** Reads the files under path with the items and blobs APIs. Keys are relative to path. */
  async getFiles(
    project: string,
    repo: string,
    ref: string,
    path: string,
  ): Promise<{ files: Record<string, string>; commitSha: string }> {
    const r = ref || (await this.defaultBranch(project, repo));
    const vtype = SHA_RE.test(r) ? "commit" : "branch";
    const scope = "/" + path.replace(/^\/+|\/+$/g, "");
    const q = {
      scopePath: scope,
      recursionLevel: "Full",
      "versionDescriptor.version": r.replace(/^refs\/heads\//, ""),
      "versionDescriptor.versionType": vtype,
      latestProcessedChange: "true",
    };
    interface Item {
      objectId?: string;
      gitObjectType?: string;
      commitId?: string;
      path: string;
      isFolder?: boolean;
    }
    const items = await this.wrap("list items", () =>
      this.api.json<{ value?: Item[] }>("GET", withVersion(`${repoAPI(project, repo)}/items`, q)),
    );
    let commitSha = vtype === "commit" ? r : "";
    const prefix = scope.replace(/\/+$/, "") + "/";
    const files: Record<string, string> = {};
    let count = 0;
    for (const it of items.value ?? []) {
      if (commitSha === "" && it.commitId && it.path === scope) commitSha = it.commitId;
      if (it.isFolder || it.gitObjectType !== "blob") continue;
      if (count >= MAX_TREE_FILES) {
        throw new OfError("invalid_request", `azuredevops: more than ${MAX_TREE_FILES} files under "${path}"`);
      }
      const raw = await this.wrap(`read ${it.path}`, () =>
        this.api.bytes(
          "GET",
          withVersion(`${repoAPI(project, repo)}/blobs/${pathEscape(it.objectId ?? "")}`, { $format: "octetstream" }),
        ),
      );
      if (raw.byteLength > MAX_FILE_BYTES) {
        throw new OfError("invalid_request", `azuredevops: ${it.path} exceeds ${MAX_FILE_BYTES} bytes`);
      }
      files[it.path.startsWith(prefix) ? it.path.slice(prefix.length) : it.path] = new TextDecoder().decode(raw);
      count++;
    }
    if (commitSha === "") {
      // Resolve the branch head explicitly.
      try {
        const refs = await this.api.json<{ value?: Array<{ objectId: string }> }>(
          "GET",
          withVersion(`${repoAPI(project, repo)}/refs`, { filter: "heads/" + r.replace(/^refs\/heads\//, "") }),
        );
        commitSha = refs.value?.[0]?.objectId ?? "";
      } catch {
        // The files are valid without a commit ID.
      }
    }
    return { files, commitSha };
  }

  // ---------- Review operations. A thread reference is the ID of a pull request comment thread. ----------

  async replyInThread(project: string, repo: string, number: number, threadRef: string, body: string): Promise<void> {
    const id = threadId(threadRef);
    await this.wrap(`reply in thread ${id} of pull request ${number}`, () =>
      this.api.send("POST", withVersion(`${repoAPI(project, repo)}/pullRequests/${number}/threads/${id}/comments`), {
        parentCommentId: 1,
        content: body,
        commentType: 1,
      }),
    );
  }

  /** Sets the thread status to "fixed". */
  async resolveThread(project: string, repo: string, number: number, threadRef: string): Promise<void> {
    const id = threadId(threadRef);
    await this.wrap(`resolve thread ${id} of pull request ${number}`, () =>
      this.api.send("PATCH", withVersion(`${repoAPI(project, repo)}/pullRequests/${number}/threads/${id}`), {
        status: "fixed",
      }),
    );
  }

  /**
   * A comment review: each inline comment becomes a thread on its file. A vote
   * needs the reviewer identity of the credential, so approve and
   * request_changes are not supported.
   */
  async submitReview(project: string, repo: string, number: number, input: ReviewInput): Promise<void> {
    const event = (input.event ?? "").toLowerCase();
    if (event === "approve" || event === "request_changes") throw notSupported("azuredevops: review vote");
    if (event !== "" && event !== "comment") {
      throw new OfError("invalid_request", `unknown review event "${input.event}"`);
    }
    const path = withVersion(`${repoAPI(project, repo)}/pullRequests/${number}/threads`);
    for (const cm of input.comments ?? []) {
      if (cm.path === "" || cm.line <= 0 || cm.body.trim() === "") {
        throw new OfError("invalid_request", "an inline comment needs a path, a line and a body");
      }
      const file = cm.path.startsWith("/") ? cm.path : "/" + cm.path;
      const pos = { line: cm.line, offset: 1 };
      const left = (cm.side ?? "").toUpperCase() === "LEFT";
      await this.wrap(`inline comment on ${cm.path}:${cm.line} of pull request ${number}`, () =>
        this.api.send("POST", path, {
          comments: [{ parentCommentId: 0, content: cm.body, commentType: 1 }],
          status: 1,
          threadContext: {
            filePath: file,
            [left ? "leftFileStart" : "rightFileStart"]: pos,
            [left ? "leftFileEnd" : "rightFileEnd"]: pos,
          },
        }),
      );
    }
    if (input.body.trim() !== "") await this.comment(project, repo, number, input.body);
  }

  async markReady(project: string, repo: string, number: number): Promise<void> {
    await this.wrap(`mark pull request ${number} ready`, () =>
      this.api.send("PATCH", withVersion(`${repoAPI(project, repo)}/pullrequests/${number}`), { isDraft: false }),
    );
  }

  // ---------- Review context ----------

  async reviewThread(project: string, repo: string, number: number, threadRef: string): Promise<ReviewThread> {
    const id = threadId(threadRef);
    interface Thread {
      status?: string;
      threadContext?: {
        filePath?: string;
        rightFileStart?: { line: number } | null;
        leftFileStart?: { line: number } | null;
      } | null;
      comments?: Array<{
        id: number;
        content?: string;
        commentType?: string;
        isDeleted?: boolean;
        publishedDate?: string;
        author?: { displayName?: string; uniqueName?: string };
      }>;
    }
    const t = await this.wrap(`read thread ${id} of pull request ${number}`, () =>
      this.api.json<Thread>("GET", withVersion(`${repoAPI(project, repo)}/pullRequests/${number}/threads/${id}`)),
    );
    const th: ReviewThread = { ref: String(id), resolved: false, comments: [] };
    if (["fixed", "closed", "bydesign", "wontfix"].includes((t.status ?? "").toLowerCase())) th.resolved = true;
    const tc = t.threadContext;
    if (tc) {
      th.path = stripSlash(tc.filePath ?? "");
      if (tc.rightFileStart) th.line = tc.rightFileStart.line;
      else if (tc.leftFileStart) th.line = tc.leftFileStart.line;
    }
    for (const cm of t.comments ?? []) {
      if (cm.isDeleted || (cm.commentType ?? "").toLowerCase() === "system") continue;
      const x: ThreadComment = {
        id: String(cm.id),
        author: cm.author?.uniqueName || cm.author?.displayName || "",
        body: cm.content ?? "",
        createdAt: "",
      };
      if (th.path !== undefined) x.path = th.path;
      if (th.line !== undefined) x.line = th.line;
      if (cm.publishedDate) {
        const d = new Date(cm.publishedDate);
        if (!Number.isNaN(d.getTime())) x.createdAt = d.toISOString();
      }
      th.comments.push(x);
    }
    return th;
  }

  /**
   * Reads the file at the merge base and at the head of the last iteration of
   * the pull request and makes the diff. A binary or large file gives
   * resource_not_found.
   */
  async fileDiff(project: string, repo: string, number: number, path: string): Promise<FileDiff> {
    const p = stripSlash(path.trim());
    if (p === "") throw new OfError("invalid_request", "azuredevops: a file path is required");
    const base = `${repoAPI(project, repo)}/pullRequests/${number}/iterations`;
    const its = await this.wrap(`read the iterations of pull request ${number}`, () =>
      this.api.json<{
        value?: Array<{ id: number; sourceRefCommit?: { commitId?: string }; commonRefCommit?: { commitId?: string } }>;
      }>("GET", withVersion(base)),
    );
    const last = its.value?.at(-1);
    if (!last) throw new OfError("resource_not_found", `azuredevops: pull request ${number} has no iteration`);
    const changes = await this.wrap(`read the changes of pull request ${number}`, () =>
      this.api.json<{
        changeEntries?: Array<{ changeType?: string; originalPath?: string; item?: { path?: string } }>;
      }>("GET", withVersion(`${base}/${last.id}/changes`, { $compareTo: "0", $top: "2000" })),
    );
    const entry = (changes.changeEntries ?? []).find((ch) => stripSlash(ch.item?.path ?? "") === p);
    if (!entry) throw new OfError("resource_not_found", `azuredevops: ${p} is not changed in pull request ${number}`);
    const kind = (entry.changeType ?? "").toLowerCase();
    const oldPath = stripSlash(entry.originalPath ?? "") || p;
    let before = "";
    let after = "";
    if (!kind.includes("add")) before = await this.fileAt(project, repo, oldPath, last.commonRefCommit?.commitId ?? "");
    if (!kind.includes("delete")) after = await this.fileAt(project, repo, p, last.sourceRefCommit?.commitId ?? "");
    const patch = unifiedDiff(before, after);
    if (patch === "") {
      throw new OfError("resource_not_found", `azuredevops: ${p} has no text change in pull request ${number}`);
    }
    return { path: p, patch, truncated: false };
  }

  /** Reads a text file at a commit. */
  private async fileAt(project: string, repo: string, path: string, commit: string): Promise<string> {
    const raw = await this.wrap(`read ${path} at ${commit}`, () =>
      this.api.bytes(
        "GET",
        withVersion(`${repoAPI(project, repo)}/items`, {
          path: "/" + stripSlash(path),
          "versionDescriptor.version": commit,
          "versionDescriptor.versionType": "commit",
          $format: "octetstream",
        }),
      ),
    );
    if (raw.byteLength > MAX_FILE_BYTES) {
      throw new OfError("resource_not_found", `azuredevops: ${path} is larger than ${MAX_FILE_BYTES} bytes`);
    }
    if (raw.includes(0)) throw new OfError("resource_not_found", `azuredevops: ${path} is a binary file`);
    return new TextDecoder().decode(raw);
  }

  /** Azure DevOps names a reviewer by the ID of the identity: this is the ID of the user of the token. */
  async login(): Promise<string> {
    const path = "/_apis/connectionData?connectOptions=none&lastChangeId=-1&lastChangeId64=-1&api-version=7.1-preview";
    const cd = await this.wrap("read the user of the token", () =>
      this.api.json<{ authenticatedUser?: { id?: string } }>("GET", path),
    );
    const id = (cd.authenticatedUser?.id ?? "").trim();
    if (id === "") throw new OfError("invalid_request", "azuredevops: the token has no user identity");
    return id;
  }

  /** Each login is the ID of an identity (see login). The reviewer is optional and has no vote. */
  async requestReviewers(project: string, repo: string, number: number, logins: string[]): Promise<void> {
    for (const raw of logins) {
      const id = raw.trim();
      if (id === "") throw new OfError("invalid_request", "azuredevops: a reviewer identity is required");
      await this.wrap(`request review of pull request ${number}`, () =>
        this.api.send(
          "PUT",
          withVersion(`${repoAPI(project, repo)}/pullRequests/${number}/reviewers/${pathEscape(id)}`),
          { id, vote: 0, isRequired: false },
        ),
      );
    }
  }

  // ---------- Work item tracker ----------

  private workItemPath(project: string, id: number, q?: Record<string, string>): string {
    return withVersion(`/${pathEscape(project)}/_apis/wit/workitems/${id}`, q);
  }

  private getWorkItem(project: string, id: number, expand: boolean): Promise<WorkItem> {
    return this.wrap(`get work item ${id}`, () =>
      this.api.json<WorkItem>("GET", this.workItemPath(project, id, expand ? { $expand: "relations" } : undefined)),
    );
  }

  private patchWorkItem(project: string, id: number, ops: Array<Record<string, unknown>>): Promise<void> {
    return this.wrap(`update work item ${id}`, () => this.api.send("PATCH", this.workItemPath(project, id), ops));
  }

  /**
   * Moves a work item to a generic state (in_progress, in_review or done). A
   * workflow without a review state leaves the work item in progress.
   */
  async setWorkItemState(project: string, id: number, state: string): Promise<void> {
    if (state !== "in_progress" && state !== "in_review" && state !== "done") {
      throw new OfError("invalid_request", `azuredevops: unknown work item state "${state}"`);
    }
    const w = await this.getWorkItem(project, id, false);
    const typ = String(w.fields?.["System.WorkItemType"] ?? "");
    const current = String(w.fields?.["System.State"] ?? "");
    if (typ === "") throw new OfError("internal_error", `azuredevops: work item ${id} has no type`);
    const list = await this.wrap(`list states of ${typ}`, () =>
      this.api.json<{ value?: TypeState[] }>(
        "GET",
        withVersion(`/${pathEscape(project)}/_apis/wit/workitemtypes/${pathEscape(typ)}/states`),
      ),
    );
    const target = pickState(state, list.value ?? [], current);
    if (target === "") return;
    await this.patchWorkItem(project, id, [{ op: "add", path: "/fields/System.State", value: target }]);
  }

  /** Adds an artifact link from the work item to a pull request. Does nothing when the link exists. */
  async attachPullRequestToWorkItem(project: string, id: number, pr: PullRequestRef): Promise<void> {
    const { owner, name } = splitRepo(pr.repo);
    const repoProject = owner || project;
    if (name === "" || !pr.number) {
      throw new OfError("invalid_request", "azuredevops: a pull request needs a repository and a number");
    }
    const r = await this.wrap(`get repository ${pr.repo}`, () =>
      this.api.json<Repository>("GET", withVersion(repoAPI(repoProject, name))),
    );
    if (!r.id || !r.project?.id) throw new OfError("internal_error", `azuredevops: repository ${pr.repo} has no ID`);
    // vstfs:///Git/PullRequestId/{project ID}%2F{repository ID}%2F{number}
    const link = `vstfs:///Git/PullRequestId/${r.project.id}%2F${r.id}%2F${pr.number}`;
    const w = await this.getWorkItem(project, id, true);
    for (const rel of w.relations ?? []) {
      if (rel.rel === "ArtifactLink" && rel.url.toLowerCase() === link.toLowerCase()) return;
    }
    await this.patchWorkItem(project, id, [
      {
        op: "add",
        path: "/relations/-",
        value: { rel: "ArtifactLink", url: link, attributes: { name: "Pull Request" } },
      },
    ]);
  }

  // Adds context to an error and keeps its code.
  private fail(what: string, e: unknown): unknown {
    if (e instanceof ApiError) {
      const o = fromApiError(e);
      return new OfError(o.code, `azuredevops: ${what}: ${o.detail}`, { cause: e });
    }
    if (e instanceof OfError) return new OfError(e.code, `azuredevops: ${what}: ${e.detail}`, { cause: e });
    if (e instanceof Error) return new Error(`azuredevops: ${what}: ${e.message}`, { cause: e });
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
