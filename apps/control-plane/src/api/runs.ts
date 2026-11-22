// REST handlers: runs. Owner: WS5. The Worker authenticates; the WorkDO of
// the run (by its partition token) owns it; lists come from IndexDO.
import {
  builtinMCPProvider,
  defaultModel,
  doName,
  invalid,
  newRoutedId,
  newToken,
  notFound,
  newId,
  OfError,
  partitionOf,
  sourceKey,
  validateMetadata,
  type SourceRef,
} from "@openfactory/core";
import type { MachineSpec as CoreMachineSpec, MCPServer } from "@openfactory/core";
import type { CallMeta, CoreEvent } from "../contracts/do-messages";
import type { Env } from "../contracts/env";
import { registerCronHook, registerServerRoute } from "../builder";
import { runView } from "../engine/view";
import { callerPrincipal } from "../engine/principal";
import {
  isTerminal,
  waitsForInput,
  type AgentConfig,
  type Principal,
  type Run,
  type RunMessage,
  type RunSession,
} from "../engine/types";
import { notImplementedHandler, type Handler, type RouteContext } from "../http/handler";
import { handleRunMcp, RUN_MCP_PREFIX } from "../orchestration/index";
import { handleRunnerApi, RUNNER_PREFIX } from "../runner-api/index";
import { sseResponse, subscribe } from "../stream/index";
import { checkAttachments } from "./attachments";
import { dispatchAutomation, platformIntakeDeps } from "../do/ingress/intake";
import { applyOverrides } from "../scoring/overrides";
import { mergeMessages, readCompactedMessages } from "../compaction/compact";
import type { Overrides } from "../scoring/types";
import type { ManualRunDispatcher } from "./schedules";

/**
 * The dispatcher of POST /automations/{id}/run (setManualRunDispatcher in
 * ./schedules). wiring.ts registers it: a static import of ./schedules here
 * closes a module cycle through the TeamDO modules at load time.
 */
export const manualRunDispatcher: ManualRunDispatcher = async (env, input) => {
  const out = await dispatchAutomation(
    platformIntakeDeps(env, input.teamId),
    input.factoryId,
    input.automation,
    input.event as unknown as CoreEvent,
  );
  if (!out.runId) throw new OfError("internal_error", "the automation started no run");
  const run = (await env.WORK.getByName(doName.work(input.teamId, partitionOf(out.runId))).getRun(out.runId)) as Run;
  return runView(run, { publicUrl: (env.OF_PUBLIC_URL ?? "").replace(/\/+$/, "") });
};

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
  const sessions = (await workFor(env, ctx, run.id).api(metaOf(ctx), "listRunSessions", { runId: run.id }, null)) as {
    sessions: RunSession[];
  };
  return runView(run, { publicUrl: publicUrl(env), latest: sessions.sessions.at(-1) });
}

/** Factory and agent from TeamDO (configSnapshot: { factory: {id, team_id}, agents: AgentConfig[] }). */
interface Snapshot {
  factory?: { id: string };
  agents?: AgentConfig[];
  runtime?: { machines?: Record<string, CoreMachineSpec> | null };
}

interface Resolved {
  factoryId: string;
  agent: AgentConfig;
  machines?: Record<string, CoreMachineSpec> | null;
}

async function resolveAgent(env: Env, ctx: RouteContext, factoryRef: string, agentRef: string): Promise<Resolved> {
  const snap = (await env.TEAM.getByName(doName.team(teamOf(ctx))).configSnapshot(factoryRef)) as Snapshot | null;
  if (!snap?.factory) throw notFound(`factory ${factoryRef}`);
  const agent = (snap.agents ?? []).find((a) => a.id === agentRef || a.name === agentRef);
  if (!agent) throw invalid(`agent ${JSON.stringify(agentRef)} does not exist in factory ${factoryRef}`);
  return { factoryId: snap.factory.id, agent, machines: snap.runtime?.machines ?? null };
}

/** An agent by its stable ID across the team's factories (agent_identity_uid without factory_id). */
async function resolveAgentById(env: Env, ctx: RouteContext, agentId: string): Promise<Resolved> {
  const team = env.TEAM.getByName(doName.team(teamOf(ctx)));
  const list = (await team.api(metaOf(ctx), "listFactories", { "?limit": "500" }, null)) as {
    items?: { id: string }[];
    $error?: { detail: string };
  };
  if (list.$error) throw new OfError("internal_error", list.$error.detail);
  for (const f of list.items ?? []) {
    const snap = (await team.configSnapshot(f.id)) as Snapshot | null;
    const agent = (snap?.agents ?? []).find((a) => a.id === agentId);
    if (snap?.factory && agent) return { factoryId: snap.factory.id, agent, machines: snap.runtime?.machines ?? null };
  }
  throw notFound(`agent ${agentId}`);
}

function actorOf(ctx: RouteContext): string {
  return ctx.principal?.userId || ctx.principal?.id || "api";
}

// ---------- handlers ----------

/** RunConfig of POST /runs (internal/api/runs.go): optional overrides for the agent of the run. */
interface RunConfigBody {
  harness?: {
    type?: string;
    model?: string;
    auth?: { source?: string; secretName?: string };
    [k: string]: unknown;
  } | null;
  model_id?: string;
  environment_id?: string;
  machine?: string | CoreMachineSpec | null;
  worker_host?: string;
  mcp_servers?: Record<string, MCPServer> | null;
  secrets?: string[] | null;
  idle_timeout_minutes?: number | null;
}

interface CreateRunBody {
  prompt?: string;
  title?: string;
  agent_identity_uid?: string;
  factory_id?: string;
  agent_name?: string;
  config?: RunConfigBody | null;
  metadata?: Record<string, string>;
}

/** DispatchRequest (internal/api/factories.go). */
interface DispatchBody extends CreateRunBody {
  agent?: string;
  ticket_ref?: string;
  ticket_url?: string;
  source?: SourceRef | null;
  /** core.AgentOverrides of the dispatched agent (ApplyOverrides in prepareDispatch). */
  overrides?: Overrides | null;
}

/**
 * withRequestSource (internal/api/runfilter.go): the triggered_by of a run
 * that an API caller starts. The Factory MCP dispatched with "mcp"
 * (internal/mcpserver); the CLI sends a User-Agent of openfactory/...; a
 * signed-in person without an API key is the dashboard.
 */
export function requestSource(req: Request, ctx: RouteContext): string {
  const p = ctx.principal;
  if (p?.kind === "factory_mcp") return "mcp";
  if ((req.headers.get("user-agent") ?? "").startsWith("openfactory/")) return "cli";
  if (p?.kind === "user" && !p.keyId && p.userId) return "dashboard";
  return "api";
}

const TICKET_REF_RE = /^([a-z][a-z0-9_-]*):(\S+)$/;

/** The source of a dispatch (handleDispatch): body source, ticket_ref and ticket_url; provider api by default. */
export function dispatchSource(b: DispatchBody, title: string): SourceRef {
  let src: SourceRef = { provider: "api", kind: "task" };
  if (b.source) {
    src = { ...b.source };
    if (!src.provider) src.provider = "api";
  }
  const ref = (b.ticket_ref ?? "").trim();
  if (b.ticket_ref) {
    const m = TICKET_REF_RE.exec(ref);
    if (!m) throw invalid("ticket_ref must have the form <source>:<id>, e.g. linear:PAY-123");
    src.provider = m[1]!;
    src.external_id = m[2]!;
    if (!src.kind || src.kind === "task") src.kind = "issue";
  }
  if (b.ticket_url) src.url = b.ticket_url;
  if (!src.title) src.title = title;
  return src;
}

const SECRET_NAME_RE = /^[A-Za-z_][A-Za-z0-9_]{0,127}$/;

/** normalizeHarness (internal/api/runs.go). */
function normalizeHarness(h: NonNullable<RunConfigBody["harness"]>, workerHost: string): void {
  const t = String(h.type ?? "").toLowerCase();
  if (t === "" || t === "claude" || t === "claude-code") h.type = "claude-code";
  else if (t === "codex") h.type = "codex";
  else throw invalid("harness.type must be one of claude, claude-code, codex");
  if (!h.model) h.model = defaultModel(h.type);
  const a = h.auth;
  if (a) {
    if (a.source === "managedSecret") {
      if (!a.secretName) throw invalid("harness.auth.secretName is required with source managedSecret");
    } else if (a.source === "workerEnvironment") {
      if (a.secretName) throw invalid("harness.auth.secretName is not allowed with source workerEnvironment");
      if (workerHost === "" || workerHost === "server")
        throw invalid("harness.auth.source workerEnvironment requires a self-hosted worker_host");
    } else throw invalid("harness.auth.source must be managedSecret or workerEnvironment");
  }
}

/** ValidateMCPServers (internal/api/runs.go): each server selects exactly one transport. */
export function validateMcpServers(m: Record<string, MCPServer> | null | undefined): void {
  for (const [name, srv] of Object.entries(m ?? {})) {
    if (name.trim() === "") throw invalid("mcp_servers keys must be non-empty");
    const s = srv as MCPServer & {
      managedId?: string;
      command?: string;
      url?: string;
      args?: unknown[];
      env?: object;
      headers?: object;
      allowWrite?: boolean;
    };
    const n = [!!s.managedId, !!s.command, !!s.url].filter(Boolean).length;
    if (n !== 1) throw invalid(`mcp_servers.${name} must set exactly one of managedId, command or url`);
    if (s.url) {
      let ok: boolean;
      try {
        const u = new URL(s.url);
        ok = (u.protocol === "http:" || u.protocol === "https:") && u.host !== "";
      } catch {
        ok = false;
      }
      if (!ok) throw invalid(`mcp_servers.${name}.url must be an absolute http(s) URL`);
      if ((s.args?.length ?? 0) > 0 || Object.keys(s.env ?? {}).length > 0)
        throw invalid(`mcp_servers.${name}: args and env apply only to command servers`);
    }
    if (s.command && Object.keys(s.headers ?? {}).length > 0)
      throw invalid(`mcp_servers.${name}: headers apply only to url servers`);
    if (s.managedId?.startsWith("builtin/")) {
      if (!builtinMCPProvider(s.managedId).known)
        throw invalid(`mcp_servers.${name}: unknown built-in MCP server ${JSON.stringify(s.managedId)}`);
    } else if (s.allowWrite) throw invalid(`mcp_servers.${name}: allowWrite is only valid with a built-in managedId`);
  }
}

/** The sparse overrides of a RunConfig (CreateRun in internal/api/runs.go). */
function runConfigOverrides(cfgIn: RunConfigBody | null | undefined): Overrides | null {
  if (!cfgIn) return null;
  let workerHost = (cfgIn.worker_host ?? "").trim();
  if (workerHost === "managed" || workerHost === "default") workerHost = "";
  let harness: RunConfigBody["harness"] = null;
  if (cfgIn.harness) {
    harness = { ...cfgIn.harness };
    if (!harness.model) harness.model = cfgIn.model_id ?? "";
    normalizeHarness(harness, workerHost);
  }
  validateMcpServers(cfgIn.mcp_servers);
  for (const sn of cfgIn.secrets ?? [])
    if (!SECRET_NAME_RE.test(sn)) throw invalid(`invalid secret name ${JSON.stringify(sn)}`);
  const t = cfgIn.idle_timeout_minutes;
  if (t !== undefined && t !== null && (t < 0 || t > 60))
    throw invalid("idle_timeout_minutes must be between 0 and 60");
  let machine = "";
  if (cfgIn.machine !== undefined && cfgIn.machine !== null) {
    if (typeof cfgIn.machine === "string") machine = cfgIn.machine;
    else if (typeof cfgIn.machine === "object") machine = cfgIn.machine.name ?? "";
    else throw invalid("config.machine must be a machine name or a machine object");
  }
  const ov: Overrides = {};
  if (harness) ov.harness = harness as Overrides["harness"];
  else if (cfgIn.model_id) ov.model = cfgIn.model_id;
  if (cfgIn.secrets?.length) ov.secrets = cfgIn.secrets;
  if (cfgIn.mcp_servers && Object.keys(cfgIn.mcp_servers).length) ov.mcp_servers = cfgIn.mcp_servers;
  if (t !== undefined && t !== null) ov.idle_timeout_minutes = t;
  if (cfgIn.environment_id) ov.environment_id = cfgIn.environment_id;
  if (workerHost) ov.worker_host = workerHost;
  if (machine) ov.machine = machine;
  return ov;
}

/** CreateRun (internal/api/runs.go): config.environment_id must name an environment of the team. */
async function checkEnvironment(env: Env, ctx: RouteContext, id: string): Promise<void> {
  const out = (await env.TEAM.getByName(doName.team(teamOf(ctx))).api(metaOf(ctx), "getEnvironment", { id }, null)) as {
    $error?: { code: string; detail: string };
  };
  if (!out?.$error) return;
  if (out.$error.code === "resource_not_found") throw invalid(`environment ${id} does not exist`);
  throw new OfError(out.$error.code as never, out.$error.detail);
}

/** The creator and run_as of a run that an API caller starts: the caller for both (engine/principal.go). */
function callerPrincipals(ctx: RouteContext): { creator?: Principal; runAs?: Principal } {
  const p = callerPrincipal(ctx.principal);
  return p ? { creator: p, runAs: p } : {};
}

/** POST /api/v1/runs: a run of an agent: a new work item with the run as its root. */
export const createRun: Handler = async (req, env, ctx) => {
  const b = await body<CreateRunBody>(req);
  if (!b.prompt?.trim()) throw invalid("prompt is required");
  validateMetadata(b.metadata);
  const factoryRef = b.factory_id ?? "";
  const agentRef = b.agent_identity_uid || b.agent_name || "";
  if (!agentRef || (!factoryRef && !b.agent_identity_uid))
    throw invalid("set agent_identity_uid, or factory_id and agent_name");
  const ov = runConfigOverrides(b.config);
  if (ov?.environment_id) await checkEnvironment(env, ctx, ov.environment_id);
  const resolved = factoryRef
    ? await resolveAgent(env, ctx, factoryRef, agentRef)
    : await resolveAgentById(env, ctx, b.agent_identity_uid!);
  const factoryId = resolved.factoryId;
  const agent = applyOverrides(resolved.agent as never, ov, resolved.machines as never) as unknown as AgentConfig;
  const token = newToken("w");
  const out = (await env.WORK.getByName(doName.work(teamOf(ctx), token)).runsCreate(metaOf(ctx), {
    dispatch: {
      factoryId,
      config: agent,
      prompt: b.prompt,
      actor: actorOf(ctx),
      title: b.title || b.prompt.split("\n")[0]!.slice(0, 120),
      workItemId: newRoutedId("wi", token),
      triggeredBy: requestSource(req, ctx),
      ...(b.metadata ? { metadata: b.metadata } : {}),
      ...(ctx.principal?.userId ? { creatorId: ctx.principal.userId } : {}),
      ...callerPrincipals(ctx),
    },
  })) as { run: Run };
  return json(201, runView(out.run, { publicUrl: publicUrl(env) }));
};

/**
 * POST /api/v1/factories/{uid}/runs: dispatch to the factory's foreman
 * (handleDispatch). A source with a key routes to the active work item of
 * that key: its root run gets the prompt as a follow-up (Engine.Dispatch).
 */
export const dispatchRun: Handler = async (req, env, ctx) => {
  const b = await body<DispatchBody>(req);
  if (!b.prompt?.trim()) throw invalid("prompt is required");
  validateMetadata(b.metadata);
  const title = b.title?.trim() || b.prompt.split("\n")[0]!.slice(0, 120);
  const source = dispatchSource(b, title);
  const resolved = await resolveAgent(env, ctx, ctx.params.uid ?? "", b.agent || b.agent_name || "foreman");
  const { factoryId } = resolved;
  if (b.overrides?.environment_id) await checkEnvironment(env, ctx, b.overrides.environment_id);
  const agent = b.overrides
    ? (applyOverrides(resolved.agent as never, b.overrides, resolved.machines as never) as unknown as AgentConfig)
    : resolved.agent;
  const key = sourceKey(source);
  let token = newToken("w");
  let workItemId = newRoutedId("wi", token);
  if (key) {
    const r = await env.ROUTER.getByName(doName.router(teamOf(ctx))).resolveOrReserve({
      factoryId,
      sourceKeys: [key],
      actionKey: newId("act"),
      target: `api:${factoryId}`,
    });
    token = r.token;
    workItemId = r.wiId;
  }
  const out = (await env.WORK.getByName(doName.work(teamOf(ctx), token)).runsCreate(metaOf(ctx), {
    dispatch: {
      factoryId,
      config: agent,
      prompt: b.prompt,
      actor: actorOf(ctx),
      title,
      source: source as never,
      ...(key ? { sourceKey: key } : {}),
      workItemId,
      triggeredBy: requestSource(req, ctx),
      ...(b.metadata ? { metadata: b.metadata } : {}),
      ...(ctx.principal?.userId ? { creatorId: ctx.principal.userId } : {}),
      ...callerPrincipals(ctx),
    },
  })) as { run: Run; workItemId?: string };
  return json(201, { ...runView(out.run, { publicUrl: publicUrl(env) }), work_item_id: out.workItemId });
};

/** GET /api/v1/runs: IndexDO read model; a parent_run_id filter reads the run tree's WorkDO. */
export const listRuns: Handler = async (_req, env, ctx) => {
  const q = Object.fromEntries(ctx.url.searchParams);
  if (q.parent_run_id) {
    const res = (await workFor(env, ctx, q.parent_run_id).api(
      metaOf(ctx),
      "listRunChildren",
      { runId: q.parent_run_id },
      null,
    )) as { runs: Run[] };
    return json(200, {
      items: res.runs.map((r) => runView(r, { publicUrl: publicUrl(env) })),
      page_info: { has_next_page: false },
    });
  }
  const res = await env.INDEX.getByName(doName.index(teamOf(ctx))).list(metaOf(ctx), "runs", q);
  return json(200, res);
};

/** The run of the route, read through WorkDO.api so that WorkDO checks the caller's role for its factory. */
async function authorizedRun(env: Env, ctx: RouteContext, runId: string): Promise<Run> {
  return (await workFor(env, ctx, runId).api(metaOf(ctx), "getRun", { runId }, null)) as Run;
}

/** GET /api/v1/runs/{runId} */
export const getRun: Handler = async (_req, env, ctx) => {
  const run = await authorizedRun(env, ctx, ctx.params.runId ?? "");
  return json(200, await viewOf(env, ctx, run));
};

/** A WorkDO list read as the openapi list shape {items, page_info} (one page: a run's lists are bounded). */
const workApi =
  (op: string, field: string): Handler =>
  async (_req, env, ctx) => {
    const out = (await workFor(env, ctx, ctx.params.runId ?? "").api(
      metaOf(ctx),
      op,
      { ...Object.fromEntries(ctx.url.searchParams), ...ctx.params },
      null,
    )) as Record<string, unknown[]>;
    return json(200, { items: out[field] ?? [], page_info: { has_next_page: false } });
  };

/**
 * The messages of a run after `after`: compacted ones from R2 (when the run
 * was compacted and `after` is below the compacted prefix), then the rows
 * that WorkDO still holds.
 */
export async function readRunMessages(
  env: Env,
  stub: { api(meta: CallMeta, op: string, params: Record<string, string>, body: unknown): Promise<unknown> },
  meta: CallMeta,
  runId: string,
  after: number,
): Promise<RunMessage[]> {
  const res = (await stub.api(meta, "listRunMessages", { runId, after: String(after), limit: "0" }, null)) as {
    messages: RunMessage[];
    compacted_upto?: number;
  };
  if (!res.compacted_upto || after >= res.compacted_upto) return res.messages;
  const compacted = (await readCompactedMessages(env.DATA, runId)).filter((m) => m.seq > after) as RunMessage[];
  return mergeMessages(compacted, res.messages);
}

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
  const run = await authorizedRun(env, ctx, runId);
  const all = await readRunMessages(env, stub, metaOf(ctx), runId, after);
  const roles = q
    .getAll("role")
    .flatMap((r) => r.split(","))
    .map((r) => r.trim())
    .filter(Boolean);
  let msgs = roles.length ? all.filter((m) => roles.includes(m.role)) : all;
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
  return json(202, { run_id: ctx.params.runId ?? "", ...rec });
};

/** POST /api/v1/runs/{runId}/followups */
export const followUpRun: Handler = postRunMessage;

/** POST /api/v1/runs/{runId}/cancel */
export const cancelRun: Handler = async (req, env, ctx) => {
  const b = await body<{ cascade?: boolean }>(req);
  const res = (await workFor(env, ctx, ctx.params.runId ?? "").runsCancel(
    metaOf(ctx),
    ctx.params.runId ?? "",
    b.cascade !== false,
  )) as {
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
  await authorizedRun(env, ctx, runId); // 403 or 404 before the stream starts
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
      const msgs = await readRunMessages(env, stub, metaOf(ctx), runId, Number(cursor) || 0);
      for (const m of msgs) events.push({ id: String(m.seq), event: "message", data: m });
      const next = msgs.at(-1) ? String(msgs.at(-1)!.seq) : cursor;
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
  const runId = ctx.params.runId ?? "";
  const items = await readRunMessages(env, workFor(env, ctx, runId), metaOf(ctx), runId, 0);
  return json(200, { run_id: runId, items });
};

export { OfError };
