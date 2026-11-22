import { describe, expect, it } from "vitest";
import type { IntegrationRecord, PluginEvent, RepoRef } from "@openfactory/plugin-api";
import { FakeHostApi, type FakeFactory } from "@openfactory/plugin-api/testing";
import { basicAuth, extractMentions, handleWebhook, orgFromBaseUrl, projectCovers } from "./webhook";

const FACTORY_IDENTITY = "22222222-2222-4222-8222-222222222222";
const REPO_ID = "11111111-1111-4111-8111-111111111111";

const baseConfig = {
  org: "contoso",
  hook_username: "of",
  hook_password_ref: "ADO_HOOK",
  factory_identities: `${FACTORY_IDENTITY}=payments`,
};

const rec = (over: Partial<IntegrationRecord> = {}): IntegrationRecord => ({
  id: "int_ado",
  teamId: "team_1",
  provider: "azure_devops",
  name: "ado",
  config: baseConfig,
  ...over,
});

const containers = { account: { baseUrl: "https://dev.azure.com/contoso/" } };

function req(user: string, pass: string, payload: unknown): Request {
  return new Request("https://cp.test/plugins/azuredevops/webhook", {
    method: "POST",
    headers: { "content-type": "application/json", Authorization: "Basic " + btoa(`${user}:${pass}`) },
    body: JSON.stringify({ resourceContainers: containers, ...(payload as object) }),
  });
}

const hostWith = (o: { records?: IntegrationRecord[]; factories?: FakeFactory[]; repos?: Record<string, RepoRef[]> } = {}) =>
  new FakeHostApi({
    integrations: o.records ?? [rec()],
    factories: o.factories ?? [{ id: "fac_1", teamId: "team_1", alias: "payments", name: "Payments" }],
    secrets: { team_1: { ADO_HOOK: "pw" } },
    ...(o.repos ? { repos: o.repos } : {}),
  });

const pr = {
  pullRequestId: 11,
  status: "active",
  title: "T",
  targetRefName: "refs/heads/main",
  sourceRefName: "refs/heads/f",
  createdBy: { id: "u1", uniqueName: "alice@contoso.com" },
  repository: { id: REPO_ID, name: "web", project: { name: "proj" } },
};

const find = (evs: PluginEvent[], type: string) => evs.find((e) => e.type === type);

describe("handleWebhook", () => {
  it("rejects wrong credentials with a Basic challenge", async () => {
    const r = await handleWebhook(req("of", "bad", { eventType: "git.pullrequest.created", resource: pr }), hostWith());
    expect(r.status).toBe(401);
    expect(r.headers?.["WWW-Authenticate"]).toContain("Basic");
    expect(r.events).toEqual([]);
    const wrongUser = await handleWebhook(req("other", "pw", { eventType: "git.pullrequest.created", resource: pr }), hostWith());
    expect(wrongUser.status).toBe(401);
    const none = await handleWebhook(
      new Request("https://cp.test/x", { method: "POST", body: JSON.stringify({ resourceContainers: containers }) }),
      hostWith(),
    );
    expect(none.status).toBe(401);
  });

  it("rejects invalid JSON", async () => {
    const r = await handleWebhook(new Request("https://cp.test/x", { method: "POST", body: "nope" }), hostWith());
    expect(r.status).toBe(400);
  });

  it("emits a pull request event with the repository ID", async () => {
    const r = await handleWebhook(req("of", "pw", { id: "n1", eventType: "git.pullrequest.created", resource: pr }), hostWith());
    expect(r.status).toBe(202);
    const ev = find(r.events, "pull_request_created")!;
    expect(ev.attributes).toMatchObject({ repo: REPO_ID, base_branch: "main", head_branch: "f", pr_number: 11, author: "alice@contoso.com" });
    expect(ev.source).toMatchObject({ repo: "proj/web", kind: "pull_request", number: 11 });
    expect(ev.deliveryId).toBe("n1:pull_request_created");
    expect(ev.actionKey).toBe("azure_devops:n1");
    expect(ev.teamId).toBe("team_1");
  });

  it("routes a mention of a factory identity to the factory", async () => {
    const comment = {
      comment: { content: `@<${FACTORY_IDENTITY.toUpperCase()}> please review`, author: { id: "u1", uniqueName: "alice@contoso.com" } },
      pullRequest: pr,
    };
    const r = await handleWebhook(
      req("of", "pw", { id: "n2", eventType: "ms.vss-code.git-pullrequest-comment-event", resource: comment }),
      hostWith(),
    );
    const m = find(r.events, "pull_request_mentioned")!;
    expect(m.factoryId).toBe("fac_1");
    expect(m.attributes).toMatchObject({ mentioned: [FACTORY_IDENTITY], alias: "payments" });
    expect(find(r.events, "pull_request_commented")).toBeDefined();
  });

  it("gives the two deliveries of one merge the same delivery ID and action key", async () => {
    const done = { ...pr, status: "completed" };
    const a = await handleWebhook(req("of", "pw", { id: "n3", eventType: "git.pullrequest.updated", resource: done }), hostWith());
    const b = await handleWebhook(req("of", "pw", { id: "n4", eventType: "git.pullrequest.merged", resource: done }), hostWith());
    const ea = find(a.events, "pull_request_merged")!;
    const eb = find(b.events, "pull_request_merged")!;
    expect(ea.deliveryId).toBe(`${REPO_ID}/11:merged`);
    expect(eb.deliveryId).toBe(ea.deliveryId);
    expect(eb.actionKey).toBe(ea.actionKey);
    // A merge attempt that did not complete gives no event.
    const attempt = await handleWebhook(req("of", "pw", { id: "n5", eventType: "git.pullrequest.merged", resource: pr }), hostWith());
    expect(attempt.events).toEqual([]);
  });

  it("maps abandoned and updated pull requests", async () => {
    const ab = await handleWebhook(req("of", "pw", { id: "a", eventType: "git.pullrequest.updated", resource: { ...pr, status: "abandoned" } }), hostWith());
    expect(ab.events[0]!.type).toBe("pull_request_closed");
    const up = await handleWebhook(req("of", "pw", { id: "b", eventType: "git.pullrequest.updated", resource: pr }), hostWith());
    expect(up.events[0]!.type).toBe("pull_request_updated");
  });

  it("emits work item assigned and labeled events", async () => {
    const wi = {
      workItemId: 5,
      fields: {
        "System.AssignedTo": { oldValue: null, newValue: { uniqueName: "factory@contoso.com" } },
        "System.Tags": { oldValue: "a", newValue: "a; factory-ready" },
      },
      revisedBy: { id: "u1", uniqueName: "alice@contoso.com" },
      revision: {
        fields: {
          "System.TeamProject": "proj",
          "System.Title": "Bug",
          "System.WorkItemType": "Bug",
          "System.Tags": "a; factory-ready",
          "System.CreatedBy": "Alice <alice@contoso.com>",
        },
      },
    };
    const r = await handleWebhook(req("of", "pw", { id: "n5", eventType: "workitem.updated", resource: wi }), hostWith());
    const as = find(r.events, "work_item_assigned")!;
    const lb = find(r.events, "work_item_labeled")!;
    expect(as.attributes?.["assignees"]).toEqual(["factory@contoso.com"]);
    expect(as.source).toMatchObject({ kind: "work_item", repo: "proj", number: 5, externalId: "5" });
    expect(lb.attributes).toMatchObject({ added_labels: ["factory-ready"], author: "alice@contoso.com", work_item_type: "Bug", labels: ["a", "factory-ready"] });
  });

  it("emits comment and mention events of a work item", async () => {
    const res = {
      id: 7,
      fields: {
        "System.TeamProject": "proj",
        "System.Title": "Bug",
        "System.History": `<div>hi <a href="#" data-vss-mention="version:2.0,${FACTORY_IDENTITY}">@bot</a></div>`,
      },
      revisedBy: { id: "u1", uniqueName: "alice@contoso.com" },
    };
    const r = await handleWebhook(req("of", "pw", { id: "c1", eventType: "workitem.commented", resource: res }), hostWith());
    expect(find(r.events, "work_item_commented")!.text).toContain("hi @bot");
    expect(find(r.events, "work_item_mentioned")!.attributes).toMatchObject({ mentioned: [FACTORY_IDENTITY], alias: "payments" });
  });

  it("ignores the activity of a factory identity", async () => {
    const comment = {
      comment: { content: `@<${FACTORY_IDENTITY}> again`, author: { id: FACTORY_IDENTITY.toUpperCase(), uniqueName: "bot@contoso.com" } },
      pullRequest: pr,
    };
    const r = await handleWebhook(req("of", "pw", { id: "s", eventType: "ms.vss-code.git-pullrequest-comment-event", resource: comment }), hostWith());
    expect(find(r.events, "pull_request_mentioned")).toBeUndefined();
    expect(r.events[0]!.attributes?.["actor_is_self"]).toBe(true);
  });

  it("emits a push event per branch update and skips deletions and tags", async () => {
    const push = {
      pushedBy: { id: "u1", uniqueName: "alice@contoso.com" },
      repository: { id: REPO_ID, name: "web", project: { name: "proj" } },
      refUpdates: [
        { name: "refs/heads/main", newObjectId: "abc" },
        { name: "refs/heads/gone", newObjectId: "0000000" },
        { name: "refs/tags/v1", newObjectId: "def" },
      ],
    };
    const r = await handleWebhook(req("of", "pw", { id: "p1", eventType: "git.push", resource: push }), hostWith());
    expect(r.events).toHaveLength(1);
    expect(r.events[0]).toMatchObject({ type: "push", deliveryId: "p1:push:main", actionKey: "azure_devops:p1:push:main" });
    expect(r.events[0]!.attributes).toMatchObject({ branch: "main", head_sha: "abc", repo: REPO_ID });
  });
});

describe("work item routing", () => {
  const created = (project: string) => ({
    id: 9,
    fields: {
      "System.TeamProject": project,
      "System.Title": "Crash",
      "System.WorkItemType": "Bug",
      "System.CreatedBy": { id: "u1", uniqueName: "alice@contoso.com" },
    },
  });
  const repos: Record<string, RepoRef[]> = {
    fac_pay: [{ codeForge: "AZURE_DEVOPS", owner: "payments-proj", name: "api" }],
    fac_web: [{ codeForge: "AZURE_DEVOPS", owner: "contoso/web-proj", name: "site" }],
    fac_web2: [{ codeForge: "AZURE_DEVOPS", owner: "web-proj", name: "admin" }],
    fac_gh: [{ codeForge: "GITHUB", owner: "payments-proj", name: "api" }],
  };
  const factories: FakeFactory[] = [
    { id: "fac_pay", teamId: "team_1", alias: "payments", name: "" },
    { id: "fac_web", teamId: "team_1", alias: "web", name: "" },
    { id: "fac_web2", teamId: "team_1", alias: "builder", name: "" },
    { id: "fac_gh", teamId: "team_1", alias: "gh", name: "" },
  ];
  const cfg = { ...baseConfig, factory_identities: `${FACTORY_IDENTITY}=builder` };

  const cases: Array<{
    name: string;
    config?: Record<string, string>;
    factory?: string;
    teamToo?: boolean;
    payload: { id: string; eventType: string; resource: unknown };
    type: string;
    want: string | undefined;
  }> = [
    { name: "project with one factory", payload: { id: "r1", eventType: "workitem.created", resource: created("payments-proj") }, type: "work_item_created", want: "fac_pay" },
    { name: "project with two factories", payload: { id: "r2", eventType: "workitem.created", resource: created("web-proj") }, type: "work_item_created", want: undefined },
    { name: "unknown project", payload: { id: "r3", eventType: "workitem.created", resource: created("other") }, type: "work_item_created", want: undefined },
    { name: "record of a factory", factory: "fac_web", payload: { id: "r4", eventType: "workitem.created", resource: created("web-proj") }, type: "work_item_created", want: "fac_web" },
    { name: "team record wins at intake", factory: "fac_web", teamToo: true, payload: { id: "r6", eventType: "workitem.created", resource: created("web-proj") }, type: "work_item_created", want: undefined },
    { name: "old config factory_id is not read", config: { factory_id: "fac_web" }, payload: { id: "r7", eventType: "workitem.created", resource: created("web-proj") }, type: "work_item_created", want: undefined },
    {
      name: "assigned to a factory identity",
      payload: {
        id: "r5",
        eventType: "workitem.updated",
        resource: {
          workItemId: 7,
          fields: { "System.AssignedTo": { newValue: { id: FACTORY_IDENTITY.toUpperCase(), uniqueName: "builder@contoso.com" } } },
          revisedBy: { id: "u1", uniqueName: "alice@contoso.com" },
          revision: { fields: { "System.TeamProject": "web-proj", "System.Title": "Bug" } },
        },
      },
      type: "work_item_assigned",
      want: "fac_web2",
    },
  ];

  it.each(cases)("$name", async (tc) => {
    const c = { ...cfg, ...tc.config };
    const records = [rec({ config: c, ...(tc.factory ? { factoryId: tc.factory } : {}) })];
    if (tc.teamToo) records.push(rec({ id: "int_team", config: c }));
    const r = await handleWebhook(req("of", "pw", tc.payload), hostWith({ records, factories, repos }));
    const ev = find(r.events, tc.type);
    expect(ev, JSON.stringify(r.events.map((e) => e.type))).toBeDefined();
    expect(ev!.factoryId).toBe(tc.want);
  });
});

describe("helpers", () => {
  it("reads the organization of a base URL", () => {
    expect(orgFromBaseUrl("https://dev.azure.com/contoso/")).toBe("contoso");
    expect(orgFromBaseUrl("https://contoso.visualstudio.com/")).toBe("contoso");
    expect(orgFromBaseUrl("")).toBe("");
    expect(orgFromBaseUrl("not a url")).toBe("");
  });

  it("extracts mention GUIDs from markdown and HTML, not from code", () => {
    expect(extractMentions(`hi @<${FACTORY_IDENTITY.toUpperCase()}> and \`@<${REPO_ID}>\``)).toEqual([FACTORY_IDENTITY]);
    expect(extractMentions(`<a data-vss-mention="version:2.0,${REPO_ID}">x</a>`)).toEqual([REPO_ID]);
  });

  it("matches a project to a factory repository", () => {
    expect(projectCovers({ codeForge: "AZURE_DEVOPS", owner: "contoso/web", name: "x" }, "WEB")).toBe(true);
    expect(projectCovers({ codeForge: "", owner: "web", name: "x" }, "web")).toBe(true);
    expect(projectCovers({ codeForge: "GITHUB", owner: "web", name: "x" }, "web")).toBe(false);
    expect(projectCovers({ codeForge: "", owner: "web", name: "x" }, "")).toBe(false);
  });

  it("parses basic authentication", () => {
    const r = new Request("https://x", { headers: { Authorization: "Basic " + btoa("u:p:q") } });
    expect(basicAuth(r)).toEqual({ user: "u", pass: "p:q" });
    expect(basicAuth(new Request("https://x"))).toBeUndefined();
    expect(basicAuth(new Request("https://x", { headers: { Authorization: "Bearer x" } }))).toBeUndefined();
  });
});
