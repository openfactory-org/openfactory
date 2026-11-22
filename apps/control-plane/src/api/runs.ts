// REST handlers: runs. Owner: WS5. The Worker authenticates; the WorkDO of
// the run (by its partition token) owns it; lists come from IndexDO.
import { doName, invalid, newRoutedId, newToken, notFound, OfError, partitionOf } from "@openfactory/core";
import type { CallMeta } from "../contracts/do-messages";
import type { Env } from "../contracts/env";
import { registerCronHook, registerServerRoute } from "../builder";
import { runView } from "../engine/view";
import type { AgentConfig, Run, RunMessage, RunSession } from "../engine/types";
import { notImplementedHandler, type Handler, type RouteContext } from "../http/handler";
import { handleRunMcp, RUN_MCP_PREFIX } from "../orchestration/index";
import { handleRunnerApi, RUNNER_PREFIX } from "../runner-api/index";
import { sseResponse, subscribe } from "../stream/index";

// ---------- server routes and cron owned by WS5 ----------

registerServerRoute(RUNNER_PREFIX + "/", (req, env, ctx) => handleRunnerApi(req, env, ctx));
registerServerRoute(RUN_MCP_PREFIX, (req, env) => handleRunMcp(req, env));
registerCronHook(async (env) => {
  await env.DISPATCH.getByName(doName.dispatch(teamOfEnv())).ensureAlarm();
});

function teamOfEnv(): string {
  return "default";
}

// ---------- helpers ----------

export function teamOf(ctx: RouteContext): string {
  return ctx.principal?.teamId || "default";
}

export function metaOf(ctx: RouteContext): CallMeta {
  return { requestId: ctx.requestId, ...(ctx.principal ? { principal: ctx.principal } : {}) };
}

export function workFor(env: Env, ctx: RouteContext, id: string) {
  return env.WORK.getByName(doName.work(teamOf(ctx), partitionOf(id)));
}

export function json(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
}

async function body<T>(req: Request): Promise<T> {
  const t = await req.text();
  if (!t.trim()) return {} as T;
  try {
    return JSON.parse(t) as T;
  } catch {
    throw invalid("request body is not valid JSON");
  }
}

function publicUrl(env: Env): string {
  return (env.OF_PUBLIC_URL ?? "").replace(/\/+$/, "");
}

async function viewOf(env: Env, ctx: RouteContext, run: Run): Promise<Record<string, unknown>> {
  const sessions = (await workFor(env, ctx, run.id).api(metaOf(ctx), "listRunSessions", { runId: run.id }, null)) as { sessions: RunSession[] };
  return runView(run, { publicUrl: publicUrl(env), latest: sessions.sessions.at(-1) });
}

/** Factory and agent from TeamDO (configSnapshot: { factory: {id, team_id}, agents: AgentConfig[] }). */
async function resolveAgent(env: Env, ctx: RouteContext, factoryRef: string, agentRef: string): Promise<{ factoryId: string; agent: AgentConfig }> {
  const snap = (await env.TEAM.getByName(doName.team(teamOf(ctx))).configSnapshot(factoryRef)) as {
    factory?: { id: string };
    agents?: AgentConfig[];
  } | null;
  if (!snap?.factory) throw notFound(`factory ${factoryRef}`);
  const agent = (snap.agents ?? []).find((a) => a.id === agentRef || a.name === agentRef);
  if (!agent) throw invalid(`agent ${JSON.stringify(agentRef)} does not exist in factory ${factoryRef}`);
  return { factoryId: snap.factory.id, agent };
}

function actorOf(ctx: RouteContext): string {
  return ctx.principal?.userId || ctx.principal?.id || "api";
}

// ---------- handlers ----------

interface CreateRunBody {
  prompt?: string;
  title?: string;
  agent_identity_uid?: string;
  factory_id?: string;
  agent_name?: string;
  metadata?: Record<string, string>;
}

/** POST /api/v1/runs: a run of an agent: a new work item with the run as its root. */
export const createRun: Handler = async (req, env, ctx) => {
  const b = await body<CreateRunBody>(req);
  if (!b.prompt?.trim()) throw invalid("prompt is required");
  const factoryRef = b.factory_id ?? "";
  const agentRef = b.agent_identity_uid || b.agent_name || "";
  if (!agentRef || (!factoryRef && !b.agent_identity_uid)) throw invalid("set agent_identity_uid, or factory_id and agent_name");
  const { factoryId, agent } = await resolveAgent(env, ctx, factoryRef || b.agent_identity_uid!, agentRef);
  const token = newToken("w");
  const out = (await env.WORK.getByName(doName.work(teamOf(ctx), token)).runsCreate(metaOf(ctx), {
    dispatch: {
      factoryId,
      config: agent,
      prompt: b.prompt,
      actor: actorOf(ctx),
      title: b.title || b.prompt.split("\n")[0]!.slice(0, 120),
      workItemId: newRoutedId("wi", token),
      triggeredBy: "api",
      ...(b.metadata ? { metadata: b.metadata } : {}),
      ...(ctx.principal?.userId ? { creatorId: ctx.principal.userId } : {}),
    },
  })) as { run: Run };
  return json(201, runView(out.run, { publicUrl: publicUrl(env) }));
};

/** POST /api/v1/factories/{uid}/runs: dispatch to the factory's foreman. */
export const dispatchRun: Handler = async (req, env, ctx) => {
  const b = await body<CreateRunBody & { agent?: string }>(req);
  if (!b.prompt?.trim()) throw invalid("prompt is required");
  const { factoryId, agent } = await resolveAgent(env, ctx, ctx.params.uid ?? "", b.agent || b.agent_name || "foreman");
  const token = newToken("w");
  const out = (await env.WORK.getByName(doName.work(teamOf(ctx), token)).runsCreate(metaOf(ctx), {
    dispatch: {
      factoryId,
      config: agent,
      prompt: b.prompt,
      actor: actorOf(ctx),
      title: b.title || b.prompt.split("\n")[0]!.slice(0, 120),
      workItemId: newRoutedId("wi", token),
      triggeredBy: "api",
      ...(b.metadata ? { metadata: b.metadata } : {}),
    },
  })) as { run: Run; workItemId?: string };
  return json(201, { ...runView(out.run, { publicUrl: publicUrl(env) }), work_item_id: out.workItemId });
};

/** GET /api/v1/runs: IndexDO read model; a parent_run_id filter reads the run tree's WorkDO. */
export const listRuns: Handler = async (_req, env, ctx) => {
  const q = Object.fromEntries(ctx.url.searchParams);
  if (q.parent_run_id) {
    const res = (await workFor(env, ctx, q.parent_run_id).api(metaOf(ctx), "listRunChildren", { runId: q.parent_run_id }, null)) as { runs: Run[] };
    return json(200, { items: res.runs.map((r) => runView(r, { publicUrl: publicUrl(env) })), page_info: { has_next_page: false } });
  }
  const res = await env.INDEX.getByName(doName.index(teamOf(ctx))).list(metaOf(ctx), "runs", q);
  return json(200, res);
};

/** GET /api/v1/runs/{runId} */
export const getRun: Handler = async (_req, env, ctx) => {
  const run = (await workFor(env, ctx, ctx.params.runId ?? "").getRun(ctx.params.runId ?? "")) as Run;
  return json(200, await viewOf(env, ctx, run));
};

const workApi =
  (op: string): Handler =>
  async (_req, env, ctx) =>
    json(200, await workFor(env, ctx, ctx.params.runId ?? "").api(metaOf(ctx), op, { ...Object.fromEntries(ctx.url.searchParams), ...ctx.params }, null));

/** GET /api/v1/runs/{runId}/messages */
export const listRunMessages: Handler = async (_req, env, ctx) => {
  const res = (await workFor(env, ctx, ctx.params.runId ?? "").api(
    metaOf(ctx),
    "listRunMessages",
    { ...Object.fromEntries(ctx.url.searchParams), runId: ctx.params.runId ?? "" },
    null,
  )) as { messages: RunMessage[] };
  const role = ctx.url.searchParams.get("role");
  return json(200, { messages: role ? res.messages.filter((m) => m.role === role) : res.messages });
};

/** POST /api/v1/runs/{runId}/messages */
export const postRunMessage: Handler = async (req, env, ctx) => {
  const b = await body<{ message?: string; content?: string; attachments?: string[] }>(req);
  const message = b.message ?? b.content ?? "";
  if (!message.trim()) throw invalid("message is required");
  const rec = await workFor(env, ctx, ctx.params.runId ?? "").runsMessage(metaOf(ctx), ctx.params.runId ?? "", {
    message,
    actor: actorOf(ctx),
    ...(b.attachments ? { attachments: b.attachments } : {}),
  });
  return json(202, rec);
};

/** POST /api/v1/runs/{runId}/followups */
export const followUpRun: Handler = postRunMessage;

/** POST /api/v1/runs/{runId}/cancel */
export const cancelRun: Handler = async (req, env, ctx) => {
  const b = await body<{ cascade?: boolean }>(req);
  const res = (await workFor(env, ctx, ctx.params.runId ?? "").runsCancel(metaOf(ctx), ctx.params.runId ?? "", b.cascade !== false)) as {
    cancelled: string[];
    run: Run;
  };
  return json(200, { cancelled: res.cancelled, run: runView(res.run, { publicUrl: publicUrl(env) }) });
};

/** POST /api/v1/runs/{runId}/interrupt */
export const interruptRun: Handler = async (_req, env, ctx) => {
  await workFor(env, ctx, ctx.params.runId ?? "").runsInterrupt(metaOf(ctx), ctx.params.runId ?? "");
  return json(202, { interrupted: true });
};

/** GET /api/v1/runs/{runId}/events: SSE of run messages, woken by the WorkDO socket. */
export const streamRunEvents: Handler = async (req, env, ctx) => {
  const runId = ctx.params.runId ?? "";
  const stub = workFor(env, ctx, runId);
  await stub.getRun(runId); // 404 before the stream starts
  return sseResponse({
    lastEventId: req.headers.get("last-event-id") ?? ctx.url.searchParams.get("after") ?? "0",
    backlog: async (cursor) => {
      const res = (await stub.api(metaOf(ctx), "listRunMessages", { runId, after: cursor, limit: "200" }, null)) as { messages: RunMessage[] };
      const events = res.messages.map((m) => ({ id: String(m.seq), event: "message", data: m }));
      return { events, cursor: events.at(-1)?.id ?? cursor };
    },
    subscribe: () => subscribe(stub, `/subscribe/run?run=${encodeURIComponent(runId)}`),
    signal: req.signal,
  });
};

/** GET /api/v1/runs/{runId}/transcript (R2 object of the transcript upload): not in this wave. */
export const getRunTranscript: Handler = notImplementedHandler;
export const listRunArtifacts: Handler = notImplementedHandler;
export const getRunArtifact: Handler = notImplementedHandler;
export const listRunAttachments: Handler = notImplementedHandler;
export const createRunAttachment: Handler = notImplementedHandler;
export const getRunAttachment: Handler = notImplementedHandler;

/** GET /api/v1/runs/{runId}/inputs */
export const listRunInputs: Handler = workApi("listRunInputs");
/** GET /api/v1/runs/{runId}/timeline */
export const getRunTimeline: Handler = workApi("getRunTimeline");
/** GET /api/v1/runs/{runId}/diagnostics */
export const getRunDiagnostics: Handler = workApi("getRunDiagnostics");
/** GET /api/v1/runs/{runId}/sessions */
export const listRunSessions: Handler = workApi("listRunSessions");
/** GET /api/v1/runs/{runId}/conversation */
export const getRunConversation: Handler = async (_req, env, ctx) => {
  const res = (await workFor(env, ctx, ctx.params.runId ?? "").api(metaOf(ctx), "listRunMessages", { runId: ctx.params.runId ?? "" }, null)) as {
    messages: RunMessage[];
  };
  return json(200, { run_id: ctx.params.runId, items: res.messages });
};

export { OfError };
