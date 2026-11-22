// Review operations on pull requests: threads, reviews, reviewers, diffs.
// Port of internal/forge/github/{review,reviewcontext,reviewers}.go.
import { OfError, type FileDiff, type ReviewThread, type ThreadComment } from "@openfactory/core";
import type { ReviewInput } from "../forge";
import { commentId, repoPath, wrap, type GitHubTransport } from "./rest";

const MAX_REVIEW_PAGES = 30;

const REVIEW_THREADS_QUERY = `query($owner:String!,$repo:String!,$number:Int!,$after:String){
  repository(owner:$owner,name:$repo){pullRequest(number:$number){
    reviewThreads(first:100,after:$after){
      pageInfo{hasNextPage endCursor}
      nodes{id isResolved comments(first:100){nodes{databaseId}}}
    }}}}`;

export async function replyInThread(
  t: GitHubTransport,
  owner: string,
  repo: string,
  number: number,
  threadRef: string,
  body: string,
): Promise<void> {
  const id = commentId(threadRef);
  try {
    await t.call(owner, repo, "POST", `${repoPath(owner, repo)}/pulls/${number}/comments/${id}/replies`, { body });
  } catch (e) {
    throw wrap(`github: reply in thread ${threadRef} of ${owner}/${repo}#${number}`, e);
  }
}

async function reviewThreadId(
  t: GitHubTransport,
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
    const data: Page = await t.graphql<Page>(owner, repo, REVIEW_THREADS_QUERY, { owner, repo, number, after });
    const rt = data?.repository?.pullRequest?.reviewThreads;
    for (const n of rt?.nodes ?? []) {
      if (n.comments.nodes.some((c) => c.databaseId === id)) return { nodeId: n.id, resolved: n.isResolved };
    }
    if (!rt?.pageInfo.hasNextPage) break;
    after = rt.pageInfo.endCursor;
  }
  throw new OfError("resource_not_found", `github: no review thread of ${owner}/${repo}#${number} has comment ${id}`);
}

export async function resolveThread(
  t: GitHubTransport,
  owner: string,
  repo: string,
  number: number,
  threadRef: string,
): Promise<void> {
  const id = commentId(threadRef);
  let th: { nodeId: string; resolved: boolean };
  try {
    th = await reviewThreadId(t, owner, repo, number, id);
  } catch (e) {
    throw wrap("github: resolve thread", e);
  }
  if (th.resolved) return;
  const m = "mutation($id:ID!){resolveReviewThread(input:{threadId:$id}){thread{id isResolved}}}";
  try {
    await t.graphql(owner, repo, m, { id: th.nodeId });
  } catch (e) {
    throw wrap(`github: resolve thread ${threadRef} of ${owner}/${repo}#${number}`, e);
  }
}

export async function submitReview(t: GitHubTransport, input: ReviewInput): Promise<void> {
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
    await t.call(
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

export async function markReady(t: GitHubTransport, owner: string, repo: string, number: number): Promise<void> {
  let pr: { node_id: string; draft: boolean };
  try {
    pr = await t.call(owner, repo, "GET", `${repoPath(owner, repo)}/pulls/${number}`);
  } catch (e) {
    throw wrap(`github: read ${owner}/${repo}#${number}`, e);
  }
  if (!pr.draft) return;
  const m = "mutation($id:ID!){markPullRequestReadyForReview(input:{pullRequestId:$id}){pullRequest{id isDraft}}}";
  try {
    await t.graphql(owner, repo, m, { id: pr.node_id });
  } catch (e) {
    throw wrap(`github: mark ${owner}/${repo}#${number} ready`, e);
  }
}

export async function requestReviewers(
  t: GitHubTransport,
  owner: string,
  repo: string,
  number: number,
  logins: string[],
): Promise<void> {
  if (logins.length === 0) return;
  try {
    await t.call(owner, repo, "POST", `${repoPath(owner, repo)}/pulls/${number}/requested_reviewers`, {
      reviewers: logins,
    });
  } catch (e) {
    throw wrap(`github: request review of ${owner}/${repo}#${number}`, e);
  }
}

/** The thread of the first comment threadRef: that comment and each reply to it. */
export async function reviewThread(
  t: GitHubTransport,
  owner: string,
  repo: string,
  number: number,
  threadRef: string,
): Promise<ReviewThread> {
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
        (await t.call<C[]>(
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
      const ms = x.created_at ? Date.parse(x.created_at) : NaN;
      if (Number.isFinite(ms)) tc.created_at = new Date(ms).toISOString().replace(".000Z", "Z");
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
export async function fileDiff(
  t: GitHubTransport,
  owner: string,
  repo: string,
  number: number,
  path: string,
): Promise<FileDiff> {
  const p = path.trim().replace(/^\//, "");
  if (p === "") throw new OfError("invalid_request", "github: a file path is required");
  for (let page = 1; page <= MAX_REVIEW_PAGES; page++) {
    let files: { filename: string; patch?: string }[];
    try {
      files =
        (await t.call(
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
