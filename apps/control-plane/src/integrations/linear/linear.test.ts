import { describe, expect, it } from "vitest";
import { NOW, integration, kit, post, user } from "../testkit";
import { hmacSha256Hex } from "../util";
import { LinearHandler, extractMentions, pickState, type WorkflowState } from "./linear";
import { LinearNameResolver } from "./resolver";

const URL_ = "https://of.example.com/integrations/linear/webhook";

function setup() {
  const k = kit({ LIN_TOKEN: "lin-key", LIN_HOOK: "hook-secret" });
  k.store.integrations = [
    integration({ id: "int_l", provider: "linear", secret_ref: "LIN_TOKEN", config: { organization_id: "org-1", app_user_id: "app-1", webhook_secret_ref: "LIN_HOOK" } }),
  ];
  k.store.users = [user({ id: "usr_1" })];
  k.store.identities.set("linear|u-1", { user_id: "usr_1", provider: "linear", external_id: "u-1", handle: "" });
  k.ff.on("graphql", () => ({ data: {} }));
  return { k, h: new LinearHandler(k.deps) };
}

const signed = async (body: object, headers: Record<string, string> = {}) => {
  const text = JSON.stringify(body);
  return post(URL_, text, { "Linear-Signature": await hmacSha256Hex("hook-secret", text), "Linear-Delivery": "dl-1", ...headers });
};

const base = { organizationId: "org-1", webhookTimestamp: NOW.getTime(), actor: { id: "u-1", type: "user", name: "Al" } };
const issueData = { id: "iss-1", identifier: "ENG-4", number: 4, title: "Crash", description: "see @x", url: "https://linear.app/a/issue/ENG-4", teamId: "t1", team: { id: "t1", name: "Eng" }, labels: [{ id: "l1", name: "bug" }], labelIds: ["l1"], assigneeId: "u-2", creatorId: "u-1", stateId: "s1", state: { id: "s1", name: "Todo" } };

describe("linear webhook", () => {
  it("rejects a missing or wrong signature and a stale timestamp", async () => {
    const { h } = setup();
    expect((await h.handleWebhook(post(URL_, JSON.stringify(base)))).response.status).toBe(401);
    expect((await h.handleWebhook(post(URL_, JSON.stringify(base), { "Linear-Signature": "00" }))).response.status).toBe(401);
    const stale = await h.handleWebhook(await signed({ ...base, webhookTimestamp: NOW.getTime() - 120_000 }));
    expect(stale.response.status).toBe(401);
    expect((await stale.response.json()) as { error: { code: string } }).toMatchObject({ error: { code: "stale_webhook" } });
  });

  it("ignores an unknown organization (after the signature check against nothing: 401)", async () => {
    const { h } = setup();
    expect((await h.handleWebhook(await signed({ ...base, organizationId: "other" }))).response.status).toBe(401);
  });

  it("publishes Issue create events and drops a duplicate", async () => {
    const { k, h } = setup();
    const out = await h.handleWebhook(await signed({ ...base, type: "Issue", action: "create", data: issueData }));
    expect(out.response.status).toBe(202);
    await out.background!();
    expect(k.published.map((e) => e.type)).toEqual(["issue_created", "issue_labeled", "issue_assigned"]);
    expect(k.published[0]).toMatchObject({
      delivery_id: "dl-1:issue_created",
      actor: { external_id: "u-1", user_id: "usr_1" },
      source: { kind: "issue", channel: "iss-1", external_id: "ENG-4", workspace: "org-1" },
    });
    expect(k.published[0]!.attributes).toMatchObject({ issue_number: 4, team_name: "Eng", state_name: "Todo", assignees: ["u-2"], actor_is_self: false });
    expect(k.published[1]!.attributes).toMatchObject({ added_labels: ["bug"] });
    expect((await (await h.handleWebhook(await signed({ ...base, type: "Issue", action: "create", data: issueData }))).response.json())).toEqual({ status: "duplicate" });
  });

  it("derives added labels, assignment and state change from updatedFrom", async () => {
    const { k, h } = setup();
    const evs = await h.normalize(k.store.integrations[0]!, "", JSON.stringify({ ...base, type: "Issue", action: "update", data: { ...issueData, labelIds: ["l1", "l2"], labels: [{ id: "l1", name: "bug" }, { id: "l2", name: "urgent" }] }, updatedFrom: { labelIds: ["l1"], stateId: "s0" } }));
    expect(evs.map((e) => e.type)).toEqual(["issue_labeled", "issue_state_changed"]);
    expect(evs[0]!.attributes).toMatchObject({ added_labels: ["urgent"] });
  });

  it("ignores activity of the app itself", async () => {
    const { k, h } = setup();
    const body = JSON.stringify({ ...base, actor: { id: "app-1", type: "app" }, type: "Issue", action: "create", data: issueData });
    expect(await h.normalize(k.store.integrations[0]!, "", body)).toEqual([]);
    const own = JSON.stringify({ ...base, actor: { id: "u-3" }, type: "Comment", action: "create", data: { id: "c", body: "x", issueId: "iss-1", userId: "app-1" } });
    expect(await h.normalize(k.store.integrations[0]!, "", own)).toEqual([]);
  });

  it("enriches a comment from the issue and extracts mentions", async () => {
    const { k, h } = setup();
    k.ff.on("graphql", () => ({ data: { issue: { id: "iss-1", identifier: "ENG-4", number: 4, title: "Crash", url: "https://linear.app/a/issue/ENG-4", team: { id: "t1", name: "Eng" }, labels: { nodes: [{ id: "l", name: "bug" }] } } } }));
    const body = JSON.stringify({ ...base, type: "Comment", action: "create", data: { id: "c1", body: "hi https://linear.app/acme/profiles/bot-user", issueId: "iss-1", userId: "u-1" } });
    const evs = await h.normalize(k.store.integrations[0]!, "", body);
    expect(evs).toHaveLength(1);
    expect(evs[0]!.type).toBe("comment_created");
    expect(evs[0]!.source).toMatchObject({ external_id: "ENG-4", channel: "iss-1" });
    expect(evs[0]!.attributes).toMatchObject({ labels: ["bug"], mentioned: ["bot-user"], comment_id: "c1", team_name: "Eng" });
    expect(evs[0]!.attributes).not.toHaveProperty("assignees");
    expect(k.ff.calls[0]!.headers.get("Authorization")).toBe("lin-key");
  });

  it("agent sessions need a linked member; otherwise the app asks for a link", async () => {
    const { k, h } = setup();
    const session = (creator: string) => JSON.stringify({ ...base, type: "AgentSessionEvent", action: "created", agentSession: { id: "as-1", issueId: "iss-1", issue: issueData, creatorId: creator, comment: { id: "c", body: "do it" } } });
    const none = await h.normalize(k.store.integrations[0]!, "", session("stranger"));
    expect(none).toEqual([]);
    const post_ = k.ff.calls.at(-1)!;
    expect(k.ff.json(k.ff.calls.length - 1)).toMatchObject({ variables: { input: { agentSessionId: "as-1", content: { type: "elicitation" } } } });
    expect(post_.body).not.toContain("/integrations/link/");
    const evs = await h.normalize(k.store.integrations[0]!, "d9", session("u-1"));
    expect(evs).toHaveLength(1);
    expect(evs[0]).toMatchObject({ type: "agent_session_created", text: "do it", delivery_id: "d9:agent_session_created", source: { kind: "agent_session", thread_id: "as-1" } });
    expect(evs[0]!.attributes).toMatchObject({ linked: true, session_id: "as-1" });
    const prompted = JSON.stringify({ ...base, type: "AgentSessionEvent", action: "prompted", agentSession: { id: "as-1", issueId: "iss-1", issue: issueData, creatorId: "u-1" }, agentActivity: { content: { body: "more" }, userId: "u-1" } });
    expect((await h.normalize(k.store.integrations[0]!, "", prompted))[0]).toMatchObject({ type: "agent_session_prompted", text: "more" });
  });

  it("extractMentions finds UUIDs and profile slugs", () => {
    const id = "123e4567-e89b-12d3-a456-426614174000";
    expect(extractMentions(`[@Al](mention:${id}) and @[Bo](${id.toUpperCase()}) https://linear.app/x/profiles/al.b`)).toEqual([id, "al.b"]);
  });
});

describe("linear responder and tracker", () => {
  const src = { provider: "linear", kind: "issue", channel: "iss-1", external_id: "ENG-4", workspace: "org-1" };

  it("comments on the issue", async () => {
    const { k, h } = setup();
    k.ff.on("commentCreate", () => ({ data: { commentCreate: { success: true } } }));
    await h.reply("team_1", { work_item_id: "wi", source: src, kind: "error", text: "boom" });
    expect(k.ff.json(0)).toMatchObject({ variables: { input: { issueId: "iss-1", body: "**The factory hit an error**\n\nboom" } } });
  });

  it("answers an agent session with activities by message kind", async () => {
    const { k, h } = setup();
    k.ff.on("agentActivityCreate", () => ({ data: { agentActivityCreate: { success: true } } }));
    const s = { ...src, kind: "agent_session", thread_id: "as-1" };
    for (const [kind, want] of [["update", "thought"], ["question", "elicitation"], ["approval_request", "elicitation"], ["error", "error"], ["handoff", "response"]] as const) {
      k.ff.calls.length = 0;
      await h.reply("team_1", { work_item_id: "wi", source: s, kind, text: "t" });
      expect(k.ff.json(0)).toMatchObject({ variables: { input: { agentSessionId: "as-1", content: { type: want } } } });
    }
  });

  it("reports a GraphQL error and a failed mutation", async () => {
    const { k, h } = setup();
    k.ff.on("commentCreate", () => ({ errors: [{ message: "denied" }, { message: "again" }] }));
    await expect(h.reply("team_1", { work_item_id: "wi", source: src, kind: "update", text: "x" })).rejects.toThrow("linear graphql: denied; again");
    const k2 = setup();
    k2.k.ff.on("commentCreate", () => ({ data: { commentCreate: { success: false } } }));
    await expect(k2.h.reply("team_1", { work_item_id: "wi", source: src, kind: "update", text: "x" })).rejects.toThrow("not successful");
    await expect(k2.h.reply("team_1", { work_item_id: "wi", source: { ...src, channel: "", external_id: "" }, kind: "update", text: "x" })).rejects.toThrow("no issue");
  });

  const S = (id: string, name: string, type: string, position: number): WorkflowState => ({ id, name, type, position });
  const states = [S("1", "Backlog", "backlog", 0), S("2", "Todo", "unstarted", 1), S("3", "In Progress", "started", 2), S("4", "In Review", "started", 3), S("5", "Done", "completed", 4), S("6", "Canceled", "canceled", 5)];

  it("picks the workflow state", () => {
    expect(pickState(states, states[1]!, "in_progress")).toBe("3");
    expect(pickState(states, states[2]!, "in_progress")).toBe("");
    expect(pickState(states, states[2]!, "in_review")).toBe("4");
    expect(pickState(states, states[3]!, "in_review")).toBe("");
    expect(pickState(states, states[1]!, "done")).toBe("5");
    expect(pickState(states, states[4]!, "done")).toBe("");
    expect(pickState(states.filter((s) => s.id !== "4"), states[1]!, "in_review")).toBe("3");
    expect(pickState(states.filter((s) => s.id !== "4"), states[2]!, "in_review")).toBe("");
  });

  it("moves the issue and attaches a pull request", async () => {
    const { k, h } = setup();
    k.ff
      .on("IssueStates", () => ({ data: { issue: { id: "iss-uuid", state: states[1], team: { states: { nodes: states } } } } }))
      .on("issueUpdate", () => ({ data: { issueUpdate: { success: true } } }))
      .on("attachmentCreate", () => ({ data: { attachmentCreate: { success: true } } }));
    await h.setIssueState("team_1", src, "done");
    expect(k.ff.json(1)).toMatchObject({ variables: { id: "iss-uuid", input: { stateId: "5" } } });
    await h.attachPullRequest("team_1", src, { forge: "github", repo: "acme/api", number: 7, url: "https://github.com/acme/api/pull/7", branch: "b", state: "open" });
    expect(k.ff.json(2)).toMatchObject({ variables: { input: { issueId: "iss-1", url: "https://github.com/acme/api/pull/7", title: "Pull request acme/api#7", subtitle: "open" } } });
  });
});

describe("linear name resolver", () => {
  it("resolves teams, scoped projects, states and issue numbers", async () => {
    const { k } = setup();
    k.ff
      .on("teams(first", () => ({ data: { teams: { nodes: [{ id: "t1", name: "Eng" }, { id: "t2", name: "Ops" }], pageInfo: { hasNextPage: false, endCursor: "" } } } }))
      .on("projects(first", () => ({ data: { projects: { nodes: [{ id: "p1", name: "Alpha", teams: { nodes: [{ id: "t1" }] } }, { id: "p2", name: "Beta", teams: { nodes: [{ id: "t2" }] } }], pageInfo: { hasNextPage: false, endCursor: "" } } } }))
      .on("workflowStates", () => ({ data: { workflowStates: { nodes: [{ id: "s1", name: "Done", team: { id: "t1" } }, { id: "s2", name: "Done", team: { id: "t2" } }], pageInfo: { hasNextPage: false, endCursor: "" } } } }))
      .on("issues(first", () => ({ data: { issues: { nodes: [{ id: "i1", team: { id: "t1" } }, { id: "i2", team: { id: "t2" } }] } } }));
    const r = new LinearNameResolver(k.deps);
    expect(await r.resolveNames("team_1", "linear", "teams", ["eng", "Ops"])).toEqual({ eng: ["t1"], Ops: ["t2"] });
    expect(await r.resolveNames("team_1", "linear", "projects", ["alpha", "beta"], { team_ids: ["t1"] })).toEqual({ alpha: ["p1"] });
    expect(await r.resolveNames("team_1", "linear", "states", ["done"])).toEqual({ done: ["s1", "s2"] });
    expect(await r.resolveNames("team_1", "linear", "issues", ["#42", "x"], { team_ids: ["t2"] })).toEqual({ "#42": ["i2"] });
    await expect(r.resolveNames("team_1", "linear", "cycles", [])).rejects.toThrow(/cannot resolve/);
  });

  it("explains an authentication error", async () => {
    const { k } = setup();
    k.ff.on("teams(first", () => ({ errors: [{ message: "Authentication required" }] }));
    await expect(new LinearNameResolver(k.deps).resolveNames("team_1", "linear", "teams", ["x"])).rejects.toThrow(/refused the credential/);
  });
});
