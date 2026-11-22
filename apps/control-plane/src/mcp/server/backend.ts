// What the Factory MCP tools call. Go called the REST API's service methods
// in process (api.Server); here each tool calls the same REST operation of
// /api/v1 through the route table, as the authenticated principal, so the
// owners of those routes (TeamDO, WorkDO, IndexDO) stay the only writers.
// Work-item changes without a REST route go to WorkDO ops.
import { OfError, doName, partitionOf, type ErrorCode } from "@openfactory/core";
import { compile } from "@openfactory/definition";
import type { Principal } from "../../contracts/do-messages";
import type { Env } from "../../contracts/env";
import { API_PREFIX, Router } from "../../http/router";
import { unwrapOp } from "../../lib/rpc";
import { ROUTES } from "../../http/routes";
import type { PullRequestRef } from "./refs";

export interface Repository {
  codeForge: string;
  owner: string;
  name: string;
  default_branch?: string;
  private?: boolean;
  url?: string;
}

export interface TrackerScope {
  provider: string;
  id: string;
  key?: string;
  name: string;
}

export interface Connection {
  connection_id?: string;
  provider: string;
  status: string;
  authorize_url?: string;
  integration_id?: string;
  detail?: string;
}

export interface WorkItemPatch {
  /** "" clears it. */
  local_owner?: string;
  labels?: Record<string, string>;
  /** A work-item event to append (kind "note"). */
  event?: { kind: string; actor: string; data: Record<string, unknown> };
}

export interface FactoryMcpBackend {
  /**
   * Calls a REST operation (path below /api/v1) as p. Returns the JSON body;
   * throws OfError on 4xx/5xx. executionCtx is the MCP request's context:
   * background work of the route (waitUntil) runs on it.
   */
  api<T = unknown>(
    env: Env,
    p: Principal,
    method: string,
    path: string,
    body?: unknown,
    executionCtx?: ExecutionContext,
  ): Promise<T>;
  /** api.ValidateFiles: compiles a definition tree without saving it. */
  validateFiles(env: Env, files: Record<string, string>): Promise<unknown>;
  /** Sets local_owner, labels and appends a timeline event of a work item. */
  patchWorkItem(env: Env, p: Principal, workItemId: string, patch: WorkItemPatch): Promise<void>;
  /** Engine.LinkPullRequest for a work item, as p. */
  linkPullRequest(env: Env, p: Principal, workItemId: string, pr: PullRequestRef): Promise<void>;
  /** The active work item of a source key in a factory (FindWorkItemBySource), or null. */
  resolveSourceKey(env: Env, p: Principal, factoryId: string, key: string): Promise<string | null>;
  listRepositories?: (env: Env, teamId: string, codeForge: string, query: string) => Promise<Repository[]>;
  listTrackerScopes?: (env: Env, teamId: string, provider: string) => Promise<TrackerScope[]>;
  startConnection?: (env: Env, teamId: string, provider: string) => Promise<Connection>;
  connectionStatus?: (env: Env, teamId: string, connectionId: string) => Promise<Connection>;
}

const router = new Router(ROUTES);

/**
 * The context passed to a route handler. With the request's context, waitUntil
 * goes to it. Without one, the promises are collected and awaited before
 * callRoute returns, so background work is never dropped.
 */
function routeCtx(executionCtx: ExecutionContext | undefined): { ctx: ExecutionContext; settle: () => Promise<void> } {
  if (executionCtx) return { ctx: executionCtx, settle: async () => undefined };
  const pending: Promise<unknown>[] = [];
  const ctx = {
    waitUntil: (pr: Promise<unknown>) => void pending.push(pr),
    passThroughOnException: () => undefined,
    props: {},
  } as unknown as ExecutionContext;
  const settle = async () => {
    while (pending.length > 0) {
      for (const r of await Promise.allSettled(pending.splice(0))) {
        if (r.status === "rejected") console.error("mcp: background work of a route failed", r.reason);
      }
    }
  };
  return { ctx, settle };
}

/** Calls a route handler in process with a pre-authenticated principal. */
export async function callRoute<T>(
  env: Env,
  p: Principal,
  method: string,
  path: string,
  body?: unknown,
  executionCtx?: ExecutionContext,
): Promise<T> {
  const url = new URL(API_PREFIX + path, env.OF_PUBLIC_URL || "http://localhost");
  const m = router.match(method, url.pathname.slice(API_PREFIX.length));
  if (m.kind !== "match") throw new OfError("internal_error", `no route ${method} ${path}`);
  const headers = new Headers({ "x-request-id": "mcp" });
  let reqBody: string | undefined;
  if (body !== undefined) {
    headers.set("content-type", "application/json");
    reqBody = JSON.stringify(body);
  }
  const request = new Request(url, { method, headers, ...(reqBody !== undefined ? { body: reqBody } : {}) });
  const rc = routeCtx(executionCtx);
  const res = await m.route.handler(request, env, {
    params: m.params,
    url,
    requestId: "mcp",
    route: m.route,
    executionCtx: rc.ctx,
    principal: p,
  });
  const text = await res.text();
  await rc.settle();
  let out: unknown = null;
  if (text.trim() !== "") {
    try {
      out = JSON.parse(text);
    } catch {
      out = text;
    }
  }
  if (res.status >= 400) {
    const e = out as { error?: { code?: string }; detail?: string } | null;
    const code = (e?.error?.code ?? (res.status === 404 ? "resource_not_found" : "internal_error")) as ErrorCode;
    throw new OfError(code, e?.detail ?? "");
  }
  return out as T;
}

/** b with every api call running on executionCtx (the MCP request's context). */
export function withExecutionCtx(b: FactoryMcpBackend, executionCtx: ExecutionContext | undefined): FactoryMcpBackend {
  if (!executionCtx) return b;
  return {
    ...b,
    api: <T>(env: Env, p: Principal, method: string, path: string, body?: unknown, ctx?: ExecutionContext) =>
      b.api<T>(env, p, method, path, body, ctx ?? executionCtx),
  };
}

async function workOp(env: Env, p: Principal, id: string, op: string, body: unknown): Promise<unknown> {
  const stub = env.WORK.getByName(doName.work(p.teamId, partitionOf(id)));
  return unwrapOp(await stub.api({ requestId: "mcp", principal: p }, op, { id }, body));
}

export const defaultFactoryBackend: FactoryMcpBackend = {
  api: callRoute,
  async validateFiles(_env, files) {
    // api.ValidateFiles: compile only (no team preflight), with a summary when valid.
    if (Object.keys(files).length === 0) throw new OfError("invalid_request", "files is required");
    const res = compile(files);
    const valid = res.errors.length === 0;
    const def = res.definition;
    return {
      valid,
      errors: res.errors,
      warnings: [],
      skipped: [],
      ...(valid && def
        ? {
            summary: {
              name: def.name,
              ...(def.alias ? { alias: def.alias } : {}),
              agents: (def.agents ?? []).map((a) => a.name),
              automations: (def.automations ?? []).map((a) => a.name),
              machines: Object.keys(def.machines ?? {}).sort(),
              scorers: (def.scorers ?? []).map((x) => x.slug),
              webhooks: (def.webhooks ?? []).map((w) => w.name),
              benchmarks: (def.benchmarks ?? []).map((x) => (x as { slug: string }).slug),
              skills: (def.skills ?? []).map((x) => (x as { name: string }).name),
            },
          }
        : {}),
    };
  },
  async patchWorkItem(env, p, id, patch) {
    await workOp(env, p, id, "mcpPatchWorkItem", patch);
  },
  async linkPullRequest(env, p, id, pr) {
    await workOp(env, p, id, "mcpLinkPullRequest", pr);
  },
  async resolveSourceKey(env, p, factoryId, key) {
    const r = await env.ROUTER.getByName(doName.router(p.teamId)).resolve(factoryId, key);
    return r && r.status !== "complete" && r.status !== "cancelled" ? r.wiId : null;
  },
};
