import { describe, expect, it } from "vitest";
import { toPluginError } from "@openfactory/plugin-api";
import { findCall, mockFetch } from "@openfactory/plugin-api/testing";
import { GitLabClient } from "./client";

const P = "/api/v4/projects/acme%2Fplatform%2Fapi";
const auth = (h: Headers) => h.get("PRIVATE-TOKEN") === "glpat";

function client(routes: Parameters<typeof mockFetch>[0], extra: { tokenType?: string } = {}) {
  const m = mockFetch(routes);
  return { c: new GitLabClient({ baseUrl: "https://gl.test", token: "glpat", fetch: m.fetch, ...extra }), calls: m.calls };
}

async function code(p: Promise<unknown>): Promise<string> {
  try {
    await p;
  } catch (e) {
    return toPluginError(e).code;
  }
  return "none";
}

describe("GitLabClient", () => {
  it("covers the forge operations", async () => {
    const { c, calls } = client({
      [`GET ${P}`]: () => ({ default_branch: "main" }),
      [`POST ${P}/merge_requests`]: () => ({ iid: 3, web_url: "https://gl/mr/3", state: "opened", draft: true, source_branch: "factory/x" }),
      [`GET ${P}/merge_requests/3`]: () => ({ iid: 3, state: "merged", source_branch: "factory/x" }),
      [`POST ${P}/merge_requests/3/notes`]: () => new Response("{}", { status: 201 }),
      [`POST ${P}/issues/4/notes`]: () => new Response("{}", { status: 201 }),
      [`POST ${P}/statuses/abc`]: () => new Response("{}", { status: 201 }),
      [`GET ${P}/repository/commits/main`]: () => ({ id: "sha1" }),
      [`GET ${P}/repository/tree`]: (call) => {
        expect(call.query.get("ref")).toBe("sha1");
        expect(call.query.get("path")).toBe("def");
        return [
          { path: "def/factory.yaml", type: "blob" },
          { path: "def/agents", type: "tree" },
        ];
      },
      [`GET ${P}/repository/files/def%2Ffactory.yaml/raw`]: () => "name: x\n",
    });
    const u = c.cloneUrl("acme/platform", "api");
    expect(u).toContain("oauth2:glpat@");
    expect(u.endsWith("/acme/platform/api.git")).toBe(true);

    const pr = await c.createPullRequest("acme/platform", "api", { head: "factory/x", base: "", title: "Fix", body: "", draft: true });
    expect(pr).toMatchObject({ number: 3, state: "draft", forge: "GITLAB", repo: "acme/platform/api", branch: "factory/x" });
    const mr = findCall(calls, `POST ${P}/merge_requests`)!;
    expect(mr.body).toMatchObject({ title: "Draft: Fix", target_branch: "main", source_branch: "factory/x" });
    expect(auth(mr.headers)).toBe(true);

    expect((await c.getPullRequest("acme/platform", "api", 3)).state).toBe("merged");
    await c.comment("acme/platform", "api", 3, "hi");
    await c.commentIssue("acme/platform", "api", 4, "hi");
    await c.setCommitStatus("acme/platform", "api", "abc", { context: "openfactory/definition", state: "error" });
    expect(findCall(calls, `POST ${P}/statuses/abc`)!.body).toMatchObject({ state: "failed", name: "openfactory/definition" });

    const got = await c.getFiles("acme/platform", "api", "main", "def");
    expect(got).toEqual({ files: { "factory.yaml": "name: x\n" }, commitSha: "sha1" });
  });

  it("uses a Bearer header for oauth tokens and normalizes the base URL", async () => {
    const { c, calls } = client({ [`GET ${P}`]: () => ({ default_branch: "main" }) }, { tokenType: "oauth" });
    expect(c.baseUrl).toBe("https://gl.test/api/v4");
    expect(c.webUrl).toBe("https://gl.test");
    await c.defaultBranch("acme/platform", "api");
    expect(calls[0]!.headers.get("Authorization")).toBe("Bearer glpat");
    expect(calls[0]!.headers.get("PRIVATE-TOKEN")).toBeNull();
  });

  it("maps API failures to error codes", async () => {
    const { c } = client({ [`GET ${P}/merge_requests/9`]: () => new Response("nope", { status: 404 }) });
    expect(await code(c.getPullRequest("acme/platform", "api", 9))).toBe("resource_not_found");
    const { c: c2 } = client({ [`POST ${P}/merge_requests/3/notes`]: () => new Response("busy", { status: 503 }) });
    expect(await code(c2.comment("acme/platform", "api", 3, "x"))).toBe("resource_unavailable");
  });

  it("requires a token", () => {
    expect(() => new GitLabClient({ token: "" })).toThrow(/token is required/);
  });
});

describe("review operations", () => {
  const mr = "/api/v4/projects/acme%2Fapi/merge_requests/3";
  const details = () => ({ title: "Draft: Fix it", draft: true, diff_refs: { base_sha: "b", head_sha: "h", start_sha: "s" } });

  function reviewClient() {
    return client({
      [`GET ${mr}`]: details,
      [`PUT ${mr}`]: () => ({}),
      [`POST ${mr}/discussions/d1/notes`]: () => ({}),
      [`PUT ${mr}/discussions/d1`]: () => ({}),
      [`POST ${mr}/discussions`]: () => ({}),
      [`POST ${mr}/notes`]: () => ({}),
      [`POST ${mr}/approve`]: () => ({}),
    });
  }

  it("replies, resolves, submits and marks ready", async () => {
    const { c, calls } = reviewClient();
    await c.replyInThread("acme", "api", 3, "d1", "done");
    expect(findCall(calls, `POST ${mr}/discussions/d1/notes`)!.body).toMatchObject({ body: "done" });
    await c.resolveThread("acme", "api", 3, "d1");
    expect(calls.some((x) => x.method === "PUT" && x.path === `${mr}/discussions/d1?resolved=true`)).toBe(true);

    await c.submitReview("acme", "api", 3, { event: "approve", body: "LGTM", comments: [{ path: "a.go", line: 7, body: "nit" }] });
    const pos = (findCall(calls, `POST ${mr}/discussions`)!.body as { position: Record<string, unknown> }).position;
    expect(pos).toMatchObject({ new_line: 7, head_sha: "h", base_sha: "b", start_sha: "s", new_path: "a.go" });
    expect(findCall(calls, `POST ${mr}/approve`)).toBeDefined();
    expect(findCall(calls, `POST ${mr}/notes`)!.body).toMatchObject({ body: "LGTM" });

    await c.markReady("acme", "api", 3);
    expect(findCall(calls, `PUT ${mr}`)!.body).toMatchObject({ title: "Fix it" });
  });

  it("uses old_line for the LEFT side", async () => {
    const { c, calls } = reviewClient();
    await c.submitReview("acme", "api", 3, { body: "", comments: [{ path: "a.go", line: 4, side: "LEFT", body: "x" }] });
    const pos = (findCall(calls, `POST ${mr}/discussions`)!.body as { position: Record<string, unknown> }).position;
    expect(pos["old_line"]).toBe(4);
    expect(pos["new_line"]).toBeUndefined();
  });

  it("rejects request_changes, unknown events and bad comments", async () => {
    const { c } = reviewClient();
    expect(await code(c.submitReview("acme", "api", 3, { event: "request_changes", body: "" }))).toBe("operation_not_supported");
    expect(await code(c.submitReview("acme", "api", 3, { event: "dance" as never, body: "" }))).toBe("invalid_request");
    expect(await code(c.submitReview("acme", "api", 3, { body: "", comments: [{ path: "", line: 1, body: "x" }] }))).toBe("invalid_request");
    expect(await code(c.replyInThread("acme", "api", 3, " ", "x"))).toBe("invalid_request");
  });

  it("does nothing to a merge request that is not a draft", async () => {
    const { c, calls } = client({ [`GET ${mr}`]: () => ({ title: "Fix", draft: false }) });
    await c.markReady("acme", "api", 3);
    expect(calls).toHaveLength(1);
  });
});

describe("review context", () => {
  const mr = "/api/v4/projects/acme%2Fapi/merge_requests/3";

  it("reads a thread and finds a diff on the second page", async () => {
    const { c } = client({
      [`GET ${mr}/discussions/d1`]: () => ({
        id: "d1",
        notes: [
          { id: 10, body: "rename this", created_at: "2026-01-02T03:04:05Z", resolvable: true, resolved: true, author: { username: "dana" }, position: { new_path: "pkg/x.go", new_line: 7 } },
          { id: 11, body: "assigned to someone", system: true, author: { username: "gitlab" } },
          { id: 12, body: "done", resolvable: true, resolved: true, author: { username: "bot" } },
        ],
      }),
      [`GET ${mr}/diffs`]: (call) => {
        if (call.query.get("page") === "1") {
          return new Response(JSON.stringify([{ old_path: "a.go", new_path: "a.go", diff: "@@ -1 +1 @@\n-a\n+b\n" }]), {
            headers: { "X-Next-Page": "2" },
          });
        }
        return [
          { old_path: "pkg/x.go", new_path: "pkg/x.go", diff: "@@ -7 +7 @@\n-old\n+new\n" },
          { old_path: "big.go", new_path: "big.go", diff: "", too_large: true },
        ];
      },
    });
    const th = await c.reviewThread("acme", "api", 3, "d1");
    expect(th).toMatchObject({ ref: "d1", path: "pkg/x.go", line: 7, resolved: true });
    expect(th.comments).toHaveLength(2);
    expect(th.comments[0]).toMatchObject({ id: "10", author: "dana", createdAt: "2026-01-02T03:04:05.000Z" });

    const d = await c.fileDiff("acme", "api", 3, "pkg/x.go");
    expect(d.patch).toContain("+new");
    expect(d.truncated).toBe(false);
    await expect(c.fileDiff("acme", "api", 3, "big.go")).rejects.toThrow(/too large/);
    expect(await code(c.fileDiff("acme", "api", 3, "big.go"))).toBe("resource_not_found");
    expect(await code(c.fileDiff("acme", "api", 3, "nope.go"))).toBe("resource_not_found");
    expect(await code(c.reviewThread("acme", "api", 3, ""))).toBe("invalid_request");
  });

  it("falls back to the changes call of GitLab before 15.7", async () => {
    const { c } = client({
      [`GET ${mr}/changes`]: () => ({ changes: [{ old_path: "a.go", new_path: "a.go", diff: "@@ -1 +1 @@\n-a\n+b\n" }] }),
    });
    expect((await c.fileDiff("acme", "api", 3, "a.go")).patch).toContain("+b");
  });

  it("adds reviewers to the present ones and finds users by name", async () => {
    let put: Record<string, unknown> | undefined;
    const { c } = client({
      "GET /api/v4/user": () => ({ id: 5, username: "alice" }),
      "GET /api/v4/users": (call) =>
        call.query.get("username") === "alice" ? [{ id: 5, username: "alice" }, { id: 6, username: "alice2" }] : [],
      [`GET ${mr}`]: () => ({ reviewers: [{ id: 9 }] }),
      [`PUT ${mr}`]: (call) => {
        put = call.body as Record<string, unknown>;
        return {};
      },
    });
    expect(await c.login()).toBe("alice");
    await c.requestReviewers("acme", "api", 3, ["alice"]);
    expect(put?.["reviewer_ids"]).toEqual([9, 5]);
    put = undefined;
    expect(await code(c.requestReviewers("acme", "api", 3, ["nobody"]))).toBe("resource_not_found");
    expect(put).toBeUndefined();
  });

  it("refuses a token without a username", async () => {
    const { c } = client({ "GET /api/v4/user": () => ({ id: 1 }) });
    expect(await code(c.login())).toBe("invalid_request");
  });
});
