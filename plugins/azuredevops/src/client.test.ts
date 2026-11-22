import { describe, expect, it } from "vitest";
import { toPluginError } from "@openfactory/plugin-api";
import { findCall, mockFetch } from "@openfactory/plugin-api/testing";
import { AzureDevOpsClient } from "./client";

const BASE = "/contoso/proj/_apis/git/repositories/web";
const wantAuth = "Basic " + btoa(":pat");

function client(routes: Parameters<typeof mockFetch>[0]) {
  const m = mockFetch(routes);
  return { c: new AzureDevOpsClient({ organization: "contoso", baseUrl: "https://ado.test", token: "pat", fetch: m.fetch }), calls: m.calls };
}

async function code(p: Promise<unknown>): Promise<string> {
  try {
    await p;
  } catch (e) {
    return toPluginError(e).code;
  }
  return "none";
}

describe("AzureDevOpsClient", () => {
  it("covers the forge operations", async () => {
    const { c, calls } = client({
      [`GET ${BASE}`]: () => ({ defaultBranch: "refs/heads/main" }),
      [`POST ${BASE}/pullrequests`]: () => ({
        pullRequestId: 11,
        status: "active",
        isDraft: true,
        sourceRefName: "refs/heads/factory/x",
        repository: { webUrl: "https://dev.azure.com/contoso/proj/_git/web" },
      }),
      [`GET ${BASE}/pullrequests/11`]: () => ({ pullRequestId: 11, status: "completed" }),
      [`POST ${BASE}/pullRequests/11/threads`]: () => ({}),
      [`POST ${BASE}/commits/abc/statuses`]: () => ({}),
      "POST /contoso/proj/_apis/wit/workItems/5/comments": () => ({}),
      [`GET ${BASE}/items`]: () => ({
        value: [
          { path: "/def", isFolder: true, gitObjectType: "tree", commitId: "c1" },
          { path: "/def/factory.yaml", gitObjectType: "blob", objectId: "o1", commitId: "c1" },
        ],
      }),
      [`GET ${BASE}/blobs/o1`]: () => "name: y\n",
    });
    expect(c.cloneUrl("proj", "web")).toBe("https://pat:pat@ado.test/contoso/proj/_git/web");

    const pr = await c.createPullRequest("proj", "web", { head: "factory/x", base: "", title: "T", body: "b", draft: true });
    expect(pr).toEqual({
      forge: "AZURE_DEVOPS",
      repo: "proj/web",
      number: 11,
      state: "draft",
      url: "https://dev.azure.com/contoso/proj/_git/web/pullrequest/11",
      branch: "factory/x",
    });
    const create = findCall(calls, `POST ${BASE}/pullrequests`)!;
    expect(create.body).toMatchObject({ targetRefName: "refs/heads/main", sourceRefName: "refs/heads/factory/x", isDraft: true });
    expect(create.headers.get("Authorization")).toBe(wantAuth);
    expect(create.query.get("api-version")).toBe("7.1");

    expect((await c.getPullRequest("proj", "web", 11)).state).toBe("merged");
    await c.comment("proj", "web", 11, "hi");
    expect(findCall(calls, `POST ${BASE}/pullRequests/11/threads`)!.body).toMatchObject({ status: 1 });
    await c.commentWorkItem("proj", 5, "hi");
    await c.setCommitStatus("proj", "web", "abc", { context: "openfactory/definition", state: "success" });
    expect(findCall(calls, `POST ${BASE}/commits/abc/statuses`)!.body).toMatchObject({
      state: "succeeded",
      context: { genre: "openfactory", name: "definition" },
    });
    expect(await c.getFiles("proj", "web", "main", "def")).toEqual({ files: { "factory.yaml": "name: y\n" }, commitSha: "c1" });
  });

  it("maps abandoned and plain open pull requests", async () => {
    const { c } = client({
      [`GET ${BASE}/pullrequests/1`]: () => ({ pullRequestId: 1, status: "abandoned" }),
      [`GET ${BASE}/pullrequests/2`]: () => ({ pullRequestId: 2, status: "active" }),
    });
    expect((await c.getPullRequest("proj", "web", 1)).state).toBe("closed");
    expect((await c.getPullRequest("proj", "web", 2)).state).toBe("open");
  });

  it("uses a commit version for a SHA ref and resolves the branch head when the listing has none", async () => {
    const sha = "a".repeat(40);
    const { c, calls } = client({
      [`GET ${BASE}/items`]: () => ({ value: [{ path: "/x/a.txt", gitObjectType: "blob", objectId: "o" }] }),
      [`GET ${BASE}/blobs/o`]: () => "A",
      [`GET ${BASE}/refs`]: () => ({ value: [{ objectId: "head1" }] }),
    });
    expect((await c.getFiles("proj", "web", sha, "x")).commitSha).toBe(sha);
    expect(calls[0]!.query.get("versionDescriptor.versionType")).toBe("commit");
    const r = await c.getFiles("proj", "web", "main", "x");
    expect(r).toEqual({ files: { "a.txt": "A" }, commitSha: "head1" });
  });

  it("requires a token and an organization", () => {
    expect(() => new AzureDevOpsClient({ organization: "o", token: "" })).toThrow(/token/);
    expect(() => new AzureDevOpsClient({ organization: "", token: "t" })).toThrow(/organization/);
  });

  it("maps API failures to error codes", async () => {
    const { c } = client({ [`GET ${BASE}/pullrequests/9`]: () => new Response("x", { status: 401 }) });
    expect(await code(c.getPullRequest("proj", "web", 9))).toBe("external_authentication_required");
  });
});

describe("review operations", () => {
  const prs = `${BASE}/pullRequests/11`;
  const routes = () => ({
    [`POST ${prs}/threads`]: () => ({}),
    [`POST ${prs}/threads/4/comments`]: () => ({}),
    [`PATCH ${prs}/threads/4`]: () => ({}),
    [`PATCH ${BASE}/pullrequests/11`]: () => ({}),
  });

  it("replies, resolves and marks ready", async () => {
    const { c, calls } = client(routes());
    await c.replyInThread("proj", "web", 11, "4", "done");
    expect(findCall(calls, `POST ${prs}/threads/4/comments`)!.body).toMatchObject({ parentCommentId: 1, content: "done" });
    await c.resolveThread("proj", "web", 11, "4");
    expect(findCall(calls, `PATCH ${prs}/threads/4`)!.body).toEqual({ status: "fixed" });
    await c.markReady("proj", "web", 11);
    expect(findCall(calls, `PATCH ${BASE}/pullrequests/11`)!.body).toEqual({ isDraft: false });
  });

  it("makes a thread of each inline comment and then one for the body", async () => {
    const { c, calls } = client(routes());
    await c.submitReview("proj", "web", 11, {
      body: "Summary",
      comments: [
        { path: "src/a.ts", line: 7, body: "nit" },
        { path: "/b.ts", line: 3, side: "LEFT", body: "old" },
      ],
    });
    const posts = calls.filter((x) => x.method === "POST");
    expect(posts).toHaveLength(3);
    expect(posts[0]!.body).toMatchObject({
      threadContext: { filePath: "/src/a.ts", rightFileStart: { line: 7, offset: 1 }, rightFileEnd: { line: 7, offset: 1 } },
    });
    expect(posts[1]!.body).toMatchObject({ threadContext: { filePath: "/b.ts", leftFileStart: { line: 3, offset: 1 } } });
    expect(posts[2]!.body).toMatchObject({ comments: [{ content: "Summary" }] });
  });

  it("rejects votes, unknown events, bad comments and bad thread references", async () => {
    const { c } = client(routes());
    expect(await code(c.submitReview("proj", "web", 11, { event: "approve", body: "" }))).toBe("operation_not_supported");
    expect(await code(c.submitReview("proj", "web", 11, { event: "request_changes", body: "" }))).toBe("operation_not_supported");
    expect(await code(c.submitReview("proj", "web", 11, { event: "x" as never, body: "" }))).toBe("invalid_request");
    expect(await code(c.submitReview("proj", "web", 11, { body: "", comments: [{ path: "a", line: 0, body: "x" }] }))).toBe("invalid_request");
    for (const ref of ["", "abc", "0", "-3", "1.5"]) {
      expect(await code(c.replyInThread("proj", "web", 11, ref, "x"))).toBe("invalid_request");
    }
  });
});

describe("review context", () => {
  const prs = `${BASE}/pullRequests/11`;

  it("reads a thread", async () => {
    const { c } = client({
      [`GET ${prs}/threads/4`]: () => ({
        id: 4,
        status: "fixed",
        threadContext: { filePath: "/src/a.ts", rightFileStart: { line: 9 } },
        comments: [
          { id: 1, content: "rename", publishedDate: "2026-01-02T03:04:05Z", author: { displayName: "Dana", uniqueName: "dana@contoso.com" } },
          { id: 2, content: "system", commentType: "system", author: {} },
          { id: 3, content: "gone", isDeleted: true, author: {} },
          { id: 4, content: "ok", author: { displayName: "Bob" } },
        ],
      }),
    });
    const th = await c.reviewThread("proj", "web", 11, "4");
    expect(th).toMatchObject({ ref: "4", path: "src/a.ts", line: 9, resolved: true });
    expect(th.comments.map((x) => [x.id, x.author])).toEqual([["1", "dana@contoso.com"], ["4", "Bob"]]);
    expect(th.comments[0]).toMatchObject({ path: "src/a.ts", line: 9, createdAt: "2026-01-02T03:04:05.000Z" });
  });

  const diffRoutes = (kind: string, before = "a\nb\nc\n", after = "a\nB\nc\n") => ({
    [`GET ${prs}/iterations`]: () => ({
      value: [
        { id: 1, sourceRefCommit: { commitId: "old" }, commonRefCommit: { commitId: "base" } },
        { id: 2, sourceRefCommit: { commitId: "head" }, commonRefCommit: { commitId: "base" } },
      ],
    }),
    [`GET ${prs}/iterations/2/changes`]: () => ({
      changeEntries: [{ changeType: kind, originalPath: "/src/a.ts", item: { path: "/src/a.ts" } }],
    }),
    [`GET ${BASE}/items`]: (call: { query: URLSearchParams }) =>
      call.query.get("versionDescriptor.version") === "base" ? before : after,
  });

  it("makes the diff of a file from the two commits", async () => {
    const { c, calls } = client(diffRoutes("edit"));
    const d = await c.fileDiff("proj", "web", 11, "src/a.ts");
    expect(d.patch).toBe("@@ -1,3 +1,3 @@\n a\n-b\n+B\n c\n");
    expect(calls.filter((x) => x.rawPath === `${BASE}/items`).map((x) => x.query.get("versionDescriptor.version"))).toEqual(["base", "head"]);
  });

  it("reads only the head of an added file and only the base of a deleted one", async () => {
    const add = client(diffRoutes("add", "", "x\n"));
    expect((await add.c.fileDiff("proj", "web", 11, "src/a.ts")).patch).toBe("@@ -0,0 +1 @@\n+x\n");
    expect(add.calls.filter((x) => x.rawPath === `${BASE}/items`)).toHaveLength(1);
    const del = client(diffRoutes("delete", "x\n", ""));
    expect((await del.c.fileDiff("proj", "web", 11, "src/a.ts")).patch).toBe("@@ -1 +0,0 @@\n-x\n");
  });

  it("fails with resource_not_found for an unchanged, binary or identical file", async () => {
    const { c } = client(diffRoutes("edit"));
    expect(await code(c.fileDiff("proj", "web", 11, "src/other.ts"))).toBe("resource_not_found");
    const bin = client(diffRoutes("edit", "a\u0000b", "c"));
    await expect(bin.c.fileDiff("proj", "web", 11, "src/a.ts")).rejects.toThrow(/binary/);
    expect(await code(bin.c.fileDiff("proj", "web", 11, "src/a.ts"))).toBe("resource_not_found");
    const same = client(diffRoutes("edit", "a\n", "a\n"));
    expect(await code(same.c.fileDiff("proj", "web", 11, "src/a.ts"))).toBe("resource_not_found");
    expect(await code(c.fileDiff("proj", "web", 11, "  "))).toBe("invalid_request");
  });

  it("fails for a pull request without an iteration", async () => {
    const { c } = client({ [`GET ${prs}/iterations`]: () => ({ value: [] }) });
    expect(await code(c.fileDiff("proj", "web", 11, "a"))).toBe("resource_not_found");
  });

  it("returns the identity of the token and requests reviewers by identity", async () => {
    const { c, calls } = client({
      "GET /contoso/_apis/connectionData": () => ({ authenticatedUser: { id: "guid-1" } }),
      [`PUT ${prs}/reviewers/guid-1`]: () => ({}),
    });
    expect(await c.login()).toBe("guid-1");
    await c.requestReviewers("proj", "web", 11, ["guid-1"]);
    expect(findCall(calls, `PUT ${prs}/reviewers/guid-1`)!.body).toEqual({ id: "guid-1", vote: 0, isRequired: false });
    expect(await code(c.requestReviewers("proj", "web", 11, [" "]))).toBe("invalid_request");
    const none = client({ "GET /contoso/_apis/connectionData": () => ({ authenticatedUser: {} }) });
    expect(await code(none.c.login())).toBe("invalid_request");
  });
});
