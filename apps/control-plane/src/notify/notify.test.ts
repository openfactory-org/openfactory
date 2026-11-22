// Ports of internal/notify notify_test.go and internal/mergeguard and
// internal/eventctx tests.
import { describe, expect, it } from "vitest";
import { OfError, type Event, type OutboundMessage } from "@openfactory/core";
import { Router, normalizeProvider, permanent, routes, teamOfOrigin } from "./index";
import type { Responder } from "../forges/registry";
import { ApiError } from "../forges/util/http";
import { isMergeTool, parse, wrap, words, REFUSAL_TEXT } from "../mergeguard/index";
import * as ec from "../eventctx/index";
import { FakeResolver } from "../../test/fakes/forge";

const msg = (provider: string, kind = "update", extra: Partial<OutboundMessage> = {}): OutboundMessage => ({
  work_item_id: "wi_1",
  source: { provider, kind: "issue", repo: "acme/api", number: 1 },
  text: "hi",
  kind,
  ...extra,
});

class Rec implements Responder {
  calls: string[] = [];
  constructor(
    private readonly p: string,
    private readonly fail?: Error,
    tracker = false,
  ) {
    if (tracker) {
      this.setIssueState = async (_t, _i, s) => void this.calls.push("state:" + s);
    }
  }
  provider() {
    return this.p;
  }
  async reply(teamId: string, m: OutboundMessage) {
    this.calls.push(`reply:${teamId}:${m.kind}`);
    if (this.fail) throw this.fail;
  }
  setIssueState?: Responder["setIssueState"];
}

describe("notify router", () => {
  it("routes by normalized provider and skips silent ones", async () => {
    const teams = new Rec("microsoft_teams");
    const r = new Router([teams, new Rec("github")]);
    expect(r.providers()).toEqual(["github", "teams"]);
    expect(await r.deliverOnce("t1", msg("msteams"))).toBe("delivered");
    expect(teams.calls).toEqual(["reply:t1:update"]);
    expect(await r.deliverOnce("t1", msg("dashboard"))).toBe("skipped");
    expect(normalizeProvider("Azure-DevOps")).toBe("azure_devops");
  });
  it("tracker write-backs go to trackers only", async () => {
    const lin = new Rec("linear", undefined, true);
    const gh = new Rec("github");
    const r = new Router([lin, gh]);
    expect(await r.deliverOnce("t", msg("linear", "issue_state", { issue_state: "done" }))).toBe("delivered");
    expect(lin.calls).toEqual(["state:done"]);
    expect(await r.deliverOnce("t", msg("github", "issue_state", { issue_state: "done" }))).toBe("skipped");
    expect(gh.calls).toEqual([]);
    await expect(
      new Router([new Rec("jira", undefined, true)]).deliverOnce("t", msg("jira", "attach_pull_request")),
    ).resolves.toBe("skipped");
  });
  it("classifies errors", async () => {
    expect(permanent(new OfError("resource_not_found"))).toBe(true);
    expect(permanent(new OfError("resource_unavailable"))).toBe(false);
    expect(permanent(new ApiError("POST", "u", 503, ""))).toBe(false);
    expect(permanent(new ApiError("POST", "u", 422, ""))).toBe(true);
    expect(permanent(new Error("x", { cause: new ApiError("GET", "u", 404, "") }))).toBe(true);
    expect(permanent(new Error("network"))).toBe(false);
    await expect(new Router([new Rec("github", new Error("boom"))]).deliverOnce("t", msg("github"))).rejects.toThrow(
      "boom",
    );
    expect(teamOfOrigin("team:acme|work:w123")).toBe("acme");
  });
  it("lists notification routes", () => {
    const rs = routes({ slack: { channel: "C1" }, jira: null }, ["github", "linear", "gitlab"], ["slack", "github"]);
    expect(rs.map((r) => [r.provider, r.kind, r.deliverable])).toEqual([
      ["dashboard", "inbox", true],
      ["jira", "tracker", false],
      ["slack", "chat", true],
      ["github", "forge", true],
      ["gitlab", "forge", false],
    ]);
    expect(rs[2]!.settings).toEqual({ channel: "C1" });
  });
});

describe("mergeguard", () => {
  it("merge tool names (mergeguard_test.go)", () => {
    const merge = [
      "merge_pull_request",
      "mergePullRequest",
      "forge.merge-pr",
      "merge_pr",
      "merge",
      "merge_merge_request",
      "accept_merge_request",
      "acceptMergeRequest",
      "complete_pull_request",
      "pull_request_merge",
      "pr_merge",
      "squash_merge",
      "enable_auto_merge",
      "enable_pull_request_automerge",
      "set_auto_merge",
      "merge_branch",
      "mcp__forge__merge_pull_request",
      "MergePR",
      "merge_and_close",
      "add_to_merge_queue",
      "enqueue_pull_request",
      "merge_into",
      "rebase_merge",
    ];
    for (const n of merge) expect(isMergeTool(n), n).toBe(true);
    const other = [
      "get_merge_request",
      "list_merge_requests",
      "create_merge_request",
      "create_pull_request",
      "get_pull_request",
      "merge_request_diff",
      "get_merge_status",
      "list_merges_in_queue_status",
      "get_merge_base",
      "linear_get_issue",
      "comment_on_merge_request",
      "update_merge_request",
      "emerge",
      "submerged_items",
      "",
      "merge_request",
    ];
    for (const n of other) expect(isMergeTool(n), n).toBe(false);
    expect(words("getPR2Merge_now")).toEqual(["get", "pr2", "merge", "now"]);
  });
  it("wraps a handler", async () => {
    const seen: string[] = [];
    const records: string[] = [];
    let allow = false;
    const h = wrap(
      async (r) => {
        seen.push(await r.text());
        return new Response("ok");
      },
      { allow: () => allow, record: (_r, t, ok) => records.push(`${t}:${ok}`) },
    );
    const post = (b: string) => h(new Request("https://cp.test/mcp", { method: "POST", body: b }));
    await post('{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_pull_request"}}');
    expect(seen.length).toBe(1);
    const r = await post('{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"merge_pull_request"}}');
    expect(r.headers.get("x-of-merge-refused")).toBe("true");
    expect(await r.json()).toEqual({
      jsonrpc: "2.0",
      id: 2,
      result: { content: [{ type: "text", text: REFUSAL_TEXT }], isError: true },
    });
    const b = await post(
      '[{"jsonrpc":"2.0","id":3,"method":"tools/list"},{"jsonrpc":"2.0","method":"tools/call","params":{"name":"merge"}},{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"merge"}}]',
    );
    const arr = (await b.json()) as { id: number; error?: unknown; result?: unknown }[];
    expect(arr.map((x) => [x.id, !!x.error, !!x.result])).toEqual([
      [3, true, false],
      [4, false, true],
    ]);
    const n = await post('{"jsonrpc":"2.0","method":"tools/call","params":{"name":"merge"}}');
    expect(n.status).toBe(202);
    allow = true;
    await post('{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"merge"}}');
    expect(seen.length).toBe(2);
    expect(records).toEqual(["merge_pull_request:false", "merge:false", "merge:false", "merge:false", "merge:true"]);
    expect(parse("not json").calls).toEqual([]);
  });
});

const event = (extra: Partial<Event> = {}): Event => ({
  id: "evt_1",
  team_id: "t1",
  factory_id: "f1",
  provider: "webhook",
  type: "received",
  delivery_id: "d1",
  actor: { provider: "webhook", external_id: "whk_1", handle: "deploys", is_bot: false },
  source: { provider: "webhook", kind: "delivery", title: "Deploy" },
  attributes: { webhook_id: "whk_1", webhook_name: "deploys" },
  payload: { a: 1 },
  received_at: "2026-01-01T00:00:00Z",
  ...extra,
});

describe("eventctx", () => {
  const text = (f: ec.File) => new TextDecoder().decode(f.data);
  it("builds the envelope, payload and prompt section", async () => {
    const b = (await ec.build("msg/1", event(), true))!;
    expect(b.files.map((f) => f.path)).toEqual([
      ".openfactory/events/msg_1/envelope.json",
      ".openfactory/events/msg_1/payload.json",
    ]);
    const env = JSON.parse(text(b.files[0]!)) as ec.Envelope;
    expect(env).toMatchObject({
      message_id: "msg/1",
      event_types: ["received"],
      webhook: { id: "whk_1", name: "deploys", delivery: "d1" },
      payload_bytes: 7,
    });
    const p = ec.promptSection(b);
    expect(p).toContain("`.openfactory/events/msg_1/`");
    expect(p).toContain("- Webhook: deploys, delivery d1");
    expect(ec.decode(ec.encode(b))).toEqual(b);
  });
  it("cuts a long payload and limits context files", async () => {
    const big = "é".repeat(200_000);
    const builder: ec.Builder = {
      provider: () => "webhook",
      eventContext: async () => ({
        files: [
          { path: "../x/../ok.txt", data: new Uint8Array(3) },
          { path: "..", data: new Uint8Array(1) },
          ...Array.from({ length: 41 }, (_, i) => ({ path: `f${i}`, data: new Uint8Array(1) })),
        ],
        omitted: [{ name: "thread", reason: "private" }],
      }),
    };
    const b = (await ec.build("m", event({ payload: big }), true, [builder]))!;
    expect(b.envelope.payload_truncated).toBe(true);
    expect(b.files[1]!.data.length).toBeLessThanOrEqual(ec.MAX_PAYLOAD_BYTES);
    expect(b.envelope.context_files!.length).toBe(40);
    // A path that climbs above its root is refused, not re-rooted.
    expect(b.envelope.context_files![0]).toBe("context/f0");
    expect(b.envelope.omitted!.map((o) => o.reason)).toEqual([
      "private",
      "bad file name",
      "bad file name",
      "the context size limit was reached",
    ]);
  });
  it("payload builders: github review comment and jira issue", async () => {
    const gh = (await ec.build(
      "m",
      event({
        provider: "github",
        payload: { comment: { body: "nit", path: "a.go", diff_hunk: "@@ -1 +1 @@", line: 3, user: { login: "alex" } } },
      }),
      true,
    ))!;
    expect(gh.envelope.context_files).toEqual(["context/review_comment.json", "context/diff.patch"]);
    const jira = (await ec.build(
      "m",
      event({
        provider: "jira",
        source: { provider: "jira", kind: "issue", url: "https://j/ENG-1" },
        payload: {
          issue: {
            key: "ENG-1",
            fields: {
              summary: "S",
              description: {
                type: "doc",
                content: [{ type: "paragraph", content: [{ type: "text", text: "Hello" }] }],
              },
              labels: [],
            },
          },
        },
      }),
      true,
    ))!;
    const issue = JSON.parse(text(jira.files.find((f) => f.path.endsWith("issue.json"))!)) as Record<string, unknown>;
    expect(issue).toEqual({ key: "ENG-1", title: "S", description: "Hello", url: "https://j/ENG-1" });
  });
  it("review builder reads the thread and the diff", async () => {
    const fr = new FakeResolver();
    const f = fr.forgeFor("GITHUB");
    f.threads["10"] = { ref: "10", path: "a.go", comments: [{ id: "10", body: "x" }] };
    f.diffs["a.go"] = { path: "a.go", patch: "@@ -1 +1 @@\n-a\n+b" };
    const ev = event({
      provider: "github",
      payload: undefined,
      source: { provider: "github", kind: "pull_request", repo: "acme/api", number: 7, thread_ref: "10" },
    });
    const b = (await ec.build("m", ev, true, ec.reviewBuilders(fr)))!;
    expect(b.envelope.context_files).toEqual(["context/review_thread.json", "context/file_diff.patch"]);
    expect(fr.lookups[0]).toMatchObject({ codeForge: "GITHUB", repo: "acme/api", scope: ["acme/api"] });
    f.unsupported.add("fileDiff");
    const b2 = (await ec.build("m", ev, true, ec.reviewBuilders(fr)))!;
    expect(b2.envelope.omitted).toEqual([
      { name: "context/file_diff.patch", reason: "this forge cannot read the diff of a.go" },
    ]);
  });
  it("thread and diff limits", () => {
    const comments = Array.from({ length: 5 }, (_, i) => ({ id: String(i), body: "x".repeat(100_000) }));
    const { data, dropped } = ec.threadFile({ ref: "1", comments });
    expect(data.length).toBeLessThanOrEqual(ec.MAX_REVIEW_FILE_BYTES);
    expect(dropped).toBe(3);
    const d = ec.diffFile("a", "line\n".repeat(60_000));
    expect(d.left).toBeGreaterThan(0);
    expect(new TextDecoder().decode(d.data).endsWith("\n")).toBe(true);
  });
  it("stores bundles in R2 by hash", async () => {
    const store = new Map<string, string>();
    const bucket = {
      head: async (k: string) => (store.has(k) ? {} : null),
      put: async (k: string, v: string) => void store.set(k, v),
      get: async (k: string) => (store.has(k) ? { text: async () => store.get(k)! } : null),
      delete: async (ks: string[]) => ks.forEach((k) => store.delete(k)),
    } as unknown as R2Bucket;
    const b = (await ec.build("m", event(), false))!;
    const ref = await ec.storeBundle(bucket, b);
    expect(ref).toMatch(/^eventctx\/[0-9a-f]{64}$/);
    expect(await ec.storeBundle(bucket, b)).toBe(ref);
    expect(await ec.loadBundle(bucket, ref)).toEqual(b);
  });
});
