// REST handlers: runs. Owner: WS5. The Worker authenticates; the WorkDO of
// the run (by its partition token) owns it; lists come from IndexDO.
import { doName, invalid, newRoutedId, newToken, notFound, OfError, partitionOf } from "@openfactory/core";
import type { CallMeta, CoreEvent } from "../contracts/do-messages";
import type { Env } from "../contracts/env";
import { registerCronHook, registerServerRoute } from "../builder";
import { runView } from "../engine/view";
import { isTerminal, waitsForInput, type AgentConfig, type Run, type RunMessage, type RunSession } from "../engine/types";
import { notImplementedHandler, type Handler, type RouteContext } from "../http/handler";
import { handleRunMcp, RUN_MCP_PREFIX } from "../orchestration/index";
import { handleRunnerApi, RUNNER_PREFIX } from "../runner-api/index";
import { sseResponse, subscribe } from "../stream/index";
import { checkAttachments } from "./attachments";
import { dispatchAutomation, platformIntakeDeps } from "../do/ingress/intake";

// Registered lazily: a static import of ./schedules closes a module cycle
// through the TeamDO modules at load time.
void import("./schedules").then(({ setManualRunDispatcher }) => setManualRunDispatcher(async (env, input) => {
  const out = await dispatchAutomation(platformIntakeDeps(env, input.teamId), input.factoryId, input.automation, input.event as unknown as CoreEvent);
  if (!out.runId) throw new OfError("internal_error", "the automation started no run");
  const run = (await env.WORK.getByName(doName.work(input.teamId, partitionOf(out.runId))).getRun(out.runId)) as Run;
  return runView(run, { publicUrl: (env.OF_PUBLIC_URL ?? "").replace(/\/+$/, "") });
}));

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

/** An agent by its stable ID across the team's factories (agent_identity_uid without factory_id). */
async function resolveAgentById(env: Env, ctx: RouteContext, agentId: string): Promise<{ factoryId: string; agent: AgentConfig }> {
  const team = env.TEAM.getByName(doName.team(teamOf(ctx)));
  const list = (await team.api(metaOf(ctx), "listFactories", { "?limit": "500" }, null)) as { items?: { id: string }[]; $error?: { detail: string } };
  if (list.$error) throw new OfError("internal_error", list.$error.detail);
  for (const f of list.items ?? []) {
    const snap = (await team.configSnapshot(f.id)) as { factory?: { id: string }; agents?: AgentConfig[] } | null;
    const agent = (snap?.agents ?? []).find((a) => a.id === agentId);
    if (snap?.factory && agent) return { factoryId: snap.factory.id, agent };
  }
  throw notFound(`agent ${agentId}`);
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
  const { factoryId, agent } = factoryRef
    ? await resolveAgent(env, ctx, factoryRef, agentRef)
    : await resolveAgentById(env, ctx, b.agent_identity_uid!);
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

/** A WorkDO list read as the openapi list shape {items, page_info} (one page: a run's lists are bounded). */
const workApi =
  (op: string, field: string): Handler =>
  async (_req, env, ctx) => {
    const out = (await workFor(env, ctx, ctx.params.runId ?? "").api(metaOf(ctx), op, { ...Object.fromEntries(ctx.url.searchParams), ...ctx.params }, null)) as Record<string, unknown[]>;
    return json(200, { items: out[field] ?? [], page_info: { has_next_page: false } });
  };

/** GET /api/v1/runs/{runId}/messages */
export const listRunMessages: Handler = async (_req, env, ctx) => {
  const runId = ctx.params.runId ?? "";
  const q = ctx.url.searchParams;
  const after = Number(q.get("after") ?? "0");
  if (!Number.isInteger(after) || after < 0) throw invalid(`invalid after ${JSON.stringify(q.get("after"))}`);
  let limit = Number(q.get("limit") ?? "200");
  if (!Number.isInteger(limit)) throw invalid(`invalid limit ${JSON.stringify(q.get("limit"))}`);
  if (limit <= 0 || limit > 1000) limit = 1000;
  const stub = workFor(env, ctx, runId);
  const run = (await stub.getRun(runId)) as Run;
  const res = (await stub.api(metaOf(ctx), "listRunMessages", { runId, after: String(after), limit: "0" }, null)) as { messages: RunMessage[] };
  const roles = q.getAll("role").flatMap((r) => r.split(",")).map((r) => r.trim()).filter(Boolean);
  let msgs = roles.length ? res.messages.filter((m) => roles.includes(m.role)) : res.messages;
  const out: Record<string, unknown> = { run_id: run.id, state: String(runView(run).state) };
  if (msgs.length > limit) {
    msgs = msgs.slice(0, limit);
    out.next_after = msgs.at(-1)!.seq;
    out.has_more = true;
  } else out.has_more = false;
  out.messages = msgs;
  return json(200, out);
};

/** POST /api/v1/runs/{runId}/messages */
export const postRunMessage: Handler = async (req, env, ctx) => {
  const b = await body<{ message?: string; content?: string; attachments?: string[] }>(req);
  const message = b.message ?? b.content ?? "";
  if (!message.trim()) throw invalid("message is required");
  if (b.attachments?.length) await checkAttachments(env, teamOf(ctx), b.attachments);
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
  void res;
  return json(200, { run_id: ctx.params.runId ?? "" });
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
  const stopOnIdle = ["1", "true"].includes((ctx.url.searchParams.get("stop_on_idle") ?? "").toLowerCase());
  const pub = publicUrl(env);
  let lastState = "";
  return sseResponse({
    lastEventId: req.headers.get("last-event-id") ?? ctx.url.searchParams.get("after") ?? "0",
    backlog: async (cursor) => {
      // Go handleRunEvents: "run" on each state change, the messages, then "done" at a finished state.
      const run = (await stub.getRun(runId)) as Run;
      const events: { id?: string; event: string; data: unknown }[] = [];
      const view = runView(run, pub ? { publicUrl: pub } : {});
      if (run.state !== lastState) {
        lastState = run.state;
        events.push({ event: "run", data: view });
      }
      const res = (await stub.api(metaOf(ctx), "listRunMessages", { runId, after: cursor, limit: "0" }, null)) as { messages: RunMessage[] };
      for (const m of res.messages) events.push({ id: String(m.seq), event: "message", data: m });
      const next = res.messages.at(-1) ? String(res.messages.at(-1)!.seq) : cursor;
      const finished = isTerminal(run.state) || (stopOnIdle && waitsForInput(run.state));
      if (finished) events.push({ event: "done", data: view });
      return { events, cursor: next, done: finished };
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
export const listRunInputs: Handler = workApi("listRunInputs", "inputs");
/** GET /api/v1/runs/{runId}/timeline */
export const getRunTimeline: Handler = workApi("getRunTimeline", "events");
/** GET /api/v1/runs/{runId}/diagnostics */
export const getRunDiagnostics: Handler = workApi("getRunDiagnostics", "diagnostics");
/** GET /api/v1/runs/{runId}/sessions */
export const listRunSessions: Handler = workApi("listRunSessions", "sessions");
/** GET /api/v1/runs/{runId}/conversation */
export const getRunConversation: Handler = async (_req, env, ctx) => {
  const res = (await workFor(env, ctx, ctx.params.runId ?? "").api(metaOf(ctx), "listRunMessages", { runId: ctx.params.runId ?? "" }, null)) as {
    messages: RunMessage[];
  };
  return json(200, { run_id: ctx.params.runId, items: res.messages });
};

export { OfError };
