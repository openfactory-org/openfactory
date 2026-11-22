// The per-run factory MCP server (/mcp/run/{runId}, internal/orchestration):
// stateless JSON-RPC in the Worker. Each tool is one WorkDO RPC; the tool
// idempotency row commits with the effect (C8). The key hashes canonical JSON
// of the input in the active turn (plan rule 3); errors are not stored.
import { doName, isTrialRun, newId, newRoutedId, OfError, partitionOf, sourceKey, toOfError } from "@openfactory/core";
import type { Env } from "../contracts/env";
import { toolIdempotencyKey } from "../engine/canonical";
import { isTerminal, type AgentConfig, type Run } from "../engine/types";
import type { ChildPlan } from "../do/work/engine";
import type { ToolResult } from "../do/work/tools";
import { plannerFor } from "../runner-api/planner";
import { runnerKeys, verifySessionToken } from "../runner-api/token";
import { redact } from "../secrets/expand";
import { applyOverrides } from "../scoring/overrides";
import { META_SELF_IMPROVEMENT, type Overrides } from "../scoring/types";
import { proposalBody, proposalTitle } from "../scoring/proposals";
import { unwrapOp } from "../lib/rpc";
import { systemPrincipal } from "../scoring/clients";
import { issueRef, type LinkIssueInput } from "./linkissue";
import { prReference, prRef, type LinkPullRequestInput } from "./linkpr";
import { isToolName, TOOLS, type ToolName } from "./schemas";

export const RUN_MCP_PREFIX = "/mcp/run/";
const PROTOCOL_VERSION = "2025-06-18";

/** The parts of the TeamDO configSnapshot of a factory that the tools read. */
export interface FactoryConfig {
  agents: AgentConfig[];
  /** The machine types of the factory runtime; undefined when the source does not know them. */
  machines?: Record<string, unknown> | null;
}

/**
 * Agents of a factory: from TeamDO configSnapshot. A source that returns only
 * the agents (tests) lets run_agents read the machine types from TeamDO when
 * an agent pin needs them.
 */
export type AgentSource = (env: Env, teamId: string, factoryId: string) => Promise<FactoryConfig | AgentConfig[]>;

interface ConfigSnapshot {
  agents?: AgentConfig[];
  runtime?: { machines?: Record<string, unknown> | null };
}

async function configSnapshot(env: Env, teamId: string, factoryId: string): Promise<ConfigSnapshot | null> {
  return (await env.TEAM.getByName(doName.team(teamId)).configSnapshot(factoryId)) as ConfigSnapshot | null;
}

/** The agents and the machine types of a factory from one configSnapshot. */
export const teamAgents: AgentSource = async (env, teamId, factoryId) => {
  const snap = await configSnapshot(env, teamId, factoryId);
  return { agents: snap?.agents ?? [], machines: snap?.runtime?.machines ?? null };
};

async function factoryConfig(env: Env, teamId: string, factoryId: string): Promise<FactoryConfig> {
  const out = await agentSource(env, teamId, factoryId);
  return Array.isArray(out) ? { agents: out } : out;
}

let agentSource: AgentSource = teamAgents;
export function setAgentSource(fn: AgentSource): void {
  agentSource = fn;
}

/**
 * Removes the secret values of a run from text that an agent posts
 * (engine.Planner.RedactFor). The text of post_update, ask_human,
 * request_approval, send_message and follow_up_agent goes through it.
 */
export type Redactor = (env: Env, run: Run) => Promise<(s: string) => string>;

export const plannerRedactor: Redactor = async (env, run) => {
  const values = await plannerFor(env, run).redactValues(run);
  return (s) => redact(s, values);
};

let redactor: Redactor = plannerRedactor;
export function setRedactor(fn: Redactor): void {
  redactor = fn;
}

/** The argument fields that each tool posts as text, redacted before the WorkDO call. */
const REDACTED_FIELDS: Partial<Record<ToolName, string[]>> = {
  post_update: ["text"],
  ask_human: ["question", "title", "summary", "details", "body", "context", "options", "recommended_option"],
  request_approval: ["question", "title", "summary", "details", "body", "options", "links"],
  send_message: ["message", "text"],
  follow_up_agent: ["message"],
};

async function redactArgs(
  env: Env,
  work: { getRun(id: string): Promise<unknown> },
  runId: string,
  fields: string[],
  args: Record<string, unknown>,
): Promise<Record<string, unknown>> {
  if (!fields.some((f) => args[f] !== undefined)) return args;
  let rd: (s: string) => string;
  try {
    rd = await redactor(env, (await work.getRun(runId)) as Run);
  } catch {
    return args;
  }
  const out = { ...args };
  for (const f of fields) {
    const v = out[f];
    if (typeof v === "string") out[f] = rd(v);
    else if (Array.isArray(v)) out[f] = v.map((x) => (typeof x === "string" ? rd(x) : x));
  }
  return out;
}

interface JsonRpc {
  jsonrpc: "2.0";
  id?: string | number | null;
  method: string;
  params?: Record<string, unknown>;
}

const rpcResult = (id: JsonRpc["id"], result: unknown) => ({ jsonrpc: "2.0", id: id ?? null, result });
const rpcError = (id: JsonRpc["id"], code: number, message: string) => ({
  jsonrpc: "2.0",
  id: id ?? null,
  error: { code, message },
});

/** Handles POST /mcp/run/{runId}. Returns null for other paths. */
export async function handleRunMcp(request: Request, env: Env): Promise<Response | null> {
  const url = new URL(request.url);
  if (!url.pathname.startsWith(RUN_MCP_PREFIX)) return null;
  const runId = decodeURIComponent(url.pathname.slice(RUN_MCP_PREFIX.length).replace(/\/+$/, ""));
  if (request.method === "GET") return new Response(null, { status: 405, headers: { allow: "POST" } });
  if (request.method !== "POST") return new Response(null, { status: 405, headers: { allow: "POST" } });
  const auth = request.headers.get("authorization") ?? "";
  const claims = auth.startsWith("Bearer ")
    ? await verifySessionToken(await runnerKeys(env), auth.slice(7).trim())
    : undefined;
  if (!claims || claims.run !== runId) {
    return Response.json(
      { error: "invalid session token" },
      { status: 401, headers: { "www-authenticate": "Bearer" } },
    );
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

/** What a tool handler of the Worker gets. */
interface ToolCtx {
  env: Env;
  teamId: string;
  runId: string;
  name: ToolName;
  work: ReturnType<Env["WORK"]["getByName"]>;
  /** The active turn of the run; "" without one. */
  turnId: string;
  /** The idempotency key of the call in the active turn; "" without a turn. */
  key: string;
}

/** A tool in the Worker: it answers the call, or prepares it and calls WorkDO. */
type ToolHandler = (c: ToolCtx, args: Record<string, unknown>) => Promise<ToolResult>;

/** Tools that change nothing: no turn key and no WorkDO idempotency row. */
const UNKEYED = new Set<ToolName>(["list_agents"]);

/** One tool call (also used by tests). */
export async function callTool(
  env: Env,
  teamId: string,
  runId: string,
  name: string,
  args: Record<string, unknown>,
): Promise<ToolResult> {
  if (!isToolName(name)) return toolError(`unknown tool ${name}`);
  const work = env.WORK.getByName(doName.work(teamId, partitionOf(runId)));
  const c: ToolCtx = { env, teamId, runId, name, work, turnId: "", key: "" };
  if (!UNKEYED.has(name)) {
    // The key hashes the input as the model sent it, before redaction.
    c.turnId = await work.activeTurn(runId);
    c.key = c.turnId ? await toolIdempotencyKey(runId, c.turnId, name, args) : "";
  }
  const fields = REDACTED_FIELDS[name];
  if (fields) args = await redactArgs(env, work, runId, fields, args);
  return TOOL_HANDLERS[name](c, args);
}

/** The WorkDO call of a tool: the effect and its idempotency row in one transaction. */
async function forward(c: ToolCtx, args: Record<string, unknown>, plans?: ChildPlan[]): Promise<ToolResult> {
  return (await c.work.toolCall({
    runId: c.runId,
    tool: c.name,
    input: args,
    key: c.key,
    turnId: c.turnId,
    ...(plans ? { plans } : {}),
  })) as ToolResult;
}

const TOOL_HANDLERS: Record<ToolName, ToolHandler> = {
  run_agents: async (c, args) => {
    let plans: ChildPlan[];
    try {
      plans = await resolvePlans(c, args);
    } catch (e) {
      return toolError(toOfError(e).error.message);
    }
    return forward(c, args, plans);
  },
  list_agents: listAgents,
  follow_up_agent: forward,
  send_message: forward,
  ask_human: forward,
  request_approval: forward,
  post_update: forward,
  set_stage: forward,
  link_pull_request: async (c, args) => {
    // linkPR: the Worker reads the pull request (linkpr.ts); the WorkDO stores
    // it with its normalized reference. Without a repo and a number there is
    // no reference key (Go adds no alias then).
    let input: Record<string, unknown>;
    try {
      const pr = prRef(args as LinkPullRequestInput);
      const ref = prReference(pr);
      input = { pull_request: pr, ...(ref ? { key: ref.key, source: ref.source } : {}) };
    } catch (e) {
      return toolError(toOfError(e).error.message);
    }
    return (await claimReference(c, input, "pull_request")) ?? forward(c, input);
  },
  link_issue: async (c, args) => {
    let input: Record<string, unknown>;
    try {
      const src = issueRef(args as LinkIssueInput);
      input = { ...args, key: sourceKey(src), source: src };
    } catch (e) {
      return toolError(toOfError(e).error.message);
    }
    return (await claimReference(c, input, "issue")) ?? forward(c, input);
  },
  propose_definition_change: (c, args) => proposeDefinitionChange(c.env, c.teamId, c.runId, args, c.key),
  get_work_item: forward,
  complete_work_item: forward,
};

/**
 * list_agents: the agents that the run may start (TeamDO config) and the runs
 * that it can send a message to (activeTargets in sendmessage.go).
 */
async function listAgents(c: ToolCtx): Promise<ToolResult> {
  const run = (await c.work.getRun(c.runId)) as Run;
  const { agents } = await factoryConfig(c.env, c.teamId, run.factory_id);
  const caller = run.agent_name || run.config.name;
  const mayStart = agents.filter(
    (a) => a.type !== "FOREMAN" && (a.spawnable_by ? a.spawnable_by.includes(caller) : run.config.type === "FOREMAN"),
  );
  const targets = await forward(c, {});
  if (targets.isError) return targets;
  const { active_runs } = JSON.parse(targets.content[0]!.text) as { active_runs: unknown[] };
  return text({
    agents: mayStart.map((a) => ({
      agent_identity_uid: a.id,
      name: a.name,
      type: a.type,
      description: a.description,
      harness: a.harness?.type ?? "",
      model: a.harness?.model ?? "",
    })),
    active_runs,
  });
}

/**
 * Claims the reference key of a linked pull request or issue for the work
 * item of the run at RouterDO. Returns an error result when another work item
 * owns the key, undefined otherwise.
 */
async function claimReference(
  c: ToolCtx,
  input: Record<string, unknown>,
  kind: "pull_request" | "issue",
): Promise<ToolResult | undefined> {
  if (typeof input.key !== "string") return undefined;
  const run = (await c.work.getRun(c.runId)) as Run;
  if (!run.work_item_id) return undefined;
  const claim = await c.env.ROUTER.getByName(doName.router(c.teamId)).addReference({
    factoryId: run.factory_id,
    key: input.key,
    kind,
    wiId: run.work_item_id,
    token: partitionOf(c.runId),
  });
  return claim.ok ? undefined : toolError(`${input.key} belongs to work item ${claim.ownerWiId}`);
}

const text = (v: unknown): ToolResult => ({ content: [{ type: "text", text: JSON.stringify(v) }] });
const toolError = (message: string): ToolResult => ({ content: [{ type: "text", text: message }], isError: true });

/**
 * propose_definition_change (proposeDefinitionChange in tools.go and
 * scoring.Service.ProposeDefinitionChange): TeamDO checks and stores the
 * proposed version, then the WorkDO of the run gets the review inbox item.
 * The item ID is chosen first so that the version names it; a repeated call
 * with the same tool key returns the same version and the same item.
 */
async function proposeDefinitionChange(
  env: Env,
  teamId: string,
  runId: string,
  args: Record<string, unknown>,
  key: string,
): Promise<ToolResult> {
  const token = partitionOf(runId);
  const work = env.WORK.getByName(doName.work(teamId, token));
  const run = (await work.getRun(runId)) as Run;
  if (isTerminal(run.state)) return toolError(`run ${runId} is ${run.state}; it cannot propose a definition change`);
  if (isTrialRun({ metadata: run.metadata ?? {} }))
    return toolError("a benchmark trial cannot propose a definition change");
  const files: Record<string, string> = {};
  if (args.files !== undefined && (typeof args.files !== "object" || args.files === null || Array.isArray(args.files)))
    return toolError("files must be an object of path to content");
  for (const [p, c] of Object.entries((args.files ?? {}) as Record<string, unknown>)) {
    if (typeof c !== "string") return toolError(`files[${JSON.stringify(p)}] must be a string`);
    files[p] = c;
  }
  const del = Array.isArray(args.delete) ? args.delete.filter((x): x is string => typeof x === "string") : [];
  const message = typeof args.message === "string" ? args.message : "";
  const meta = { requestId: "propose", principal: { ...systemPrincipal(teamId), userId: "run:" + runId } };
  try {
    const out = (await env.TEAM.getByName(doName.team(teamId)).proposeDefinition(meta, {
      factoryId: run.factory_id,
      files,
      delete: del,
      message,
      authorRunId: runId,
      idempotencyKey: key || `propose:${runId}:${newId("k")}`,
      inboxItemId: newRoutedId("inb", token),
      ...(run.work_item_id ? { workItemId: run.work_item_id } : {}),
      selfImprovement: run.metadata?.[META_SELF_IMPROVEMENT] === "true",
    })) as {
      version: { id: string; base_version_id?: string; message?: string; inbox_item_id?: string };
      diff: string;
      reviewer_id?: string;
    };
    const v = out.version;
    unwrapOp(
      await work.api(
        meta,
        "createInboxItem",
        {},
        {
          id: v.inbox_item_id,
          track_key: `proposal:${v.id}`,
          factory_id: run.factory_id,
          ...(run.work_item_id ? { work_item_id: run.work_item_id } : {}),
          run_id: runId,
          kind: "review",
          title: proposalTitle(v.message ?? message),
          body: proposalBody(
            {
              id: v.id,
              author_run_id: runId,
              message: v.message ?? message,
              base_version_id: v.base_version_id ?? "",
            },
            out.diff,
          ),
          options: ["approve", "reject"],
          ...(out.reviewer_id ? { assignee_id: out.reviewer_id } : {}),
        },
      ),
    );
    return {
      content: [
        {
          type: "text",
          text: `Proposed version ${v.id} is stored and waits for review (base version ${v.base_version_id ?? ""}). End your turn; the decision arrives as a message.`,
        },
      ],
    };
  } catch (e) {
    return toolError(toOfError(e).error.message);
  }
}

/**
 * The children of run_agents from the agents of the factory. The agents and
 * the machine types of the pins come from one TeamDO configSnapshot.
 */
async function resolvePlans(c: ToolCtx, args: Record<string, unknown>): Promise<ChildPlan[]> {
  const { env, teamId } = c;
  const parent = (await c.work.getRun(c.runId)) as Run;
  const cfg = await factoryConfig(env, teamId, parent.factory_id);
  const agents = cfg.agents;
  const list = Array.isArray(args.agent_run_configs) ? (args.agent_run_configs as Record<string, unknown>[]) : [];
  if (list.length === 0) throw new OfError("invalid_request", "agent_run_configs must contain at least one child");
  const base = typeof args.base_prompt === "string" ? args.base_prompt.trim() : "";
  const pins = await agentPinsOf(env, teamId, parent);
  const plans = list.map((c, i) => {
    const uid = String(c.agent_identity_uid ?? "").trim();
    if (!uid) throw new OfError("invalid_request", `agent_run_configs[${i}].agent_identity_uid is required`);
    // Forgiving when the model passes an agent name instead of its UID.
    const a = agents.find((x) => x.id === uid) ?? agents.find((x) => x.name === uid);
    if (!a)
      throw new OfError(
        "invalid_request",
        `agent_run_configs[${i}]: agent ${JSON.stringify(uid)} does not exist in this factory; call list_agents for valid UIDs`,
      );
    let prompt = String(c.prompt ?? "").trim();
    if (base) prompt = prompt ? `${base}\n\n${prompt}` : base;
    if (!prompt) throw new OfError("invalid_request", `agent_run_configs[${i}]: prompt (or base_prompt) is required`);
    const metadata = {
      ...(parent.metadata ?? {}),
      ...((args.metadata as Record<string, string>) ?? {}),
      ...((c.metadata as Record<string, string>) ?? {}),
    };
    return {
      config: a,
      name: String(c.name ?? "").trim() || a.name,
      prompt,
      ...(Object.keys(metadata).length ? { metadata } : {}),
    };
  });
  if (Object.keys(pins).length === 0 || !plans.some((p) => pins[p.config.name])) return plans;
  // Pins (benchmark configurations) win over the configuration of the agent (RunAgents in engine/runs.go).
  const machines =
    cfg.machines !== undefined
      ? cfg.machines
      : ((await configSnapshot(env, teamId, parent.factory_id))?.runtime?.machines ?? null);
  return plans.map((p, i) => {
    const pin = pins[p.config.name];
    if (!pin) return p;
    try {
      return {
        ...p,
        config: applyOverrides(p.config as never, pin, machines as never) as unknown as AgentConfig,
      };
    } catch (e) {
      throw new OfError("invalid_request", `agent_run_configs[${i}]: pin: ${toOfError(e).error.message}`);
    }
  });
}

/** The agent pins of the parent's work item (a benchmark trial), or none. */
async function agentPinsOf(env: Env, teamId: string, parent: Run): Promise<Record<string, Overrides>> {
  if (!parent.work_item_id) return {};
  const work = env.WORK.getByName(doName.work(teamId, partitionOf(parent.id)));
  const out = (await work.api({ requestId: "pins" }, "getAgentPins", { id: parent.work_item_id }, null)) as {
    pins?: Record<string, Overrides>;
  };
  return out?.pins ?? {};
}
