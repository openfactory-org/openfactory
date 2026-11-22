// The run side of the MCP endpoints that runs call (/mcp/proxy/{runId}/{id}
// and /mcp/integrations/{provider}/{runId}): run authentication with the
// runner's ofs_ session token (runauth), the run view, and the effects that
// belong to other owners (WorkDO, TeamDO). Injected so that tests use fakes.
import { doName, partitionOf, type MCPServer } from "@openfactory/core";
import type { Env } from "../contracts/env";
import { runnerKeys, verifySessionToken } from "../runner-api/token";
import { expandWith, placeholders } from "../secrets/expand";
import { unwrapOp } from "../lib/rpc";
import { teamStub } from "./team";

/** What the MCP endpoints read of a run. */
export interface RunView {
  id: string;
  teamId: string;
  factoryId: string;
  workItemId?: string;
  metadata: Record<string, string>;
  /** Run.config.mcpServers (managedId, allowWrite). */
  mcpServers: Record<string, MCPServer>;
}

export type RunAuthResult = { run: RunView } | { status: number; code: string; message: string };

export interface McpRunBackend {
  /** runauth.Verify plus GetRun: 401 unauthorized, 409 session_superseded, 404 run not found. */
  authenticate(env: Env, runId: string, token: string): Promise<RunAuthResult>;
  /** The factory's allowAgentMerge. */
  allowAgentMerge(env: Env, run: RunView): Promise<boolean>;
  /** Audit of a merge tool call (OnMergeCall). */
  recordMergeCall(env: Env, run: RunView, server: string, tool: string, allowed: boolean): Promise<void>;
  /** Appends a "log" message to the run transcript (trial mode writes). */
  appendRunLog(env: Env, run: RunView, content: string, data: unknown): Promise<void>;
  /** The source of the run's work item (provider, workspace), or null. */
  workItemSource(env: Env, run: RunView): Promise<{ provider: string; workspace: string } | null>;
  /** Expands {{SECRET}} placeholders with the run's factory secrets. */
  expandSecrets(env: Env, run: RunView, value: string): Promise<string>;
}

/** Normalizes engine config.mcp_servers (snake or camel case) to MCPServer. */
export function mcpServersOf(raw: unknown): Record<string, MCPServer> {
  const out: Record<string, MCPServer> = {};
  if (typeof raw !== "object" || raw === null) return out;
  for (const [name, v] of Object.entries(raw as Record<string, Record<string, unknown>>)) {
    if (typeof v !== "object" || v === null) continue;
    const managedId = (v.managedId ?? v.managed_id) as string | undefined;
    const allowWrite = (v.allowWrite ?? v.allow_write) as boolean | undefined;
    out[name] = { ...(v as MCPServer), ...(managedId ? { managedId } : {}), ...(allowWrite ? { allowWrite } : {}) };
  }
  return out;
}

/** Engine Run (src/engine/types.ts) to RunView. */
export function runViewOf(r: {
  id: string;
  team_id: string;
  factory_id: string;
  work_item_id?: string;
  metadata?: Record<string, string>;
  config?: { mcp_servers?: unknown; mcpServers?: unknown };
}): RunView {
  return {
    id: r.id,
    teamId: r.team_id,
    factoryId: r.factory_id,
    ...(r.work_item_id ? { workItemId: r.work_item_id } : {}),
    metadata: r.metadata ?? {},
    mcpServers: mcpServersOf(r.config?.mcp_servers ?? r.config?.mcpServers),
  };
}

const UNAUTHORIZED = { status: 401, code: "unauthorized", message: "invalid or missing run token" } as const;

/** The production backend: session tokens, WorkDO and TeamDO. */
export const defaultRunBackend: McpRunBackend = {
  async authenticate(env, runId, token) {
    if (!token || !runId) return UNAUTHORIZED;
    const claims = await verifySessionToken(await runnerKeys(env), token);
    if (!claims || claims.run !== runId) return UNAUTHORIZED;
    const stub = env.WORK.getByName(doName.work(claims.team, partitionOf(runId)));
    const res = (await stub.runnerAuth(runId, claims.ses)) as unknown as
      { run: Parameters<typeof runViewOf>[0] } | { error: { status: number; code: string; message: string } };
    if ("error" in res) {
      if (res.error.status === 409)
        return { status: 409, code: "session_superseded", message: "a newer session exists" };
      if (res.error.status === 404) return { status: 404, code: "not_found", message: "run not found" };
      return UNAUTHORIZED;
    }
    return { run: runViewOf(res.run) };
  },

  async allowAgentMerge(env, run) {
    try {
      const snap = (await teamStub(env, run.teamId).configSnapshot(run.factoryId)) as Record<string, unknown> | null;
      const def = (snap?.definition ?? snap?.compiled ?? snap) as Record<string, unknown> | undefined;
      const v =
        def?.allowAgentMerge ??
        def?.allow_agent_merge ??
        (def?.settings as Record<string, unknown> | undefined)?.allowAgentMerge;
      return v === true;
    } catch {
      return false;
    }
  },

  async recordMergeCall(_env, run, server, tool, allowed) {
    console.warn(
      JSON.stringify({ msg: "agent merge call", run: run.id, factory: run.factoryId, server, tool, allowed }),
    );
  },

  async appendRunLog(env, run, content, data) {
    const stub = env.WORK.getByName(doName.work(run.teamId, partitionOf(run.id)));
    unwrapOp(await stub.api({ requestId: "mcp" }, "appendRunLog", { runId: run.id }, { content, data }));
  },

  async workItemSource(env, run) {
    if (!run.workItemId) return null;
    try {
      const stub = env.WORK.getByName(doName.work(run.teamId, partitionOf(run.workItemId)));
      const wi = (await stub.api({ requestId: "mcp" }, "getWorkItem", { id: run.workItemId }, null)) as {
        source?: { provider?: string; workspace?: string };
        work_item?: { source?: { provider?: string; workspace?: string } };
      } | null;
      const src = wi?.source ?? wi?.work_item?.source;
      return src ? { provider: src.provider ?? "", workspace: src.workspace ?? "" } : null;
    } catch {
      return null;
    }
  },

  async expandSecrets(env, run, value) {
    const names = placeholders(value);
    if (names.length === 0) return value;
    // One read for every name; a missing name is reported by expandWith.
    const vals = await teamStub(env, run.teamId).secretsResolve(
      { requestId: "mcp" },
      { factoryId: run.factoryId, runId: run.id, names },
    );
    return expandWith(value, (n) => (Object.hasOwn(vals, n) ? vals[n] : undefined));
  },
};

/** Go writeError: {"error":{"code","message"}}. */
export function runError(
  status: number,
  code: string,
  message: string,
  headers: Record<string, string> = {},
): Response {
  return new Response(JSON.stringify({ error: { code, message } }) + "\n", {
    status,
    headers: { "content-type": "application/json", ...headers },
  });
}

export function authErrorResponse(r: { status: number; code: string; message: string }): Response {
  return runError(
    r.status,
    r.code,
    r.message,
    r.status === 401 ? { "www-authenticate": 'Bearer realm="openfactory-run"' } : {},
  );
}

/** The bearer token of a run request (case-sensitive "Bearer " as in Go). */
export function runToken(request: Request): string {
  const h = request.headers.get("authorization") ?? "";
  return h.startsWith("Bearer ") ? h.slice("Bearer ".length) : "";
}
