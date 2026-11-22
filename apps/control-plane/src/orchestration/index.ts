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
import { systemPrincipal, unwrapOp } from "../scoring/clients";
import { issueRef, type LinkIssueInput } from "./linkissue";
import { prReference, prRef, type LinkPullRequestInput } from "./linkpr";

export const RUN_MCP_PREFIX = "/mcp/run/";
const PROTOCOL_VERSION = "2025-06-18";

/** Agents of a factory: from TeamDO configSnapshot (a list of resolved agent configs). */
export type AgentSource = (env: Env, teamId: string, factoryId: string) => Promise<AgentConfig[]>;

export const teamAgents: AgentSource = async (env, teamId, factoryId) => {
  const snap = (await env.TEAM.getByName(doName.team(teamId)).configSnapshot(factoryId)) as {
    agents?: AgentConfig[];
  } | null;
  return snap?.agents ?? [];
};

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
const REDACTED_FIELDS: Record<string, string[]> = {
  post_update: ["message", "text"],
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

const TOOLS: { name: string; description: string; inputSchema: Record<string, unknown> }[] = [
  {
    name: "run_agents",
    description:
      "Start one or more configured factory agents as child runs. Each child reports back to you as a follow-up message when it finishes a turn.",
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
  {
    name: "list_agents",
    description: "List the factory agents you may start, with their UIDs, types and descriptions.",
    inputSchema: { type: "object", properties: {} },
  },
  {
    name: "follow_up_agent",
    description: "Send a follow-up message to one of your child runs.",
    inputSchema: obj({ run_id: "string", message: "string" }, ["run_id", "message"]),
  },
  {
    name: "send_message",
    description:
      "Send a message to an active run of your work item: your parent, a child, a sibling or a side run. The target gets it as an agent message from your run. Find targets in active_runs of list_agents.",
    inputSchema: obj({ run_id: "string", text: "string" }, ["run_id", "text"]),
  },
  {
    name: "ask_human",
    description: "Ask the requester a question. End your turn afterwards; the answer arrives as a follow-up message.",
    inputSchema: {
      type: "object",
      properties: {
        question: { type: "string", description: "The question for the requester" },
        context: { type: "string", description: "Background needed to answer" },
        options: { type: "array", items: { type: "string" }, description: "Optional suggested answers" },
        multi_select: { type: "boolean", description: "The person can pick more than one option" },
        recommended_option: { type: "string", description: "The option you recommend. It must be one of options" },
      },
      required: ["question"],
    },
  },
  {
    name: "request_approval",
    description: "Request an explicit human approval before continuing.",
    inputSchema: {
      type: "object",
      properties: {
        title: { type: "string", description: "What needs approval, e.g. 'Spec: purchase-order field'" },
        summary: { type: "string", description: "What is being approved and why" },
        links: {
          type: "array",
          items: { type: "string" },
          description: "Links to review (spec pull request, documents)",
        },
        options: { type: "array", items: { type: "string" }, description: "Decision options (default approve/reject)" },
      },
      required: ["title"],
    },
  },
  {
    name: "post_update",
    description: "Post a progress update (or, with kind=handoff, the finished result) to where the work item started.",
    inputSchema: obj({ message: "string", kind: "string" }, ["message"]),
  },
  {
    name: "set_stage",
    description: "Set the work item's stage.",
    inputSchema: obj({ stage: "string", reason: "string" }, ["stage"]),
  },
  {
    name: "link_pull_request",
    description: "Attach a pull request to the work item so its reviews and merge events are routed back.",
    inputSchema: {
      type: "object",
      properties: {
        url: { type: "string", description: "Pull request (merge request) URL" },
        repo: { type: "string", description: "owner/name; derived from the URL when omitted" },
        number: { type: "integer", description: "Pull request number; derived from the URL when omitted" },
        branch: { type: "string", description: "Head branch" },
        forge: { type: "string", description: "GITHUB, GITLAB or AZURE_DEVOPS; derived from the URL when omitted" },
        state: { type: "string", description: "open (default), draft, merged or closed" },
      },
      required: [],
    },
  },
  {
    name: "link_issue",
    description:
      "Attach a tracker issue to the work item. Give its URL (Linear, Jira, GitHub, GitLab or an Azure DevOps work item), or a provider with a key (or a repo and a number).",
    inputSchema: obj({ url: "string", provider: "string", key: "string", repo: "string", number: "integer" }, []),
  },
  {
    name: "propose_definition_change",
    description:
      "Propose a change to the managed factory definition for human review. The files are checked with the compiler and the preflight; errors come back so you can fix them. A valid proposal is stored and a reviewer sees its diff in the inbox. An approval applies it; a rejection comes back to you as a message. For a definition in git, open a pull request instead.",
    inputSchema: {
      type: "object",
      properties: {
        files: {
          type: "object",
          additionalProperties: { type: "string" },
          description: "Path relative to the definition root to the full new content of the file.",
        },
        delete: { type: "array", items: { type: "string" }, description: "Paths to remove." },
        message: { type: "string", description: "A short description of the change." },
      },
      required: ["message"],
    },
  },
  {
    name: "get_work_item",
    description: "Read the work item: stage, source, pull requests, open human requests, child runs.",
    inputSchema: { type: "object", properties: {} },
  },
  {
    name: "complete_work_item",
    description: "Mark the work item complete.",
    inputSchema: obj({ summary: "string" }, []),
  },
];

function obj(props: Record<string, string>, required: string[]): Record<string, unknown> {
  return {
    type: "object",
    properties: Object.fromEntries(Object.entries(props).map(([k, t]) => [k, { type: t }])),
    required,
  };
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

/** One tool call (also used by tests). */
export async function callTool(
  env: Env,
  teamId: string,
  runId: string,
  name: string,
  args: Record<string, unknown>,
): Promise<ToolResult> {
  const work = env.WORK.getByName(doName.work(teamId, partitionOf(runId)));
  if (name === "list_agents") {
    const run = (await work.getRun(runId)) as Run;
    const agents = await agentSource(env, teamId, run.factory_id);
    const caller = run.agent_name || run.config.name;
    const mayStart = agents.filter(
      (a) => a.type !== "FOREMAN" && (a.spawnable_by ? a.spawnable_by.includes(caller) : run.config.type === "FOREMAN"),
    );
    // The runs that this run can send a message to (activeTargets in sendmessage.go).
    const targets = (await work.toolCall({ runId, tool: "list_agents", input: {}, key: "", turnId: "" })) as ToolResult;
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
  if (!TOOLS.some((t) => t.name === name))
    return { content: [{ type: "text", text: `unknown tool ${name}` }], isError: true };
  const turnId = await work.activeTurn(runId);
  const key = turnId ? await toolIdempotencyKey(runId, turnId, name, args) : "";
  const fields = REDACTED_FIELDS[name];
  if (fields) args = await redactArgs(env, work, runId, fields, args);
  if (name === "link_issue") {
    try {
      const src = issueRef(args as LinkIssueInput);
      args = { ...args, key: sourceKey(src), source: src };
    } catch (e) {
      return { content: [{ type: "text", text: toOfError(e).error.message }], isError: true };
    }
  }
  if (name === "link_pull_request") {
    // linkPR: the Worker reads the pull request (linkpr.ts); the WorkDO stores
    // it with its normalized reference. Without a repo and a number there is
    // no reference key (Go adds no alias then).
    try {
      const pr = prRef(args as LinkPullRequestInput);
      const ref = prReference(pr);
      args = { pull_request: pr, ...(ref ? { key: ref.key, source: ref.source } : {}) };
    } catch (e) {
      return { content: [{ type: "text", text: toOfError(e).error.message }], isError: true };
    }
  }
  if (name === "propose_definition_change") return proposeDefinitionChange(env, teamId, runId, args, key);
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
      if (!claim.ok)
        return {
          content: [{ type: "text", text: `${args.key} belongs to work item ${claim.ownerWiId}` }],
          isError: true,
        };
    }
  }
  return (await work.toolCall({
    runId,
    tool: name,
    input: args,
    key,
    turnId,
    ...(plans ? { plans } : {}),
  })) as ToolResult;
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

async function resolvePlans(
  env: Env,
  teamId: string,
  runId: string,
  args: Record<string, unknown>,
): Promise<ChildPlan[]> {
  const work = env.WORK.getByName(doName.work(teamId, partitionOf(runId)));
  const parent = (await work.getRun(runId)) as Run;
  const agents = await agentSource(env, teamId, parent.factory_id);
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
  const snap = (await env.TEAM.getByName(doName.team(teamId)).configSnapshot(parent.factory_id)) as {
    runtime?: { machines?: Record<string, unknown> | null };
  } | null;
  return plans.map((p, i) => {
    const pin = pins[p.config.name];
    if (!pin) return p;
    try {
      return {
        ...p,
        config: applyOverrides(
          p.config as never,
          pin,
          (snap?.runtime?.machines ?? null) as never,
        ) as unknown as AgentConfig,
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
