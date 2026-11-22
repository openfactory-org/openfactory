// Factory MCP tools over a fake REST backend (port of the main cases of
// internal/mcpserver/mcpserver_test.go: tools listed, onboarding, schema and
// validation, work-item lifecycle, references, hand-back).
import "../index"; // the Worker entry first, as in production (module order of TeamDO)
import { env } from "cloudflare:workers";
import { describe, expect, it } from "vitest";
import { OfError, workItemLink } from "@openfactory/core";
import type { Principal } from "../contracts/do-messages";
import type { Env } from "../contracts/env";
import { factoryMcpServer } from "./index";
import type { FactoryMcpBackend, WorkItemPatch } from "./server/backend";
import { defaultFactoryBackend } from "./server/backend";
import { FACTORY_TOOLS, type FactoryCtx } from "./server/tools";
import type { PullRequestRef } from "./server/refs";

const PUB = "https://factory.example.test";
const editor: Principal = {
  kind: "user",
  id: "usr_e",
  teamId: "default",
  role: "editor",
  factoryRoles: {},
  userId: "usr_e",
};

interface WI {
  id: string;
  factory_id: string;
  title: string;
  stage: string;
  root_run_id?: string;
  pull_requests?: PullRequestRef[];
  local_owner?: string;
  labels?: Record<string, string>;
  source_key?: string;
  created_at: string;
  updated_at: string;
}

function fakeBackend() {
  const state = {
    wis: new Map<string, WI>(),
    patches: [] as { id: string; patch: WorkItemPatch }[],
    links: [] as { id: string; pr: PullRequestRef }[],
    followups: [] as { run: string; message: string }[],
    dispatched: [] as Record<string, unknown>[],
    messages: [] as { seq: number; role: string; content: string; created_at: string }[],
  };
  let n = 0;
  const backend: FactoryMcpBackend = {
    async api<T>(_e: Env, _p: Principal, method: string, path: string, body?: unknown): Promise<T> {
      const u = new URL("http://x" + path);
      const p = u.pathname;
      const r = (v: unknown) => v as T;
      if (method === "GET" && p === "/factories")
        return r({ items: [{ id: "fac_1", name: "web", alias: "web-foreman", source: "managed" }] });
      if (method === "GET" && p === "/factories/fac_1") return r({ id: "fac_1", name: "web" });
      if (method === "GET" && p.startsWith("/factories/") && p.endsWith("/integrations"))
        return r({ integrations: ["slack", "linear"] });
      if (method === "GET" && p === "/factories/fac_1/definition")
        return r({ compiled: { repositories: [{ codeForge: "GITHUB", owner: "acme", name: "web" }] } });
      if (method === "GET" && p === "/factories/fac_1/work-items")
        return r({ items: [...state.wis.values()], page_info: {} });
      if (method === "GET" && p.startsWith("/factories/")) throw new OfError("resource_not_found", "factory not found");
      if (method === "GET" && p.startsWith("/work-items/")) {
        const wi = state.wis.get(decodeURIComponent(p.slice("/work-items/".length)));
        if (!wi) throw new OfError("resource_not_found", "work item not found");
        return r({
          ...wi,
          runs: wi.root_run_id
            ? [{ run_id: wi.root_run_id, state: "running", created_at: wi.created_at, result: "draft" }]
            : [],
          inbox: [],
          timeline: [],
        });
      }
      if (method === "GET" && p.startsWith("/runs/") && p.endsWith("/messages")) return r({ messages: state.messages });
      if (method === "GET" && p.startsWith("/runs/")) throw new OfError("resource_not_found", "run not found");
      if (method === "POST" && p === "/factories/fac_1/runs") {
        n++;
        state.dispatched.push(body as Record<string, unknown>);
        const now = new Date(Date.UTC(2026, 0, 1, 0, 0, n)).toISOString();
        const wi: WI = {
          id: `wi_${n}`,
          factory_id: "fac_1",
          title: String((body as { title: string }).title),
          stage: "intake",
          root_run_id: `run_${n}`,
          created_at: now,
          updated_at: now,
        };
        state.wis.set(wi.id, wi);
        return r({ id: wi.root_run_id, run_id: wi.root_run_id, work_item_id: wi.id, state: "queued" });
      }
      if (method === "POST" && p.endsWith("/followups")) {
        state.followups.push({ run: p.split("/")[2]!, message: (body as { message: string }).message });
        return r({});
      }
      if (method === "POST" && p.endsWith("/messages")) return r({ work_item_id: p.split("/")[2], run_id: "run_1" });
      if (method === "POST" && p.endsWith("/complete")) {
        const wi = state.wis.get(p.split("/")[2]!)!;
        wi.stage = "complete";
        return r(wi);
      }
      if (method === "GET" && p === "/teams/default") return r({ id: "default", name: "Acme" });
      if (method === "GET" && p === "/integrations") return r({ items: [{ id: "int_s", provider: "slack" }] });
      if (method === "GET" && p === "/factory-files/schemas") return r({ current_version: "v1", versions: ["v1"] });
      if (method === "GET" && p === "/factory-files/schemas/v1")
        return r({ "factory.schema.json": {}, "agent.schema.json": {} });
      if (method === "GET" && p === "/factory-files/schemas/v1/factory.schema.json") return r({ type: "object" });
      throw new Error(`unexpected ${method} ${path}`);
    },
    validateFiles: defaultFactoryBackend.validateFiles,
    async patchWorkItem(_e, _p, id, patch) {
      state.patches.push({ id, patch });
      const wi = state.wis.get(id)!;
      if (patch.local_owner !== undefined) wi.local_owner = patch.local_owner || undefined;
      if (patch.labels) wi.labels = { ...wi.labels, ...patch.labels };
    },
    async linkPullRequest(_e, _p, id, pr) {
      state.links.push({ id, pr });
      const wi = state.wis.get(id)!;
      wi.pull_requests = [...(wi.pull_requests ?? []), pr];
    },
    async resolveSourceKey() {
      return null;
    },
  };
  return { state, backend };
}

async function call(
  ctx: FactoryCtx,
  name: string,
  args: Record<string, unknown> = {},
): Promise<{ out: Record<string, unknown>; isError: boolean }> {
  const t = FACTORY_TOOLS.find((x) => x.name === name)!;
  const res = await t.handler(args, ctx);
  return { out: JSON.parse(res.content[0]!.text) as Record<string, unknown>, isError: !!res.isError };
}

const errCode = (o: Record<string, unknown>) => (o.error as { code: string }).code;

describe("Factory MCP tools", () => {
  it("lists the tools with read-only hints (TestToolsListed)", () => {
    const names = factoryMcpServer.tools.map((t) => t.name);
    expect(names).toEqual([
      "list_factories",
      "get_factory_file_schema",
      "validate_factory_files",
      "list_teams",
      "create_team",
      "join_team",
      "list_forge_repositories",
      "list_tracker_scopes",
      "start_connection",
      "get_connection_status",
      "create_factory",
      "list_work_items",
      "search_work_items",
      "get_work_item",
      "message_foreman",
      "get_conversation",
      "send_work_item",
      "list_notification_routes",
      "complete_work_item",
    ]);
    expect(factoryMcpServer.tools.find((t) => t.name === "list_factories")!.annotations?.readOnlyHint).toBe(true);
    expect(factoryMcpServer.tools.find((t) => t.name === "send_work_item")!.annotations?.readOnlyHint).toBe(false);
  });

  it("onboarding tools (TestOnboardingTools)", async () => {
    const { backend } = fakeBackend();
    const ctx: FactoryCtx = { env: env as unknown as Env, principal: editor, backend, publicURL: PUB };
    expect((await call(ctx, "list_teams")).out.memberships).toEqual([
      { name: "Acme", role: "editor", team_id: "default" },
    ]);
    expect(errCode((await call(ctx, "create_team", { name: "x" })).out)).toBe("invalid_request");
    expect(errCode((await call(ctx, "create_team", { name: "x", confirmed: true })).out)).toBe("conflict");
    expect((await call(ctx, "join_team", { team_id: "default" })).out.status).toBe("already_member");
    expect(errCode((await call(ctx, "join_team", { team_id: "t2" })).out)).toBe("operation_not_supported");
    expect(errCode((await call(ctx, "list_forge_repositories", { code_forge: "BITBUCKET" })).out)).toBe(
      "invalid_request",
    );
    expect(errCode((await call(ctx, "list_forge_repositories")).out)).toBe("integration_not_configured");
    expect(errCode((await call(ctx, "list_tracker_scopes", { provider: "linear", team_id: "other" })).out)).toBe(
      "resource_not_found",
    );
    expect((await call(ctx, "start_connection", { provider: "slack" })).out).toEqual({
      provider: "slack",
      status: "connected",
      integration_id: "int_s",
    });
    expect(errCode((await call(ctx, "start_connection", { provider: "linear" })).out)).toBe("not_authorized");
    expect(errCode((await call(ctx, "start_connection", { provider: "svn" })).out)).toBe("invalid_request");
  });

  it("schema and validation (TestSchemaAndValidation)", async () => {
    const { backend } = fakeBackend();
    const ctx: FactoryCtx = { env: env as unknown as Env, principal: editor, backend, publicURL: PUB };
    const cat = (await call(ctx, "get_factory_file_schema")).out;
    expect(cat.version).toBe("v1");
    expect(cat.documents).toEqual(["agent.schema.json", "factory.schema.json"]);
    expect((await call(ctx, "get_factory_file_schema", { document: "factory.schema.json" })).out.schema).toEqual({
      type: "object",
    });
    expect(errCode((await call(ctx, "validate_factory_files", { files: {} })).out)).toBe("invalid_request");
    const bad = (await call(ctx, "validate_factory_files", { files: { "agents/x/agent.md": "---\n---\n" } })).out;
    expect(bad.valid).toBe(false);
  });

  it("work item lifecycle: send, list, start working, converse, hand back, complete (TestWorkItemLifecycle)", async () => {
    const { state, backend } = fakeBackend();
    const ctx: FactoryCtx = { env: env as unknown as Env, principal: editor, backend, publicURL: PUB };
    expect(errCode((await call(ctx, "send_work_item", { factory_id: "fac_1" })).out)).toBe("invalid_request");
    expect(errCode((await call(ctx, "send_work_item", { note: "x", factory_id: "fac_missing" })).out)).toBe(
      "resource_not_found",
    );
    expect(
      errCode((await call(ctx, "send_work_item", { note: "x", factory_id: "fac_1", notify_route: "pager" })).out),
    ).toBe("invalid_request");
    const created = (
      await call(ctx, "send_work_item", {
        factory_id: "fac_1",
        note: "# Add dark mode\nUse CSS variables.",
        branch: "feat/dark",
        pull_request_url: "https://github.com/acme/web/pull/7",
        notify_route: "slack_dm",
      })
    ).out;
    expect(created.status).toBe("created");
    const id = created.work_item_id as string;
    expect(created.url).toBe(workItemLink(PUB, "fac_1", id));
    expect(state.dispatched[0]!.title).toBe("Add dark mode");
    expect(String(state.dispatched[0]!.prompt)).toContain("Branch: `feat/dark`");
    expect(state.links[0]!.pr).toMatchObject({ forge: "GITHUB", repo: "acme/web", number: 7, branch: "feat/dark" });
    expect(state.patches.at(-1)!.patch.labels).toEqual({ notify_route: "slack_dm" });

    const routes = (await call(ctx, "list_notification_routes", { factory_id: "fac_1" })).out.routes as {
      id: string;
    }[];
    expect(routes.map((r) => r.id)).toEqual(["inbox", "linear_issue", "slack_dm"]);

    const list = (await call(ctx, "list_work_items", { factory_id: "fac_1", stage: "intake" })).out;
    expect((list.work_items as unknown[]).length).toBe(1);
    expect(errCode((await call(ctx, "list_work_items", { factory_id: "fac_1", stage: "bogus" })).out)).toBe(
      "invalid_request",
    );
    expect(errCode((await call(ctx, "list_work_items", { factory_id: "fac_1", since: "yesterday" })).out)).toBe(
      "invalid_request",
    );

    // References resolve to the work item: by ID, by pull request URL, by branch.
    for (const ref of [id, "https://github.com/acme/web/pull/7", "feat/dark"]) {
      const got = (await call(ctx, "get_work_item", { work_item: ref })).out;
      expect((got.work_item as { id: string }).id, ref).toBe(id);
    }
    expect(errCode((await call(ctx, "get_work_item", { work_item: "nothing-matches" })).out)).toBe(
      "resource_not_found",
    );

    const sw = (await call(ctx, "get_work_item", { work_item: id, start_working: true })).out;
    const setup = sw.local_setup as {
      local_owner: string;
      repositories: { branch: string; existing_branch: boolean }[];
      warnings: string[];
    };
    expect(setup.local_owner).toBe("usr_e");
    expect(setup.repositories[0]!.branch).toBe("feat/dark");
    expect(setup.repositories[0]!.existing_branch).toBe(true);
    expect(setup.warnings.some((w) => w.includes("still active"))).toBe(true);
    expect((sw.work_item as { local_owner: string }).local_owner).toBe("usr_e");

    state.messages.push(
      { seq: 1, role: "user", content: "hi", created_at: "t" },
      { seq: 2, role: "log", content: "x", created_at: "t" },
    );
    const conv = (await call(ctx, "get_conversation", { work_item_id: id })).out;
    expect(conv.messages).toEqual([{ seq: 1, role: "user", content: "hi", created_at: "t" }]);
    expect(conv.last_seq).toBe(2);

    const back = (await call(ctx, "send_work_item", { work_item_id: id, note: "Done the CSS.", branch: "feat/dark" }))
      .out;
    expect(back.status).toBe("handed_back");
    expect(state.followups[0]!.message).toContain("## Work handed back from a local session (usr_e)");
    expect(state.patches.at(-1)!.patch).toMatchObject({
      local_owner: "",
      event: { kind: "note", data: { handed_back: true } },
    });

    expect((await call(ctx, "complete_work_item", { work_item_id: id })).out.status).toBe("completed");
    expect(errCode((await call(ctx, "send_work_item", { work_item_id: id, note: "again" })).out)).toBe("conflict");
  });

  // GET /factories returns at most 200 per page; the tools read every page.
  it("list_factories and reference lookups read every page of factories", async () => {
    const { backend } = fakeBackend();
    const queries: string[] = [];
    const asked: string[] = [];
    const paged: FactoryMcpBackend = {
      ...backend,
      async api<T>(e: Env, p: Principal, method: string, path: string, body?: unknown): Promise<T> {
        const u = new URL("http://x" + path);
        if (method === "GET" && u.pathname === "/factories") {
          queries.push(u.search);
          const cursor = u.searchParams.get("cursor");
          if (!cursor)
            return { items: [{ id: "fac_1", name: "web", source: "managed" }], page_info: { next_cursor: "c2" } } as T;
          if (cursor === "c2")
            return { items: [{ id: "fac_2", name: "api", source: "managed" }], page_info: { next_cursor: "c3" } } as T;
          return { items: [{ id: "fac_3", name: "docs", source: "git" }], page_info: {} } as T;
        }
        if (method === "GET" && /^\/factories\/fac_[23]\/work-items$/.test(u.pathname))
          return { items: [], page_info: {} } as T;
        return backend.api<T>(e, p, method, path, body);
      },
      async resolveSourceKey(_e, _p, factoryId, key) {
        asked.push(`${factoryId} ${key}`);
        return null;
      },
    };
    const ctx: FactoryCtx = { env: env as unknown as Env, principal: editor, backend: paged, publicURL: PUB };
    const out = (await call(ctx, "list_factories")).out.factories as { uid: string }[];
    expect(out.map((f) => f.uid).sort()).toEqual(["fac_1", "fac_2", "fac_3"]);
    expect(queries).toEqual(["?limit=200", "?limit=200&cursor=c2", "?limit=200&cursor=c3"]);

    // A reference lookup asks the factories of every page for the source key.
    expect(errCode((await call(ctx, "get_work_item", { work_item: "acme/web#12" })).out)).toBe("resource_not_found");
    expect(asked).toContain("fac_3 github:acme/web#12");
  });
});
