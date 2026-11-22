// The per-run factory MCP server (/mcp/run/{runId}, internal/orchestration):
// stateless JSON-RPC in the Worker. Each tool is one WorkDO RPC; the tool
// idempotency row commits with the effect (C8). The key hashes canonical JSON
// of the input in the active turn (plan rule 3); errors are not stored.
import { doName, OfError, partitionOf, toOfError } from "@openfactory/core";
import type { Env } from "../contracts/env";
import { toolIdempotencyKey } from "../engine/canonical";
import type { AgentConfig, Run } from "../engine/types";
import type { ChildPlan } from "../do/work/engine";
import type { ToolResult } from "../do/work/tools";
import { runnerKeys, verifySessionToken } from "../runner-api/token";

export const RUN_MCP_PREFIX = "/mcp/run/";
const PROTOCOL_VERSION = "2025-06-18";

/** Agents of a factory: from TeamDO configSnapshot (a list of resolved agent configs). */
export type AgentSource = (env: Env, teamId: string, factoryId: string) => Promise<AgentConfig[]>;

export const teamAgents: AgentSource = async (env, teamId, factoryId) => {
  const snap = (await env.TEAM.getByName(doName.team(teamId)).configSnapshot(factoryId)) as { agents?: AgentConfig[] } | null;
  return snap?.agents ?? [];
};

let agentSource: AgentSource = teamAgents;
export function setAgentSource(fn: AgentSource): void {
  agentSource = fn;
}

const TOOLS: { name: string; description: string; inputSchema: Record<string, unknown> }[] = [
  {
    name: "run_agents",
    description: "Start one or more configured factory agents as child runs. Each child reports back to you as a follow-up message when it finishes a turn.",
    inputSchema: {
      type: "object",
      properties: {
        summary: { type: "string" },
        base_prompt: { type: "string" },
        metadata: { type: "object", additionalProperties: { type: "string" } },
        agent_run_configs: {
          type: "array",
          items: {
            type: "object",
            properties: {
              name: { type: "string" },
              agent_identity_uid: { type: "string" },
              prompt: { type: "string" },
              metadata: { type: "object", additionalProperties: { type: "string" } },
            },
            required: ["agent_identity_uid"],
          },
        },
      },
      required: ["agent_run_configs"],
    },
  },
  { name: "list_agents", description: "List the factory agents you may start, with their UIDs, types and descriptions.", inputSchema: { type: "object", properties: {} } },
  { name: "follow_up_agent", description: "Send a follow-up message to one of your child runs.", inputSchema: obj({ run_id: "string", message: "string" }, ["run_id", "message"]) },
  { name: "send_message", description: "Send a message to an active run of your work item.", inputSchema: obj({ run_id: "string", message: "string" }, ["run_id", "message"]) },
  { name: "ask_human", description: "Ask the requester a question. End your turn afterwards; the answer arrives as a follow-up message.", inputSchema: obj({ question: "string", details: "string" }, ["question"]) },
  { name: "request_approval", description: "Request an explicit human approval before continuing.", inputSchema: obj({ title: "string", details: "string" }, ["title"]) },
  { name: "post_update", description: "Post a progress update (or, with kind=handoff, the finished result) to where the work item started.", inputSchema: obj({ message: "string", kind: "string" }, ["message"]) },
  { name: "set_stage", description: "Set the work item's stage.", inputSchema: obj({ stage: "string", reason: "string" }, ["stage"]) },
  { name: "link_pull_request", description: "Attach a pull request to the work item so its reviews and merge events are routed back.", inputSchema: obj({ key: "string", forge: "string", repo: "string", number: "integer", url: "string", branch: "string" }, ["key"]) },
  { name: "link_issue", description: "Attach a tracker issue to the work item.", inputSchema: obj({ key: "string", url: "string" }, ["key"]) },
  { name: "get_work_item", description: "Read the work item: stage, source, pull requests, open human requests, child runs.", inputSchema: { type: "object", properties: {} } },
  { name: "complete_work_item", description: "Mark the work item complete.", inputSchema: obj({ summary: "string" }, []) },
];

function obj(props: Record<string, string>, required: string[]): Record<string, unknown> {
  return { type: "object", properties: Object.fromEntries(Object.entries(props).map(([k, t]) => [k, { type: t }])), required };
}

interface JsonRpc {
  jsonrpc: "2.0";
  id?: string | number | null;
  method: string;
  params?: Record<string, unknown>;
}

const rpcResult = (id: JsonRpc["id"], result: unknown) => ({ jsonrpc: "2.0", id: id ?? null, result });
const rpcError = (id: JsonRpc["id"], code: number, message: string) => ({ jsonrpc: "2.0", id: id ?? null, error: { code, message } });

/** Handles POST /mcp/run/{runId}. Returns null for other paths. */
export async function handleRunMcp(request: Request, env: Env): Promise<Response | null> {
  const url = new URL(request.url);
  if (!url.pathname.startsWith(RUN_MCP_PREFIX)) return null;
  const runId = decodeURIComponent(url.pathname.slice(RUN_MCP_PREFIX.length).replace(/\/+$/, ""));
  if (request.method === "GET") return new Response(null, { status: 405, headers: { allow: "POST" } });
  if (request.method !== "POST") return new Response(null, { status: 405, headers: { allow: "POST" } });
  const auth = request.headers.get("authorization") ?? "";
  const claims = auth.startsWith("Bearer ") ? await verifySessionToken(await runnerKeys(env), auth.slice(7).trim()) : undefined;
  if (!claims || claims.run !== runId) {
    return Response.json({ error: "invalid session token" }, { status: 401, headers: { "www-authenticate": "Bearer" } });
  }
  let msg: JsonRpc | JsonRpc[];
  try {
    msg = (await request.json()) as JsonRpc | JsonRpc[];
  } catch {
    return Response.json(rpcError(null, -32700, "parse error"), { status: 400 });
  }
  const one = async (m: JsonRpc) => {
    try {
      return await dispatch(env, claims.team, runId, m);
    } catch (e) {
      const { error, known } = toOfError(e);
      if (!known) console.error("orchestration: internal error", m.method, e);
      return rpcError(m.id, -32603, known ? error.message : "internal error");
    }
  };
  if (Array.isArray(msg)) return Response.json((await Promise.all(msg.map(one))).filter((r) => r !== null));
  const out = await one(msg);
  if (out === null) return new Response(null, { status: 202 });
  return Response.json(out);
}

async function dispatch(env: Env, teamId: string, runId: string, m: JsonRpc): Promise<unknown> {
  switch (m.method) {
    case "initialize":
      return rpcResult(m.id, {
        protocolVersion: PROTOCOL_VERSION,
        capabilities: { tools: { listChanged: false } },
        serverInfo: { name: "openfactory-orchestration", version: "1.0.0" },
      });
    case "notifications/initialized":
    case "notifications/cancelled":
      return null;
    case "ping":
      return rpcResult(m.id, {});
    case "tools/list":
      return rpcResult(m.id, { tools: TOOLS });
    case "tools/call": {
      const name = String(m.params?.name ?? "");
      const args = (m.params?.arguments ?? {}) as Record<string, unknown>;
      return rpcResult(m.id, await callTool(env, teamId, runId, name, args));
    }
  }
  return rpcError(m.id, -32601, `method ${m.method} not found`);
}

/** One tool call (also used by tests). */
export async function callTool(env: Env, teamId: string, runId: string, name: string, args: Record<string, unknown>): Promise<ToolResult> {
  const work = env.WORK.getByName(doName.work(teamId, partitionOf(runId)));
  if (name === "list_agents") {
    const run = (await work.getRun(runId)) as Run;
    const agents = await agentSource(env, teamId, run.factory_id);
    const caller = run.agent_name || run.config.name;
    const mayStart = agents.filter((a) => a.type !== "FOREMAN" && (a.spawnable_by ? a.spawnable_by.includes(caller) : run.config.type === "FOREMAN"));
    const children = (await work.api({ requestId: "mcp" }, "listRunChildren", { runId }, null)) as { runs: Run[] };
    return text({
      agents: mayStart.map((a) => ({ agent_identity_uid: a.id, name: a.name, type: a.type, description: a.description, harness: a.harness?.type ?? "", model: a.harness?.model ?? "" })),
      active_runs: children.runs.filter((r) => r.state !== "cancelled").map((r) => ({ run_id: r.id, name: r.name, state: r.state })),
    });
  }
  if (!TOOLS.some((t) => t.name === name)) return { content: [{ type: "text", text: `unknown tool ${name}` }], isError: true };
  const turnId = await work.activeTurn(runId);
  const key = turnId ? await toolIdempotencyKey(runId, turnId, name, args) : "";
  let plans: ChildPlan[] | undefined;
  if (name === "run_agents") {
    try {
      plans = await resolvePlans(env, teamId, runId, args);
    } catch (e) {
      return { content: [{ type: "text", text: toOfError(e).error.message }], isError: true };
    }
  }
  if ((name === "link_pull_request" || name === "link_issue") && typeof args.key === "string") {
    const run = (await work.getRun(runId)) as Run;
    if (run.work_item_id) {
      const claim = await env.ROUTER.getByName(doName.router(teamId)).addReference({
        factoryId: run.factory_id,
        key: args.key,
        kind: name === "link_pull_request" ? "pull_request" : "issue",
        wiId: run.work_item_id,
        token: partitionOf(runId),
      });
      if (!claim.ok) return { content: [{ type: "text", text: `${args.key} belongs to work item ${claim.ownerWiId}` }], isError: true };
    }
  }
  return (await work.toolCall({ runId, tool: name, input: args, key, turnId, ...(plans ? { plans } : {}) })) as ToolResult;
}

const text = (v: unknown): ToolResult => ({ content: [{ type: "text", text: JSON.stringify(v) }] });

async function resolvePlans(env: Env, teamId: string, runId: string, args: Record<string, unknown>): Promise<ChildPlan[]> {
  const work = env.WORK.getByName(doName.work(teamId, partitionOf(runId)));
  const parent = (await work.getRun(runId)) as Run;
  const agents = await agentSource(env, teamId, parent.factory_id);
  const list = Array.isArray(args.agent_run_configs) ? (args.agent_run_configs as Record<string, unknown>[]) : [];
  if (list.length === 0) throw new OfError("invalid_request", "agent_run_configs must contain at least one child");
  const base = typeof args.base_prompt === "string" ? args.base_prompt.trim() : "";
  return list.map((c, i) => {
    const uid = String(c.agent_identity_uid ?? "").trim();
    if (!uid) throw new OfError("invalid_request", `agent_run_configs[${i}].agent_identity_uid is required`);
    // Forgiving when the model passes an agent name instead of its UID.
    const a = agents.find((x) => x.id === uid) ?? agents.find((x) => x.name === uid);
    if (!a) throw new OfError("invalid_request", `agent_run_configs[${i}]: agent ${JSON.stringify(uid)} does not exist in this factory; call list_agents for valid UIDs`);
    let prompt = String(c.prompt ?? "").trim();
    if (base) prompt = prompt ? `${base}\n\n${prompt}` : base;
    if (!prompt) throw new OfError("invalid_request", `agent_run_configs[${i}]: prompt (or base_prompt) is required`);
    const metadata = { ...(parent.metadata ?? {}), ...((args.metadata as Record<string, string>) ?? {}), ...((c.metadata as Record<string, string>) ?? {}) };
    return { config: a, name: String(c.name ?? "").trim() || a.name, prompt, ...(Object.keys(metadata).length ? { metadata } : {}) };
  });
}
