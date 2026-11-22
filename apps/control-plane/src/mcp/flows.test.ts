// End-to-end flows of the MCP port in the Workers runtime, with TeamDO and
// fake upstreams behind an injected fetch. Ports of mcpauth_test.go,
// internal/api/mcpservers_test.go, mcpsignins_test.go and the OAuth, proxy,
// built-in, trial and merge-guard cases of internal/mcpregistry.
import { env } from "cloudflare:workers";
import { runInDurableObject } from "cloudflare:test";
import { afterEach, beforeAll, describe, expect, it } from "vitest";
import { MCP_OAUTH_CONNECTED, MCP_OAUTH_NOT_CONNECTED, doName, type RegisteredMCPServer } from "@openfactory/core";
import type { Principal, Role } from "../contracts/do-messages";
import type { Env } from "../contracts/env";
import { issueSessionCookie } from "../auth/session";
import { b64url, sha256 } from "../lib/bytes";
import { handleMcpRequest, resetMcpBackends, setMcpBackends, setMcpRuntime } from "./index";
import { localEncrypter } from "../secrets/envelope";
import { REFUSAL_TEXT } from "./mergeguard";
import type { IntegrationBackend, IntegrationRecord } from "./registry/builtin";
import type { McpRunBackend, RunView } from "./run";
import { callRoute } from "./server/backend";
import { teamOp, type McpRuntime } from "./team";

const E = env as unknown as Env;
const PUBLIC = "http://localhost";

// ---------- fixtures ----------

type FakeHandler = (req: Request) => Response | Promise<Response>;
const upstreams = new Map<string, FakeHandler>();
let clock = Date.UTC(2026, 10, 9, 12, 0, 0);

const fakeFetch: typeof fetch = async (input, init) => {
  const req = new Request(input as RequestInfo, init);
  const u = new URL(req.url);
  for (const [prefix, h] of upstreams)
    if (req.url.startsWith(prefix) || u.origin + u.pathname === prefix) return h(req);
  return new Response("no fake for " + req.url, { status: 599 });
};

beforeAll(() => {
  let enc: ReturnType<typeof localEncrypter> | undefined;
  setMcpRuntime((e): McpRuntime => ({
    fetch: fakeFetch,
    now: () => clock,
    encrypter: () => (enc ??= localEncrypter(e)),
  }));
});

afterEach(() => {
  upstreams.clear();
  resetMcpBackends();
});

async function seedUser(id: string, role: Role, opts: { kind?: string; team?: string } = {}): Promise<void> {
  const stub = E.TEAM.getByName(doName.team("default"));
  await runInDurableObject(stub, (_i, state) => {
    const u = {
      id,
      team_id: opts.team ?? "default",
      email: `${id}@example.test`,
      name: id,
      role,
      ...(opts.kind ? { kind: opts.kind } : {}),
      created_at: new Date().toISOString(),
    };
    state.storage.sql.exec(
      "INSERT OR REPLACE INTO users (id, email, kind, role, name, created_ms, data) VALUES (?, ?, ?, ?, ?, ?, ?)",
      id,
      u.email,
      opts.kind ?? "",
      role,
      id,
      Date.now(),
      JSON.stringify(u),
    );
  });
}

const principal = (id: string, role: Role): Principal => ({
  kind: "user",
  id,
  teamId: "default",
  role,
  factoryRoles: {},
  userId: id,
});

async function rest<T = Record<string, unknown>>(
  p: Principal,
  method: string,
  path: string,
  body?: unknown,
): Promise<{ status: number; body: T }> {
  try {
    return { status: 200, body: await callRoute<T>(E, p, method, path, body) };
  } catch (e) {
    const code = (e as { code?: string }).code ?? "";
    const status = { invalid_request: 400, not_authorized: 403, resource_not_found: 404, conflict: 409 }[code] ?? 500;
    return { status, body: { code, detail: (e as { detail?: string }).detail } as T };
  }
}

async function sessionCookie(userId: string): Promise<string> {
  return (await issueSessionCookie(E, { u: userId, t: "default", p: "password" })).split(";")[0]!;
}

async function req(
  method: string,
  path: string,
  init: { body?: string; headers?: Record<string, string> } = {},
): Promise<Response> {
  const res = await handleMcpRequest(new Request(PUBLIC + path, { method, ...init, redirect: "manual" }), E);
  if (!res) throw new Error("not an MCP path: " + path);
  return res;
}

async function rpc(
  path: string,
  token: string,
  method: string,
  params: unknown = {},
): Promise<Record<string, unknown>> {
  const res = await req("POST", path, {
    body: JSON.stringify({ jsonrpc: "2.0", id: 1, method, params }),
    headers: {
      authorization: "Bearer " + token,
      "content-type": "application/json",
      accept: "application/json, text/event-stream",
    },
  });
  expect(res.status, await res.clone().text()).toBe(200);
  return (await res.json()) as Record<string, unknown>;
}

// ---------- mcpauth ----------

describe("Factory MCP browser sign-in (mcpauth)", () => {
  beforeAll(async () => {
    await seedUser("usr_ada", "editor");
    await seedUser("sa_bot", "editor", { kind: "service_account" });
  });

  async function register(uris = ["http://localhost/cb"]): Promise<string> {
    const r = await req("POST", "/oauth/register", {
      body: JSON.stringify({ client_name: "cli", redirect_uris: uris }),
    });
    expect(r.status).toBe(201);
    return ((await r.json()) as { client_id: string }).client_id;
  }

  /** The developer in the browser: opens the authorization URL and answers. */
  async function browser(query: string, decision: string, signedIn = true): Promise<URL> {
    const cookie = signedIn ? await sessionCookie("usr_ada") : "";
    const page = await req("GET", "/oauth/authorize?" + query, { headers: cookie ? { cookie } : {} });
    if (!signedIn) return new URL(page.headers.get("location")!, PUBLIC);
    const html = await page.text();
    expect(page.status, html).toBe(200);
    const m = /name="request_id" value="([^"]+)"/.exec(html);
    expect(m).not.toBeNull();
    const post = await req("POST", "/oauth/authorize", {
      body: new URLSearchParams({ request_id: m![1]!, decision }).toString(),
      headers: { "content-type": "application/x-www-form-urlencoded", cookie, origin: PUBLIC },
    });
    expect(post.status).toBe(302);
    return new URL(post.headers.get("location")!);
  }

  const token = async (form: Record<string, string>) =>
    (await (
      await req("POST", "/oauth/token", {
        body: new URLSearchParams(form).toString(),
        headers: { "content-type": "application/x-www-form-urlencoded" },
      })
    ).json()) as Record<string, unknown>;

  async function signIn(): Promise<{ clientId: string; tok: Record<string, unknown> }> {
    const clientId = await register();
    const verifier = "v".repeat(50);
    const q = new URLSearchParams({
      response_type: "code",
      client_id: clientId,
      redirect_uri: "http://localhost:5555/cb",
      code_challenge: b64url(await sha256(verifier)),
      code_challenge_method: "S256",
      state: "xyz",
      resource: PUBLIC + "/mcp",
    }).toString();
    let r = await browser(q, "allow");
    expect(r.searchParams.get("state")).toBe("xyz");
    expect(r.searchParams.get("iss")).toBe(PUBLIC);
    // A wrong verifier fails, and the code is then used up.
    const bad = await token({
      grant_type: "authorization_code",
      code: r.searchParams.get("code")!,
      client_id: clientId,
      code_verifier: "w".repeat(50),
    });
    expect(bad.error).toBe("invalid_grant");
    r = await browser(q, "allow");
    const code = r.searchParams.get("code")!;
    const tok = await token({
      grant_type: "authorization_code",
      code,
      client_id: clientId,
      code_verifier: verifier,
      redirect_uri: "http://localhost:5555/cb",
    });
    expect(tok.access_token, JSON.stringify(tok)).toBeTruthy();
    const again = await token({ grant_type: "authorization_code", code, client_id: clientId, code_verifier: verifier });
    expect(again.error).toBe("invalid_grant");
    return { clientId, tok };
  }

  it("metadata, 401 with resource metadata, registration rules", async () => {
    for (const path of ["/mcp", "/api/v1/mcp/factory"]) {
      const r = await req("POST", path, { body: "{}" });
      expect(r.status).toBe(401);
      expect(r.headers.get("www-authenticate")).toContain(
        `resource_metadata="${PUBLIC}/.well-known/oauth-protected-resource${path}"`,
      );
      const md = (await (await req("GET", "/.well-known/oauth-protected-resource" + path)).json()) as {
        resource: string;
        authorization_servers: string[];
      };
      expect(md.resource).toBe(PUBLIC + path);
      expect(md.authorization_servers).toEqual([PUBLIC]);
    }
    const meta = (await (await req("GET", "/.well-known/oauth-authorization-server")).json()) as Record<string, string>;
    expect(meta.issuer).toBe(PUBLIC);
    expect(meta.token_endpoint).toBe(PUBLIC + "/oauth/token");
    for (const [uris, ok] of [
      [["http://127.0.0.1:8080/cb"], true],
      [["https://app.example.test/cb"], true],
      [["com.example.agent://oauth/callback"], true],
      [["http://evil.example.test/cb"], false],
      [["javascript:alert(1)"], false],
      [[], false],
    ] as [string[], boolean][]) {
      const r = await req("POST", "/oauth/register", { body: JSON.stringify({ redirect_uris: uris }) });
      expect(r.status === 201, uris.join()).toBe(ok);
    }
  });

  it("denied and signed out", async () => {
    const clientId = await register();
    const q = new URLSearchParams({
      response_type: "code",
      client_id: clientId,
      redirect_uri: "http://localhost/cb",
      code_challenge: "abc",
      code_challenge_method: "S256",
    }).toString();
    const denied = await browser(q, "deny");
    expect(denied.searchParams.get("error")).toBe("access_denied");
    const out = await browser(q, "", false);
    expect(out.pathname + out.search).toContain("/login?return_to=%2Foauth%2Fauthorize");
  });

  it("token lifecycle: verify, expiry, refresh rotation, revocation, removed user", async () => {
    const { clientId, tok } = await signIn();
    const access = tok.access_token as string;
    const refresh = tok.refresh_token as string;
    expect(access.startsWith("ofm_")).toBe(true);
    expect(refresh.startsWith("ofr_")).toBe(true);
    expect(tok.expires_in).toBe(3600);

    // The Factory MCP server accepts the access token.
    const list = await rpc("/mcp", access, "tools/list");
    expect((list.result as { tools: unknown[] }).tools.length).toBeGreaterThan(10);
    const grants = await rest<{ items: { client_name: string; team_id: string; id: string }[] }>(
      principal("usr_ada", "editor"),
      "GET",
      "/mcp-sign-ins",
    );
    expect(grants.body.items.some((g) => g.client_name === "cli" && g.team_id === "default")).toBe(true);

    clock += 61 * 60 * 1000;
    expect((await req("POST", "/mcp", { body: "{}", headers: { authorization: "Bearer " + access } })).status).toBe(
      401,
    );
    const next = await token({ grant_type: "refresh_token", refresh_token: refresh, client_id: clientId });
    const newAccess = next.access_token as string;
    expect(newAccess).toBeTruthy();
    expect(next.refresh_token).not.toBe(refresh);
    await rpc("/mcp", newAccess, "ping");
    expect((await token({ grant_type: "refresh_token", refresh_token: refresh })).error).toBe("invalid_grant");

    // RFC 7009 revocation ends the sign-in.
    const rv = await req("POST", "/oauth/revoke", {
      body: new URLSearchParams({ token: next.refresh_token as string }).toString(),
    });
    expect(rv.status).toBe(200);
    expect((await req("POST", "/mcp", { body: "{}", headers: { authorization: "Bearer " + newAccess } })).status).toBe(
      401,
    );

    // A user moved out of the team cannot use a sign-in.
    const second = await signIn();
    await seedUser("usr_ada", "editor", { team: "team2" });
    expect(
      (
        await req("POST", "/mcp", {
          body: "{}",
          headers: { authorization: "Bearer " + (second.tok.access_token as string) },
        })
      ).status,
    ).toBe(401);
    await seedUser("usr_ada", "editor");
  });

  it("sign-ins: list, team scope for admins, revoke own or as admin (TestMCPSignIns)", async () => {
    await seedUser("usr_admin", "admin");
    await seedUser("usr_bob", "user");
    const { tok } = await signIn();
    const ada = principal("usr_ada", "editor");
    const mine = await rest<{ items: { id: string }[] }>(ada, "GET", "/mcp-sign-ins");
    const id = mine.body.items[0]!.id;
    expect((await rest(principal("usr_bob", "user"), "GET", "/mcp-sign-ins?scope=team")).status).toBe(403);
    const team = await rest<{ items: { id: string }[] }>(
      principal("usr_admin", "admin"),
      "GET",
      "/mcp-sign-ins?scope=team",
    );
    expect(team.body.items.some((g) => g.id === id)).toBe(true);
    expect((await rest(principal("usr_bob", "user"), "DELETE", `/mcp-sign-ins/${id}`)).status).toBe(403);
    expect((await rest(ada, "DELETE", "/mcp-sign-ins/mcps_missing")).status).toBe(404);
    expect((await rest(principal("usr_admin", "admin"), "DELETE", `/mcp-sign-ins/${id}`)).status).toBe(200);
    for (const g of mine.body.items.slice(1)) await rest(ada, "DELETE", `/mcp-sign-ins/${g.id}`);
    expect(
      (await req("POST", "/mcp", { body: "{}", headers: { authorization: "Bearer " + (tok.access_token as string) } }))
        .status,
    ).toBe(401);
  });

  it("a service account cannot sign in", async () => {
    const clientId = await register();
    const cookie = await sessionCookie("sa_bot");
    const q = new URLSearchParams({
      response_type: "code",
      client_id: clientId,
      redirect_uri: "http://localhost/cb",
      code_challenge: "abc",
      code_challenge_method: "S256",
    }).toString();
    const r = await req("GET", "/oauth/authorize?" + q, { headers: { cookie } });
    // The session principal of a service account is not a user: the browser goes to the login.
    expect([302, 403]).toContain(r.status);
  });
});

// ---------- registry CRUD ----------

describe("MCP server registry REST (TestMCPServersCRUD)", () => {
  const editor = principal("usr_ed", "editor");
  const viewer = principal("usr_view", "user");

  it("validates, defaults, enforces names, patches and deletes", async () => {
    expect((await rest(editor, "POST", "/mcp-servers", { name: "x" })).status).toBe(400);
    expect(
      (await rest(editor, "POST", "/mcp-servers", { name: "x", url: "https://a.test/mcp", command: "x" })).status,
    ).toBe(400);
    expect((await rest(editor, "POST", "/mcp-servers", { name: "builtin-x", url: "https://a.test/mcp" })).status).toBe(
      400,
    );
    const lit = await rest(editor, "POST", "/mcp-servers", {
      name: "x",
      url: "https://a.test/mcp",
      headers: { Authorization: "Bearer abc" },
    });
    expect(lit.status).toBe(400);
    expect(String(lit.body.detail)).toContain("{{SECRET_NAME}}");
    expect((await rest(viewer, "POST", "/mcp-servers", { name: "x", url: "https://a.test/mcp" })).status).toBe(403);

    const c = await rest<RegisteredMCPServer>(editor, "POST", "/mcp-servers", {
      name: "sentry",
      url: "https://mcp.sentry.test/mcp",
      headers: { Authorization: "Bearer {{SENTRY_TOKEN}}" },
    });
    expect(c.body.connection).toBe("proxy");
    expect(c.body.created_by).toBe("user:usr_ed");
    const id = c.body.id;
    for (const name of ["sentry", "SENTRY", "Sentry"]) {
      const dup = await rest(editor, "POST", "/mcp-servers", { name, url: "https://other.test/mcp" });
      expect(dup.status).toBe(409);
      expect(dup.body.detail).toBe(`an MCP server named "sentry" already exists`);
    }
    const other = await rest<RegisteredMCPServer>(editor, "POST", "/mcp-servers", {
      name: "rename-me",
      url: "https://other.test/mcp",
    });
    expect((await rest(editor, "PATCH", `/mcp-servers/${other.body.id}`, { name: "SENTRY" })).status).toBe(409);
    expect((await rest(editor, "PATCH", `/mcp-servers/${other.body.id}`, { name: "Rename-Me" })).status).toBe(200);
    expect((await rest(editor, "DELETE", `/mcp-servers/${other.body.id}`)).status).toBe(200);

    const d = await rest<RegisteredMCPServer & { warnings?: string[] }>(editor, "POST", "/mcp-servers", {
      name: "private",
      url: "http://10.0.0.5/mcp",
      connection: "direct",
    });
    expect(d.body.connection).toBe("direct");
    expect(d.body.warnings).toBeUndefined();
    const sse = await rest<RegisteredMCPServer & { warnings?: string[] }>(editor, "POST", "/mcp-servers", {
      name: "legacy",
      url: "https://c.test/sse",
    });
    expect(sse.body.connection).toBe("direct");
    expect(sse.body.warnings).toHaveLength(1);
    expect((await rest<{ warnings?: unknown }>(viewer, "GET", "/mcp-servers/legacy")).body.warnings).toBeUndefined();
    for (const n of ["private", "legacy"]) expect((await rest(editor, "DELETE", `/mcp-servers/${n}`)).status).toBe(200);

    const o = await rest<RegisteredMCPServer>(editor, "POST", "/mcp-servers", {
      name: "notion",
      url: "https://mcp.notion.test/mcp",
      oauth: { scopes: ["read"] },
    });
    expect(o.body.connection).toBe("proxy");
    expect(o.body.oauth?.status).toBe(MCP_OAUTH_NOT_CONNECTED);

    expect((await rest<RegisteredMCPServer>(viewer, "GET", "/mcp-servers/sentry")).body.id).toBe(id);
    const l = await rest<{ items: unknown[]; builtin: unknown[] }>(viewer, "GET", "/mcp-servers");
    expect(l.body.items).toHaveLength(2);
    expect(l.body.builtin).toHaveLength(4);

    const u = await rest<RegisteredMCPServer>(editor, "PATCH", `/mcp-servers/${id}`, {
      url: "",
      headers: {},
      command: "npx",
      args: ["-y", "sentry-mcp"],
    });
    expect(u.body.command).toBe("npx");
    expect(u.body.url).toBeUndefined();
    expect(u.body.connection).toBeUndefined();

    // Changing the OAuth client while connected disconnects and forgets the token.
    await teamOp(E, "mcp.oauth.complete", { serverId: o.body.id, envelope: "x", userId: "usr_ed" });
    const p = await rest<RegisteredMCPServer>(editor, "PATCH", `/mcp-servers/${o.body.id}`, {
      oauth: { client_id: "abc" },
    });
    expect(p.body.oauth?.status).toBe(MCP_OAUTH_NOT_CONNECTED);
    expect((await teamOp<{ envelope: string }>(E, "mcp.oauth.token", { serverId: o.body.id })).envelope).toBe("");
    expect((await rest(editor, "PATCH", `/mcp-servers/${o.body.id}`, { oauth: { bogus: 1 } })).status).toBe(400);
    const np = await rest<RegisteredMCPServer>(editor, "PATCH", `/mcp-servers/${o.body.id}`, {
      oauth: null,
      connection: "direct",
    });
    expect(np.body.oauth).toBeUndefined();
    expect(np.body.connection).toBe("direct");
    expect((await rest(editor, "POST", `/mcp-servers/${id}/oauth/connect`)).status).toBe(400);

    expect((await rest(editor, "DELETE", `/mcp-servers/${id}`)).status).toBe(200);
    expect((await rest(viewer, "GET", `/mcp-servers/${id}`)).status).toBe(404);
  });

  it("resolve for the planner (TestResolve)", async () => {
    const mk = async (b: unknown) => (await rest<RegisteredMCPServer>(editor, "POST", "/mcp-servers", b)).body;
    const direct = await mk({ name: "docs", command: "npx", args: ["-y", "docs"], env: { DOCS_TOKEN: "{{DOCS}}" } });
    const prox = await mk({
      name: "sentry2",
      url: "https://mcp.sentry.test/mcp",
      headers: { Authorization: "Bearer {{S}}" },
    });
    await mk({ name: "notion2", url: "https://mcp.notion.test/mcp", oauth: {} });
    const run = { id: "run_r1", mcpServers: {} };
    const resolve = (managedId: string) => teamOp<Record<string, unknown>>(E, "mcp.resolve", { run, managedId });
    expect(await resolve("docs")).toEqual({ command: "npx", args: ["-y", "docs"], env: { DOCS_TOKEN: "{{DOCS}}" } });
    expect((await resolve(direct.id)).command).toBe("npx");
    expect(await resolve("sentry2")).toEqual({ url: `${PUBLIC}/mcp/proxy/run_r1/${prox.id}` });
    await expect(resolve("notion2")).rejects.toThrow(/not connected/);
    expect(await resolve("builtin/linear")).toEqual({ url: `${PUBLIC}/mcp/integrations/linear/run_r1` });
    await expect(resolve("builtin/nope")).rejects.toMatchObject({ code: "invalid_request" });
    await expect(resolve("missing")).rejects.toMatchObject({ code: "resource_not_found" });
  });
});

// ---------- OAuth connect, refresh, proxy ----------

function fakeRunBackend(
  runs: Map<string, RunView>,
  opts: { allowMerge?: () => boolean; records?: string[]; logs?: string[]; secrets?: Record<string, string> } = {},
): McpRunBackend {
  return {
    async authenticate(_env, runId, token) {
      const run = runs.get(runId);
      if (!run || token !== "ofs_good-" + runId)
        return { status: 401, code: "unauthorized", message: "invalid or missing run token" };
      return { run };
    },
    async allowAgentMerge() {
      return opts.allowMerge?.() ?? false;
    },
    async recordMergeCall(_e, run, _server, tool, allowed) {
      opts.records?.push(`${run.id}|${tool}|${allowed ? "allowed" : "refused"}`);
    },
    async appendRunLog(_e, run, content) {
      opts.logs?.push(`${run.id}|${content}`);
    },
    async workItemSource() {
      return null;
    },
    async expandSecrets(_e, _run, v) {
      return v.replace(/\{\{\s*([A-Z_][A-Z0-9_]*)\s*\}\}/g, (_m, n: string) => {
        const s = opts.secrets?.[n];
        if (s === undefined) throw new Error("missing " + n);
        return s;
      });
    },
  };
}

/** An MCP server protected by OAuth, with its authorization server. */
function fakeProvider(base: string) {
  const st = { tokens: 0, refreshes: 0, lastAuth: "", lastResource: "", expiresIn: 3600 };
  upstreams.set(base + "/mcp", async (r) => {
    const auth = r.headers.get("authorization") ?? "";
    if (!auth.startsWith("Bearer at-")) {
      return new Response(null, {
        status: 401,
        headers: { "www-authenticate": `Bearer resource_metadata="${base}/.well-known/oauth-protected-resource/mcp"` },
      });
    }
    st.lastAuth = `${auth}|${r.headers.get("x-team") ?? ""}|${r.headers.get("cookie") ?? ""}`;
    return new Response(`event: message\ndata: ${await r.text()}\n\n`, {
      headers: { "content-type": "text/event-stream", "mcp-session-id": "sess-1", "set-cookie": "a=b" },
    });
  });
  upstreams.set(base + "/.well-known/oauth-protected-resource/mcp", () =>
    Response.json({
      resource: base + "/mcp",
      authorization_servers: [base + "/as"],
      scopes_supported: ["read", "write"],
    }),
  );
  upstreams.set(base + "/.well-known/oauth-authorization-server/as", () =>
    Response.json({
      issuer: base + "/as",
      authorization_endpoint: base + "/as/authorize",
      token_endpoint: base + "/as/token",
      registration_endpoint: base + "/as/register",
      code_challenge_methods_supported: ["S256"],
    }),
  );
  upstreams.set(base + "/as/register", async (r) => {
    const body = (await r.json()) as { redirect_uris: string[] };
    return Response.json({ client_id: "dyn-client", redirect_uris: body.redirect_uris }, { status: 201 });
  });
  upstreams.set(base + "/as/token", async (r) => {
    const f = new URLSearchParams(new TextDecoder().decode(await r.arrayBuffer()));
    st.lastResource = f.get("resource") ?? "";
    if (f.get("client_id") !== "dyn-client") return Response.json({ error: "invalid_client" }, { status: 400 });
    if (f.get("grant_type") === "authorization_code" && (f.get("code") !== "the-code" || !f.get("code_verifier"))) {
      return Response.json({ error: "invalid_grant" }, { status: 400 });
    }
    if (f.get("grant_type") === "refresh_token") {
      if (!f.get("refresh_token")?.startsWith("rt-")) return Response.json({ error: "invalid_grant" }, { status: 400 });
      st.refreshes++;
    }
    st.tokens++;
    return Response.json({
      access_token: `at-${st.tokens}`,
      token_type: "Bearer",
      expires_in: st.expiresIn,
      refresh_token: `rt-${st.tokens}`,
    });
  });
  return st;
}

describe("registry OAuth, proxy and built-in servers", () => {
  const editor = principal("usr_conn", "editor");
  beforeAll(async () => {
    await seedUser("usr_conn", "editor");
    await seedUser("usr_other", "editor");
  });

  it("connects with discovery and DCR, proxies with token and headers, refreshes once, disconnects (TestOAuthConnectRefreshAndProxy)", async () => {
    const base = "https://notion.example.test";
    const p = fakeProvider(base);
    const m = (
      await rest<RegisteredMCPServer>(editor, "POST", "/mcp-servers", {
        name: "notion-o",
        url: base + "/mcp",
        headers: { "X-Team": "{{TEAM_HEADER}}" },
        oauth: {},
      })
    ).body;
    const cn = await rest<{ authorization_url: string }>(editor, "POST", `/mcp-servers/${m.id}/oauth/connect`);
    expect(cn.status).toBe(200);
    const au = new URL(cn.body.authorization_url);
    expect(au.pathname).toBe("/as/authorize");
    expect(au.searchParams.get("client_id")).toBe("dyn-client");
    expect(au.searchParams.get("code_challenge_method")).toBe("S256");
    expect(au.searchParams.get("redirect_uri")).toBe(PUBLIC + "/api/v1/mcp-servers/oauth/callback");
    expect(au.searchParams.get("resource")).toBe(base + "/mcp");
    expect(au.searchParams.get("scope")).toBe("read write");

    // Another user cannot finish it, and the state is single use.
    const cb = (state: string, who: Principal) =>
      callRoute(E, who, "GET", `/mcp-servers/oauth/callback?state=${state}&code=the-code`).catch((e) => e);
    const callback = async (state: string, who: Principal) => {
      const url = new URL(`${PUBLIC}/api/v1/mcp-servers/oauth/callback?state=${state}&code=the-code`);
      const { mcpOAuthCallback } = await import("../api/mcp-servers");
      const res = await mcpOAuthCallback(new Request(url), E, {
        params: {},
        url,
        requestId: "t",
        route: {
          method: "GET",
          path: "/mcp-servers/oauth/callback",
          operationId: "mcpOAuthCallback",
          role: "editor",
          handler: mcpOAuthCallback,
        },
        executionCtx: {} as ExecutionContext,
        principal: who,
      });
      return new URL(res.headers.get("location")!);
    };
    void cb;
    const state = au.searchParams.get("state")!;
    const other = await callback(state, principal("usr_other", "editor"));
    expect(other.searchParams.get("oauth")).toBe("error");
    expect((await callback(state, editor)).searchParams.get("oauth")).toBe("error");

    const au2 = new URL(
      (await rest<{ authorization_url: string }>(editor, "POST", `/mcp-servers/${m.id}/oauth/connect`)).body
        .authorization_url,
    );
    const done = await callback(au2.searchParams.get("state")!, editor);
    expect(done.searchParams.get("oauth")).toBe("connected");
    expect(done.pathname).toBe(`/mcp-servers/${m.id}`);
    expect(p.lastResource).toBe(base + "/mcp");
    const after = (await rest<RegisteredMCPServer>(editor, "GET", `/mcp-servers/${m.id}`)).body;
    expect(after.oauth?.status).toBe(MCP_OAUTH_CONNECTED);
    expect(after.oauth?.connected_by).toBe("usr_conn");
    const stored = (await teamOp<{ envelope: string }>(E, "mcp.oauth.token", { serverId: m.id })).envelope;
    expect(stored).not.toBe("");
    expect(stored).not.toContain("at-1");

    // The run reaches the proxy; the proxy adds the token and headers.
    const runs = new Map<string, RunView>();
    runs.set("run_a", {
      id: "run_a",
      teamId: "default",
      factoryId: "fac1",
      metadata: {},
      mcpServers: { notes: { managedId: "notion-o" } },
    });
    runs.set("run_b", { id: "run_b", teamId: "default", factoryId: "fac1", metadata: {}, mcpServers: {} });
    setMcpBackends({ run: fakeRunBackend(runs, { secrets: { TEAM_HEADER: "eng" } }) });
    const resolved = await teamOp<{ url: string }>(E, "mcp.resolve", {
      run: { id: "run_a", mcpServers: runs.get("run_a")!.mcpServers },
      managedId: "notion-o",
    });
    const call = (auth: string, url = resolved.url) =>
      handleMcpRequest(
        new Request(url, {
          method: "POST",
          body: `{"jsonrpc":"2.0","id":1,"method":"ping"}`,
          headers: {
            authorization: auth,
            "content-type": "application/json",
            accept: "application/json, text/event-stream",
            cookie: "leak=1",
          },
        }),
        E,
      ) as Promise<Response>;
    let res = await call("Bearer ofs_good-run_a");
    const body = await res.text();
    expect(res.status, body).toBe(200);
    expect(body).toContain(`"method":"ping"`);
    expect(res.headers.get("mcp-session-id")).toBe("sess-1");
    expect(res.headers.get("set-cookie")).toBeNull();
    expect(p.lastAuth).toBe("Bearer at-1|eng|");
    expect((await call("Bearer wrong")).status).toBe(401);
    expect((await call("Bearer ofs_good-run_b", `${PUBLIC}/mcp/proxy/run_b/${m.id}`)).status).toBe(403);

    // Near expiry, the token is refreshed once.
    clock += 59 * 60 * 1000 + 30 * 1000;
    res = await call("Bearer ofs_good-run_a");
    await res.text();
    expect(p.refreshes).toBe(1);
    expect(p.lastAuth).toBe("Bearer at-2|eng|");
    await (await call("Bearer ofs_good-run_a")).text();
    expect(p.refreshes).toBe(1);

    // Disconnect deletes the token; the proxy then reports not connected.
    expect((await rest(editor, "POST", `/mcp-servers/${m.id}/oauth/disconnect`)).status).toBe(200);
    res = await call("Bearer ofs_good-run_a");
    expect(res.status).toBe(502);
    expect(await res.text()).toContain("not_connected");
  });

  it("needs a client id without registration (TestOAuthNeedsClientWithoutRegistration)", async () => {
    const base = "https://noreg.example.test";
    upstreams.set(base + "/.well-known/oauth-authorization-server", () =>
      Response.json({ authorization_endpoint: "https://idp.test/authorize", token_endpoint: "https://idp.test/token" }),
    );
    const m = (
      await rest<RegisteredMCPServer>(editor, "POST", "/mcp-servers", { name: "noreg", url: base + "/mcp", oauth: {} })
    ).body;
    const r = await rest(editor, "POST", `/mcp-servers/${m.id}/oauth/connect`);
    expect(r.status).toBe(400);
    expect(String(r.body.detail)).toContain("client_id");
    await rest(editor, "PATCH", `/mcp-servers/${m.id}`, { oauth: { client_id: "static" } });
    const ok = await rest<{ authorization_url: string }>(editor, "POST", `/mcp-servers/${m.id}/oauth/connect`);
    expect(ok.body.authorization_url.startsWith("https://idp.test/authorize?")).toBe(true);
    expect(ok.body.authorization_url).toContain("client_id=static");
  });

  it("default proxy keeps the header out of the sandbox (TestDefaultProxyKeepsHeaderOutOfSandbox)", async () => {
    let upstreamAuth = "";
    upstreams.set("https://sentry.example.test/mcp", (r) => {
      upstreamAuth = r.headers.get("authorization") ?? "";
      return Response.json({ jsonrpc: "2.0", id: 1, result: {} });
    });
    const m = (
      await rest<RegisteredMCPServer>(editor, "POST", "/mcp-servers", {
        name: "sentry-p",
        url: "https://sentry.example.test/mcp",
        headers: { Authorization: "Bearer {{SENTRY_TOKEN}}" },
      })
    ).body;
    const runs = new Map<string, RunView>([
      [
        "run_s",
        {
          id: "run_s",
          teamId: "default",
          factoryId: "fac1",
          metadata: {},
          mcpServers: { s: { managedId: "sentry-p" } },
        },
      ],
    ]);
    setMcpBackends({ run: fakeRunBackend(runs, { secrets: { SENTRY_TOKEN: "sentry-secret-value" } }) });
    const got = await teamOp<Record<string, unknown>>(E, "mcp.resolve", {
      run: { id: "run_s" },
      managedId: "sentry-p",
    });
    expect(got).toEqual({ url: `${PUBLIC}/mcp/proxy/run_s/${m.id}` });
    const res = (await handleMcpRequest(
      new Request(got.url as string, {
        method: "POST",
        body: `{"jsonrpc":"2.0","id":1,"method":"ping"}`,
        headers: { authorization: "Bearer ofs_good-run_s" },
      }),
      E,
    ))!;
    expect(res.status).toBe(200);
    expect(upstreamAuth).toBe("Bearer sentry-secret-value");
  });

  it("refuses merge calls on the proxy and built-in servers unless allowAgentMerge (TestMergeRefused)", async () => {
    let merged = 0;
    upstreams.set("https://forge.example.test/mcp", async (r) => {
      const msg = (await r.json()) as { id: number; params?: { name?: string } };
      if (msg.params?.name === "merge_pull_request") merged++;
      return Response.json({ jsonrpc: "2.0", id: msg.id, result: { content: [{ type: "text", text: "ok" }] } });
    });
    const m = (
      await rest<RegisteredMCPServer>(editor, "POST", "/mcp-servers", {
        name: "forge",
        url: "https://forge.example.test/mcp",
      })
    ).body;
    let allow = false;
    const records: string[] = [];
    const runs = new Map<string, RunView>([
      [
        "run_m",
        {
          id: "run_m",
          teamId: "default",
          factoryId: "fac1",
          metadata: {},
          mcpServers: { forge: { managedId: "forge" } },
        },
      ],
      [
        "run_l",
        {
          id: "run_l",
          teamId: "default",
          factoryId: "fac1",
          metadata: {},
          mcpServers: { linear: { managedId: "builtin/linear", allowWrite: true } },
        },
      ],
    ]);
    setMcpBackends({
      run: fakeRunBackend(runs, { allowMerge: () => allow, records }),
      integrations: fakeIntegrations(),
    });
    const callTool = async (url: string, run: string, name: string) => {
      const res = (await handleMcpRequest(
        new Request(url, {
          method: "POST",
          body: JSON.stringify({
            jsonrpc: "2.0",
            id: 1,
            method: "tools/call",
            params: { name, arguments: { number: 1 } },
          }),
          headers: { authorization: "Bearer ofs_good-" + run },
        }),
        E,
      ))!;
      return (await res.json()) as { result: { isError?: boolean; content: { text: string }[] } };
    };
    const proxyURL = `${PUBLIC}/mcp/proxy/run_m/${m.id}`;
    expect((await callTool(proxyURL, "run_m", "get_pull_request")).result.isError).toBeFalsy();
    const refused = await callTool(proxyURL, "run_m", "merge_pull_request");
    expect(refused.result.isError).toBe(true);
    expect(refused.result.content[0]!.text).toBe(REFUSAL_TEXT);
    expect(merged).toBe(0);
    const b = await callTool(`${PUBLIC}/mcp/integrations/linear/run_l`, "run_l", "merge_pull_request");
    expect(b.result.content[0]!.text).toContain("cannot merge");
    allow = true;
    expect((await callTool(proxyURL, "run_m", "merge_pull_request")).result.isError).toBeFalsy();
    expect(merged).toBe(1);
    expect(records).toEqual([
      "run_m|merge_pull_request|refused",
      "run_l|merge_pull_request|refused",
      "run_m|merge_pull_request|allowed",
    ]);
  });

  it("built-in Linear: grant, write tools, trial mode (TestBuiltinLinear, TestTrialWritesStayInTheRun)", async () => {
    const logs: string[] = [];
    const calls: string[] = [];
    const runs = new Map<string, RunView>([
      [
        "run_r",
        {
          id: "run_r",
          teamId: "default",
          factoryId: "fac1",
          metadata: {},
          mcpServers: { linear: { managedId: "builtin/linear" } },
        },
      ],
      [
        "run_w",
        {
          id: "run_w",
          teamId: "default",
          factoryId: "fac1",
          metadata: {},
          mcpServers: { linear: { managedId: "builtin/linear", allowWrite: true } },
        },
      ],
      [
        "run_t",
        {
          id: "run_t",
          teamId: "default",
          factoryId: "fac1",
          metadata: { trial_mode: "true" },
          mcpServers: { linear: { managedId: "builtin/linear", allowWrite: true } },
        },
      ],
      ["run_n", { id: "run_n", teamId: "default", factoryId: "fac1", metadata: {}, mcpServers: {} }],
    ]);
    setMcpBackends({ run: fakeRunBackend(runs, { logs }), integrations: fakeIntegrations(calls) });
    const tools = async (run: string) => {
      const r = await rpc(`/mcp/integrations/linear/${run}`, "ofs_good-" + run, "tools/list");
      return (r.result as { tools: { name: string }[] }).tools.map((t) => t.name);
    };
    expect(await tools("run_r")).toEqual(["linear_get_issue", "linear_search_issues"]);
    expect(await tools("run_w")).toEqual(["linear_get_issue", "linear_search_issues", "linear_create_comment"]);
    const notGranted = await req("POST", "/mcp/integrations/linear/run_n", {
      body: "{}",
      headers: { authorization: "Bearer ofs_good-run_n" },
    });
    expect(notGranted.status).toBe(403);
    expect(
      (
        await req("POST", "/mcp/integrations/nope/run_r", {
          body: "{}",
          headers: { authorization: "Bearer ofs_good-run_r" },
        })
      ).status,
    ).toBe(404);

    const get = await rpc("/mcp/integrations/linear/run_r", "ofs_good-run_r", "tools/call", {
      name: "linear_get_issue",
      arguments: { id: "ENG-42" },
    });
    expect((get.result as { content: { text: string }[] }).content[0]!.text).toContain("ENG-42");
    await rpc("/mcp/integrations/linear/run_w", "ofs_good-run_w", "tools/call", {
      name: "linear_create_comment",
      arguments: { issue_id: "ENG-42", body: "hi" },
    });
    expect(calls).toContain("comment:ENG-42:hi");
    const trial = await rpc("/mcp/integrations/linear/run_t", "ofs_good-run_t", "tools/call", {
      name: "linear_create_comment",
      arguments: { issue_id: "ENG-1", body: "x" },
    });
    expect((trial.result as { content: { text: string }[] }).content[0]!.text).toContain("trial mode");
    expect(calls.filter((c) => c.startsWith("comment:ENG-1:"))).toHaveLength(0);
    expect(logs[0]).toContain("Trial mode: linear_create_comment was recorded");
  });

  it("built-in Teams lists channels and posts (TestBuiltinTeams)", async () => {
    const posts: string[] = [];
    const runs = new Map<string, RunView>([
      [
        "run_tm",
        {
          id: "run_tm",
          teamId: "default",
          factoryId: "fac1",
          metadata: {},
          mcpServers: { t: { managedId: "builtin/teams", allowWrite: true } },
        },
      ],
    ]);
    const ib = fakeIntegrations([], {
      provider: "microsoft_teams",
      config: { channel_ids: "19:a@thread, 19:b@thread" },
    });
    ib.teamsPost = async (_r, ch, root, text) => void posts.push(`${ch}|${root}|${text}`);
    setMcpBackends({ run: fakeRunBackend(runs), integrations: ib });
    const list = await rpc("/mcp/integrations/teams/run_tm", "ofs_good-run_tm", "tools/call", {
      name: "teams_list_channels",
      arguments: {},
    });
    expect(JSON.parse((list.result as { content: { text: string }[] }).content[0]!.text)).toEqual({
      channel_ids: ["19:a@thread", "19:b@thread"],
      limited: true,
    });
    await rpc("/mcp/integrations/teams/run_tm", "ofs_good-run_tm", "tools/call", {
      name: "teams_post_message",
      arguments: { channel: "19:a@thread", text: "hello" },
    });
    expect(posts).toEqual(["19:a@thread||hello"]);
  });
});

function fakeIntegrations(calls: string[] = [], rec: Partial<IntegrationRecord> = {}): IntegrationBackend {
  const record: IntegrationRecord = { id: "int1", provider: "linear", config: {}, ...rec };
  return {
    async factoryIntegration(_e, _run, provider) {
      if (provider === "jira")
        throw new Error("the factory does not attach the jira integration (add it to factory.yaml integrations)");
      return record;
    },
    async credential() {
      return "lin_api_x";
    },
    async linearQuery(_r, _t, _q, vars) {
      return { issue: { identifier: vars.id, title: "Fix it" } };
    },
    async linearCreateComment(_r, _t, id, body) {
      calls.push(`comment:${id}:${body}`);
    },
    async jiraRequest() {
      return {};
    },
    jiraText: () => "",
    markdownToADF: (md) => md,
    async slackCall() {
      return { messages: [] };
    },
    async slackPost() {
      return "1.2";
    },
    teamsPost: null,
  };
}
