import { describe, expect, it } from "vitest";
import type { IntegrationRecord, PluginEvent } from "@openfactory/plugin-api";
import { FakeHostApi, webhookRequest } from "@openfactory/plugin-api/testing";
import { handleWebhook, normalize } from "./webhook";

const rec: IntegrationRecord = {
  id: "int_gl",
  teamId: "team_1",
  provider: "gitlab",
  name: "gitlab",
  config: { org: "acme", factory_bots: "payments-of-01k=payments", webhook_secret_ref: "GL_HOOK" },
};

function host(extra: Partial<ConstructorParameters<typeof FakeHostApi>[0]> = {}) {
  return new FakeHostApi({
    integrations: [rec],
    factories: [{ id: "fac_1", teamId: "team_1", alias: "payments", name: "Payments" }],
    secrets: { team_1: { GL_HOOK: "tok" } },
    ...extra,
  });
}

const project = { id: 1, path_with_namespace: "acme/platform/api" };
const alice = { id: 2, username: "alice" };

function post(h: FakeHostApi, event: string, token: string, payload: unknown, headers: Record<string, string> = {}) {
  return handleWebhook(
    webhookRequest("https://cp.test/plugins/gitlab/webhook", payload, {
      "X-Gitlab-Event": event,
      "X-Gitlab-Token": token,
      ...headers,
    }),
    h,
  );
}

const find = (evs: PluginEvent[], type: string) => evs.find((e) => e.type === type);

describe("handleWebhook", () => {
  it("rejects a delivery with the wrong token", async () => {
    const r = await post(host(), "Merge Request Hook", "nope", { object_kind: "merge_request", project });
    expect(r.status).toBe(401);
    expect(r.events).toEqual([]);
    expect(JSON.parse(r.body!).error.code).toBe("invalid_token");
  });

  it("rejects invalid JSON and a missing record", async () => {
    const bad = await handleWebhook(new Request("https://cp.test/x", { method: "POST", body: "{" }), host());
    expect(bad.status).toBe(400);
    const none = await post(new FakeHostApi({}), "Push Hook", "tok", { object_kind: "push", project });
    expect(none.status).toBe(401);
  });

  it("emits a merge request event", async () => {
    const r = await post(host(), "Merge Request Hook", "tok", {
      object_kind: "merge_request",
      user: alice,
      project,
      object_attributes: { iid: 3, title: "T", action: "open", target_branch: "main", source_branch: "f", url: "https://gl/mr/3" },
      labels: [{ title: "bug" }],
    });
    expect(r.status).toBe(202);
    expect(JSON.parse(r.body!)).toEqual({ status: "accepted", events: 1 });
    const mr = find(r.events, "merge_request")!;
    expect(mr.teamId).toBe("team_1");
    expect(mr.attributes).toMatchObject({
      action: "open",
      base_branch: "main",
      head_branch: "f",
      repo: "acme/platform/api",
      pr_number: 3,
      labels: ["bug"],
      actor_is_self: false,
    });
    expect(mr.source).toMatchObject({ provider: "gitlab", kind: "pull_request", repo: "acme/platform/api", number: 3 });
    expect(mr.actor).toMatchObject({ externalId: "2", handle: "alice", isBot: false });
    expect(mr.id).toMatch(/^evt_/);
  });

  it("routes a bot mention to the factory by alias", async () => {
    const r = await post(host(), "Note Hook", "tok", {
      object_kind: "note",
      user: alice,
      project,
      object_attributes: { note: "@payments-of-01k please review", noteable_type: "MergeRequest", action: "create" },
      merge_request: { iid: 3, title: "T" },
    });
    const bm = find(r.events, "bot_mentioned")!;
    expect(bm.factoryId).toBe("fac_1");
    expect(bm.attributes).toMatchObject({ mentioned: ["payments-of-01k"], alias: "payments" });
    expect(find(r.events, "pull_request_commented")).toBeDefined();
  });

  it("never turns the activity of a factory bot into work", async () => {
    const r = await post(host(), "Note Hook", "tok", {
      object_kind: "note",
      user: { id: 3, username: "payments-of-01k" },
      project,
      object_attributes: { note: "@payments-of-01k done", noteable_type: "MergeRequest" },
      merge_request: { iid: 3 },
    });
    expect(r.events.length).toBeGreaterThan(0);
    for (const ev of r.events) {
      expect(ev.type).not.toBe("bot_mentioned");
      expect(ev.attributes?.["actor_is_self"]).toBe(true);
    }
  });

  it("emits a push event", async () => {
    const r = await post(host(), "Push Hook", "tok", {
      object_kind: "push",
      ref: "refs/heads/main",
      after: "abc",
      user_username: "alice",
      project,
      commits: [{ added: ["x"] }],
    });
    expect(find(r.events, "push")!.attributes).toMatchObject({ branch: "main", paths: ["x"], head_sha: "abc" });
  });

  it("ignores a branch deletion and a note update", async () => {
    const del = await post(host(), "Push Hook", "tok", { object_kind: "push", ref: "refs/heads/x", after: "0000", project });
    expect(del.events).toEqual([]);
    const upd = await post(host(), "Note Hook", "tok", {
      object_kind: "note",
      project,
      object_attributes: { note: "x", noteable_type: "MergeRequest", action: "update" },
      merge_request: { iid: 3 },
    });
    expect(upd.events).toEqual([]);
  });

  it("answers 409 when records of two teams verify the delivery", async () => {
    const h = host({
      integrations: [rec, { ...rec, id: "int_gl2", teamId: "team_2" }],
      secrets: { team_1: { GL_HOOK: "tok" }, team_2: { GL_HOOK: "tok" } },
    });
    const r = await post(h, "Push Hook", "tok", { object_kind: "push", project });
    expect(r.status).toBe(409);
  });

  it("picks the team whose secret verifies, whatever the order", async () => {
    for (const order of [["team_1", "team_2"], ["team_2", "team_1"]]) {
      const recs = order.map((t) => ({ ...rec, id: `int_${t}`, teamId: t }));
      const h = new FakeHostApi({ integrations: recs, secrets: { team_1: { GL_HOOK: "s1" }, team_2: { GL_HOOK: "s2" } } });
      const r = await post(h, "Push Hook", "s2", { object_kind: "push", ref: "refs/heads/m", after: "abc", project });
      expect(r.status).toBe(202);
      expect(r.events[0]!.teamId).toBe("team_2");
    }
  });
});

describe("normalize", () => {
  const note = (type: string | null) => ({
    object_kind: "note",
    user: { id: 1, username: "dana" },
    project: { path_with_namespace: "acme/api" },
    object_attributes: { id: 9, note: "rename", noteable_type: "MergeRequest", discussion_id: "d1", type },
    merge_request: { iid: 3, title: "Fix", url: "https://gl/mr/3" },
  });

  it("gives a diff note a thread reference and a plain note none", () => {
    const rec1 = { ...rec, config: {} };
    const evs = normalize(rec1, "Note Hook", "x", JSON.stringify(note("DiffNote")));
    expect(evs[0]!.source?.threadRef).toBe("d1");
    expect(evs[0]!.attributes?.["thread_ref"]).toBe("d1");
    const plain = normalize(rec1, "Note Hook", "y", JSON.stringify(note(null)));
    expect(plain[0]!.source?.threadRef).toBeUndefined();
  });

  it("shares one action key between the events of a delivery", () => {
    const body = JSON.stringify({ ...note("DiffNote"), object_attributes: { ...note("DiffNote").object_attributes, note: "hi @payments" } });
    const r2 = { ...rec, config: { factory_bots: "payments=pay" } };
    const evs = normalize(r2, "Note Hook", "uuid-1", body);
    expect(evs.length).toBe(2);
    for (const ev of evs) {
      expect(ev.actionKey).toBe("gitlab:uuid-1");
      expect(ev.deliveryId).toBe(`uuid-1:${ev.type}`);
    }
    const none = normalize(r2, "Note Hook", "", body);
    expect(none[0]!.actionKey).toBeUndefined();
    expect(none[0]!.deliveryId).toBeUndefined();
  });

  it("derives the kind from the event header when object_kind is missing", () => {
    const body = JSON.stringify({ project: { path_with_namespace: "acme/api" }, ref: "refs/heads/m", after: "abc" });
    expect(normalize(rec, "Push Hook", "", body)[0]!.type).toBe("push");
  });

  it("maps issue and pipeline events", () => {
    const issue = normalize(rec, "Issue Hook", "", JSON.stringify({
      object_kind: "issue", project: { path_with_namespace: "acme/api" },
      object_attributes: { iid: 5, title: "Bug", description: "d", url: "u", action: "open" }, labels: [{ title: "x" }],
    }));
    expect(issue[0]).toMatchObject({ type: "issue", text: "Bug\n\nd", source: { kind: "issue", number: 5 } });
    const pl = normalize(rec, "Pipeline Hook", "", JSON.stringify({
      object_kind: "pipeline", project: { path_with_namespace: "acme/api" },
      object_attributes: { id: 7, ref: "main", sha: "s", status: "success", url: "u" }, merge_request: { iid: 3 },
    }));
    expect(pl[0]!.attributes).toMatchObject({ conclusion: "success", pr_number: 3, branch: "main" });
    expect(pl[0]!.source?.externalId).toBe("acme/api/pipeline/7");
  });
});
