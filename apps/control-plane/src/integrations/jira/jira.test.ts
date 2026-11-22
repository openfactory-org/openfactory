import { describe, expect, it } from "vitest";
import { signHS256 } from "../jwt";
import { NOW, integration, kit, post, user } from "../testkit";
import { hmacSha256Hex, toHex } from "../util";
import { adfMentions, adfText, markdownToAdf } from "./adf";
import { JiraHandler, browseUrl, pickTransition, qsh } from "./jira";
import { JiraNameResolver } from "./resolver";

const SECRETS = { JIRA_TOKEN: "api-token", JIRA_HOOK: "jsec", JIRA_SHARED: "shared" };
const URL_ = "https://of.example.com/integrations/jira/webhook";

function setup() {
  const k = kit(SECRETS);
  k.store.integrations = [
    integration({
      id: "int_j",
      provider: "jira",
      secret_ref: "JIRA_TOKEN",
      config: {
        base_url: "https://jira.test",
        site: "acme.atlassian.net",
        email: "bot@acme.com",
        bot_account_id: "bot-acc",
        webhook_secret_ref: "JIRA_HOOK",
        client_key: "ck-1",
        shared_secret_ref: "JIRA_SHARED",
      },
    }),
  ];
  k.store.users = [user({ id: "usr_1" })];
  k.store.identities.set("jira|acc-1", { user_id: "usr_1", provider: "jira", external_id: "acc-1", handle: "" });
  k.ff.on("/comment", () => new Response('{"id":"1"}', { status: 201 }));
  return { k, h: new JiraHandler(k.deps) };
}

const issue = (extra: object = {}) => ({
  id: "10001",
  key: "ENG-1",
  self: "https://acme.atlassian.net/rest/api/3/issue/10001",
  fields: {
    summary: "Fix login",
    description: "broken",
    labels: ["bug"],
    status: { id: "1", name: "To Do" },
    project: { id: "p", key: "ENG", name: "Engineering" },
    ...extra,
  },
});

const signed = async (body: object, id = "d-1") => {
  const text = JSON.stringify(body);
  return post(URL_, text, {
    "X-Hub-Signature": "sha256=" + (await hmacSha256Hex("jsec", text)),
    "X-Atlassian-Webhook-Identifier": id,
  });
};

describe("jira webhook", () => {
  it("rejects a bad signature, even for an unknown site", async () => {
    const { h } = setup();
    const out = await h.handleWebhook(
      post(URL_, JSON.stringify({ issue: issue() }), { "X-Hub-Signature": "sha256=00" }),
    );
    expect(out.response.status).toBe(401);
    const none = await h.handleWebhook(post(URL_, "{}"));
    expect(none.response.status).toBe(401);
  });

  it("ignores a delivery for a site without a record, answers 202", async () => {
    const { h } = setup();
    const out = await h.handleWebhook(
      await signed({
        webhookEvent: "jira:issue_created",
        issue: { ...issue(), self: "https://other.atlassian.net/x" },
      }),
    );
    // the record cannot verify: no record means 401
    expect(out.response.status).toBe(401);
  });

  it("publishes issue_created and issue_labeled in the background", async () => {
    const { k, h } = setup();
    const out = await h.handleWebhook(
      await signed({
        webhookEvent: "jira:issue_created",
        user: { accountId: "acc-1", displayName: "Al" },
        issue: issue(),
      }),
    );
    expect(out.response.status).toBe(202);
    expect(k.published).toHaveLength(0);
    await out.background!();
    expect(k.published.map((e) => e.type)).toEqual(["issue_created", "issue_labeled"]);
    const e = k.published[0]!;
    expect(e).toMatchObject({
      team_id: "team_1",
      provider: "jira",
      delivery_id: "d-1:issue_created",
      actor: { external_id: "acc-1", handle: "Al", user_id: "usr_1" },
      source: {
        kind: "issue",
        external_id: "ENG-1",
        url: "https://acme.atlassian.net/browse/ENG-1",
        workspace: "acme.atlassian.net",
      },
      text: "Fix login\n\nbroken",
    });
    expect(e.attributes).toMatchObject({
      project_key: "ENG",
      labels: ["bug"],
      added_labels: ["bug"],
      status_name: "To Do",
      actor_is_self: false,
    });
    expect(e.received_at).toBe(NOW.toISOString());
  });

  it("drops a duplicate delivery", async () => {
    const { h } = setup();
    await h.handleWebhook(await signed({ webhookEvent: "jira:issue_created", issue: issue() }, "dup"));
    const again = await h.handleWebhook(await signed({ webhookEvent: "jira:issue_created", issue: issue() }, "dup"));
    expect(again.response.status).toBe(200);
    expect(await again.response.json()).toEqual({ status: "duplicate" });
  });

  it("accepts a Connect JWT with the query-string hash", async () => {
    const { h } = setup();
    const body = JSON.stringify({ webhookEvent: "jira:issue_created", issue: issue() });
    const u = new URL(URL_ + "?b=2&a=1&a=0");
    const claims = {
      iss: "ck-1",
      exp: Math.floor(NOW.getTime() / 1000) + 60,
      qsh: await qsh("POST", u.pathname, u.searchParams),
    };
    const jwt = await signHS256("shared", claims);
    const ok = await h.handleWebhook(post(u.toString(), body, { Authorization: "JWT " + jwt }));
    expect(ok.response.status).toBe(202);
    const wrongQuery = await h.handleWebhook(post(URL_ + "?a=1", body, { Authorization: "JWT " + jwt }));
    expect(wrongQuery.response.status).toBe(401);
    const expired = await signHS256("shared", { ...claims, exp: Math.floor(NOW.getTime() / 1000) - 3600 });
    expect((await h.handleWebhook(post(u.toString(), body, { Authorization: "JWT " + expired }))).response.status).toBe(
      401,
    );
    const wrongIss = await signHS256("shared", { ...claims, iss: "other" });
    expect(
      (await h.handleWebhook(post(u.toString(), body, { Authorization: "JWT " + wrongIss }))).response.status,
    ).toBe(401);
  });

  it("qsh is the SHA-256 of the canonical request", async () => {
    const want = toHex(await crypto.subtle.digest("SHA-256", new TextEncoder().encode("GET&/p&a=1%2C2,3&b=x%20y")));
    expect(await qsh("get", "/p", new URLSearchParams("b=x y&a=3&a=1,2&jwt=zzz"))).toBe(want);
    const root = toHex(await crypto.subtle.digest("SHA-256", new TextEncoder().encode("GET&/&")));
    expect(await qsh("GET", "", new URLSearchParams())).toBe(root);
  });

  it("emits status_changed and issue_labeled from the changelog and ignores the bot's own activity", async () => {
    const { k, h } = setup();
    const rec = k.store.integrations[0]!;
    const body = JSON.stringify({
      webhookEvent: "jira:issue_updated",
      user: { accountId: "acc-1" },
      issue: issue(),
      changelog: {
        items: [
          { field: "labels", fromString: "bug", toString: "bug urgent" },
          { field: "status", to: "3", toString: "Done" },
        ],
      },
    });
    const evs = await h.normalize(rec, "", body);
    expect(evs.map((e) => e.type)).toEqual(["issue_labeled", "status_changed"]);
    expect(evs[0]!.attributes).toMatchObject({ added_labels: ["urgent"], labels: ["bug", "urgent"] });
    expect(evs[1]!.attributes).toMatchObject({ status_id: "3", status_name: "Done" });
    expect(evs[1]!.delivery_id).toBe("");
    expect(
      await h.normalize(
        rec,
        "",
        JSON.stringify({
          webhookEvent: "comment_created",
          user: { accountId: "bot-acc" },
          issue: issue(),
          comment: { id: "1", body: "x" },
        }),
      ),
    ).toEqual([]);
  });

  it("marks activity by another Jira bot account on this server as self", async () => {
    const { k, h } = setup();
    k.store.integrations.push(
      integration({ id: "int_2", provider: "jira", config: { site: "other.atlassian.net", bot_account_id: "acc-9" } }),
    );
    const evs = await h.normalize(
      k.store.integrations[0]!,
      "",
      JSON.stringify({ webhookEvent: "jira:issue_created", user: { accountId: "acc-9" }, issue: issue() }),
    );
    expect(evs[0]!.attributes).toMatchObject({ actor_is_self: true, actor_is_bot: true });
  });

  it("starts an agent session when a linked member mentions the bot", async () => {
    const { k, h } = setup();
    const comment = {
      type: "doc",
      version: 1,
      content: [
        {
          type: "paragraph",
          content: [
            { type: "mention", attrs: { id: "bot-acc", text: "@Factory" } },
            { type: "text", text: " do it" },
          ],
        },
      ],
    };
    const body = JSON.stringify({
      webhookEvent: "comment_created",
      comment: { id: "c1", body: comment, author: { accountId: "acc-1" } },
      issue: issue(),
    });
    const evs = await h.normalize(k.store.integrations[0]!, "d", body);
    expect(evs.map((e) => e.type)).toEqual(["comment_created", "agent_session_created"]);
    expect(evs[1]!.text).toBe("@Factory do it");
    expect(evs[1]!.attributes).toMatchObject({ linked: true });
    expect(evs[1]!.actor.user_id).toBe("usr_1");
    expect(k.ff.calls).toHaveLength(0);
  });

  it("asks an unlinked person to link, in a public comment without a link URL", async () => {
    const { k, h } = setup();
    const body = JSON.stringify({
      webhookEvent: "jira:issue_created",
      user: { accountId: "stranger" },
      issue: issue({ assignee: { accountId: "bot-acc" } }),
    });
    const evs = await h.normalize(k.store.integrations[0]!, "", body);
    expect(evs.map((e) => e.type)).toEqual(["issue_created", "issue_labeled"]);
    expect(k.ff.calls).toHaveLength(1);
    const c = k.ff.calls[0]!;
    expect(c.url).toBe("https://jira.test/rest/api/3/issue/ENG-1/comment");
    expect(c.headers.get("Authorization")).toBe("Basic " + btoa("bot@acme.com:api-token"));
    expect(c.body).toContain("connect your Jira account");
    expect(c.body).not.toContain("/integrations/link/");
  });
});

describe("jira responder and tracker", () => {
  const src = {
    provider: "jira",
    kind: "issue",
    external_id: "ENG-1",
    workspace: "acme.atlassian.net",
    url: "https://acme.atlassian.net/browse/ENG-1",
  };

  it("comments with an ADF body on the issue", async () => {
    const { k, h } = setup();
    await h.reply("team_1", {
      work_item_id: "wi",
      source: src,
      kind: "question",
      text: "Which one?",
      options: ["a", "b"],
    });
    expect(k.ff.calls[0]!.url).toBe("https://jira.test/rest/api/3/issue/ENG-1/comment");
    const body = k.ff.json(0) as { body: { type: string; content: unknown[] } };
    expect(body.body.type).toBe("doc");
    expect(JSON.stringify(body)).toContain("Question from the factory");
    await expect(
      h.reply("team_1", { work_item_id: "wi", source: { ...src, external_id: "" }, kind: "update", text: "x" }),
    ).rejects.toThrow(/no issue key/);
  });

  it("uses a Bearer token for an oauth record", async () => {
    const { k, h } = setup();
    k.store.integrations[0]!.config!["auth"] = "oauth";
    await h.reply("team_1", { work_item_id: "wi", source: src, kind: "update", text: "x" });
    expect(k.ff.calls[0]!.headers.get("Authorization")).toBe("Bearer api-token");
  });

  const T = (id: string, name: string, to: string, cat: string) => ({
    id,
    name,
    to: { name: to, statusCategory: { key: cat } },
  });

  it("picks transitions", () => {
    const ts = [
      T("1", "Start", "In Progress", "indeterminate"),
      T("2", "Review", "In Review", "indeterminate"),
      T("3", "Finish", "Closed", "done"),
      T("4", "Other", "Archived", "done"),
    ];
    expect(pickTransition(ts, "in_progress")).toBe("1");
    expect(pickTransition(ts, "in_review")).toBe("2");
    expect(pickTransition(ts, "done")).toBe("3");
    expect(
      pickTransition(
        ts.filter((t) => t.id !== "2"),
        "in_review",
      ),
    ).toBe("");
    expect(pickTransition([T("9", "x", "Y", "done")], "done")).toBe("9");
    expect(pickTransition(ts, "weird")).toBe("");
  });

  it("sets the issue state and attaches a pull request", async () => {
    const { k, h } = setup();
    k.ff
      .on("/transitions", () => new Response(null, { status: 204 }))
      .on(
        (c) => c.method === "GET" && c.url.endsWith("/transitions"),
        () => ({ transitions: [T("31", "Done", "Done", "done")] }),
      )
      .on("/remotelink", () => ({ id: 1 }));
    await h.setIssueState("team_1", src, "done");
    expect(k.ff.calls[1]!.method).toBe("POST");
    expect(k.ff.json(1)).toEqual({ transition: { id: "31" } });
    await h.attachPullRequest("team_1", src, {
      forge: "github",
      repo: "acme/api",
      number: 7,
      url: "https://github.com/acme/api/pull/7",
      branch: "b",
      state: "open",
    });
    expect(k.ff.json(2)).toEqual({
      globalId: "openfactory:pr:https://github.com/acme/api/pull/7",
      object: { url: "https://github.com/acme/api/pull/7", title: "Pull request acme/api#7" },
    });
    await expect(
      h.attachPullRequest("team_1", src, { forge: "github", repo: "r", number: 1, url: "", branch: "", state: "" }),
    ).rejects.toThrow(/no URL/);
  });

  it("does nothing when no transition fits", async () => {
    const { k, h } = setup();
    k.ff.on("/transitions", () => ({ transitions: [] }));
    await h.setIssueState("team_1", src, "in_review");
    expect(k.ff.calls).toHaveLength(1);
  });
});

describe("jira name resolver", () => {
  it("maps project names to keys and status names to IDs, paging projects", async () => {
    const { k } = setup();
    k.ff
      .on("startAt=0", () => ({ values: [{ key: "ENG", name: "Engineering" }], isLast: false }))
      .on("startAt=1", () => ({ values: [{ key: "OPS", name: "Ops" }], isLast: true }))
      .on("/rest/api/3/status", () => [{ id: "10", name: "Done" }]);
    const r = new JiraNameResolver(k.deps);
    expect(await r.resolveNames("team_1", "jira", "projects", ["engineering", "OPS", "nope"])).toEqual({
      engineering: ["ENG"],
      OPS: ["OPS"],
    });
    expect(await r.resolveNames("team_1", "jira", "statuses", ["done"])).toEqual({ done: ["10"] });
    await expect(r.resolveNames("team_1", "jira", "labels", [])).rejects.toThrow(/cannot resolve/);
    await expect(r.resolveNames("team_1", "slack", "projects", [])).rejects.toThrow(/cannot list/);
  });

  it("explains a refused credential and a missing integration", async () => {
    const { k } = setup();
    k.ff.on("/project/search", () => new Response("no", { status: 401 }));
    const r = new JiraNameResolver(k.deps);
    await expect(r.resolveNames("team_1", "jira", "projects", ["x"])).rejects.toThrow(/refused the credential/);
    k.store.integrations[0]!.status = "disabled";
    await expect(new JiraNameResolver(k.deps).resolveNames("team_1", "jira", "projects", ["x"])).rejects.toThrow(
      /no active Jira integration/,
    );
  });
});

describe("adf", () => {
  it("converts markdown to ADF", () => {
    const doc = markdownToAdf(
      "# Title\n\nHello **bold** and `code` and [x](https://e.com)\nnext\n\n- a\n- b\n\n1. one\n\n```go\nfmt.Println()\n```",
    );
    expect(doc.content!.map((n) => n.type)).toEqual(["heading", "paragraph", "bulletList", "orderedList", "codeBlock"]);
    expect(doc.content![0]).toMatchObject({ attrs: { level: 1 } });
    const para = doc.content![1]!.content!;
    expect(para.map((n) => n.type)).toEqual(["text", "text", "text", "text", "text", "text", "hardBreak", "text"]);
    expect(para[1]!.marks).toEqual([{ type: "strong" }]);
    expect(para[5]!.marks).toEqual([{ type: "link", attrs: { href: "https://e.com" } }]);
    expect(doc.content![4]).toMatchObject({ attrs: { language: "go" }, content: [{ text: "fmt.Println()" }] });
    expect(markdownToAdf("").content).toEqual([{ type: "paragraph" }]);
  });

  it("extracts text and mentions from ADF and wiki markup", () => {
    const d = {
      type: "doc",
      content: [
        {
          type: "paragraph",
          content: [
            { type: "mention", attrs: { id: "a1", text: "Al" } },
            { type: "hardBreak" },
            { type: "text", text: "hi" },
          ],
        },
        { type: "paragraph", content: [{ type: "text", text: "two" }] },
      ],
    };
    expect(adfText(d)).toBe("@Al\nhi\ntwo");
    expect(adfMentions(d)).toEqual(["a1"]);
    expect(adfText("plain [~accountid:x-1]")).toBe("plain [~accountid:x-1]");
    expect(adfMentions("see [~accountid:x-1] and [~accountid:y:2]")).toEqual(["x-1", "y:2"]);
    expect(adfText(null)).toBe("");
  });

  it("browseUrl", () => {
    expect(browseUrl("https://a.atlassian.net/rest/api/3/issue/1", "K-1")).toBe("https://a.atlassian.net/browse/K-1");
    expect(browseUrl("", "K-1")).toBe("");
  });
});
