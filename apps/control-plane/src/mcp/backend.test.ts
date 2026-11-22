// The Factory MCP passes the request's ExecutionContext to the REST routes
// that its tools call, and resolves its backends per request.
import { env } from "cloudflare:workers";
import { afterEach, describe, expect, it } from "vitest";
import type { Principal } from "../contracts/do-messages";
import type { Env } from "../contracts/env";
import { bootstrap } from "../../test/helpers";
import { handleMcpRequest, resetMcpBackends, setMcpBackendResolver, setMcpBackends } from "./index";
import { defaultFactoryBackend, withExecutionCtx, type FactoryMcpBackend } from "./server/backend";

const E = env as unknown as Env;

afterEach(() => {
  resetMcpBackends();
});

function fakeCtx(): ExecutionContext {
  return {
    waitUntil: () => undefined,
    passThroughOnException: () => undefined,
    props: {},
  } as unknown as ExecutionContext;
}

/** A factory backend that records the context of each api call. */
function recordingBackend(seen: (ExecutionContext | undefined)[]): FactoryMcpBackend {
  return {
    ...defaultFactoryBackend,
    api: async <T>(_e: Env, _p: Principal, _m: string, _path: string, _b?: unknown, ctx?: ExecutionContext) => {
      seen.push(ctx);
      return { id: "default", name: "Acme" } as T;
    },
  };
}

async function listTeams(key: string, ctx?: ExecutionContext): Promise<Response> {
  const res = await handleMcpRequest(
    new Request("http://localhost/mcp", {
      method: "POST",
      body: JSON.stringify({
        jsonrpc: "2.0",
        id: 1,
        method: "tools/call",
        params: { name: "list_teams", arguments: {} },
      }),
      headers: {
        authorization: "Bearer " + key,
        "content-type": "application/json",
        accept: "application/json, text/event-stream",
      },
    }),
    E,
    ctx,
  );
  if (!res) throw new Error("not an MCP path");
  return res;
}

describe("Factory MCP execution context", () => {
  it("passes the request's context to the routes that tools call", async () => {
    const { key } = await bootstrap();
    const seen: (ExecutionContext | undefined)[] = [];
    setMcpBackends({ factory: recordingBackend(seen) });
    const ctx = fakeCtx();
    const res = await listTeams(key, ctx);
    expect(res.status, await res.clone().text()).toBe(200);
    expect(seen).toHaveLength(1);
    expect(seen[0]).toBe(ctx);
  });

  it("keeps an explicit context of a call", async () => {
    const seen: (ExecutionContext | undefined)[] = [];
    const reqCtx = fakeCtx();
    const own = fakeCtx();
    const b = withExecutionCtx(recordingBackend(seen), reqCtx);
    const p = { kind: "user", id: "u", teamId: "default", role: "admin", factoryRoles: {} } as Principal;
    await b.api(E, p, "GET", "/teams/default");
    await b.api(E, p, "GET", "/teams/default", undefined, own);
    expect(seen).toEqual([reqCtx, own]);
  });
});

describe("Factory MCP backends", () => {
  it("resolves the backends of each request from its env", async () => {
    const { key } = await bootstrap();
    const seen: string[] = [];
    const backendOf = (tag: string): FactoryMcpBackend => ({
      ...defaultFactoryBackend,
      api: async <T>() => {
        seen.push(tag);
        return { id: "default", name: "Acme" } as T;
      },
    });
    const envA = { ...E, OF_PUBLIC_URL: "http://localhost" } as Env;
    const envB = { ...E, OF_PUBLIC_URL: "http://localhost" } as Env;
    const tags = new Map<Env, string>([
      [envA, "a"],
      [envB, "b"],
    ]);
    const prev = setMcpBackendResolver((e) => (tags.has(e) ? { factory: backendOf(tags.get(e)!) } : {}));
    try {
      const call = (e: Env) =>
        handleMcpRequest(
          new Request("http://localhost/mcp", {
            method: "POST",
            body: JSON.stringify({
              jsonrpc: "2.0",
              id: 1,
              method: "tools/call",
              params: { name: "list_teams", arguments: {} },
            }),
            headers: {
              authorization: "Bearer " + key,
              "content-type": "application/json",
              accept: "application/json, text/event-stream",
            },
          }),
          e,
        );
      // Concurrent requests with different envs each use their own backend.
      const out = await Promise.all([call(envA), call(envB), call(envA)]);
      for (const r of out) expect(r?.status).toBe(200);
      expect(seen.sort()).toEqual(["a", "a", "b"]);
    } finally {
      setMcpBackendResolver(prev);
    }
  });
});
