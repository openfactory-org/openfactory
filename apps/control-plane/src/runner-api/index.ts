// /runner/v1 in the Worker (internal/runnerapi/service.go). The Worker checks
// the runner version header, verifies tokens and holds the commands long-poll;
// each route calls one WorkDO RPC that runs one transaction.
import { OfError, parseBootstrapToken, partitionOf, doName, toOfError } from "@openfactory/core";
import type { StartRequest } from "@openfactory/runner-proto";
import type { Env } from "../contracts/env";
import type { Run, RunSession } from "../engine/types";
import { longPoll, MAX_WAIT_S, subscribe } from "../stream/index";
import { buildSecrets, buildStartConfig } from "./startconfig";
import { mintSessionToken, runnerKeys, sha256Hex, verifySessionToken, type SessionClaims } from "./token";

export const RUNNER_PREFIX = "/runner/v1";
export const HEADER_RUNNER_VERSION = "OF-Runner-Version";
const MAX_BODY = 8 << 20;
const MAJOR = "1";

type RunnerErr = { error: { status: number; code: string; message: string; ack?: number } };

function json(status: number, body: unknown, headers: Record<string, string> = {}): Response {
  return new Response(JSON.stringify(body) + "\n", { status, headers: { "content-type": "application/json", ...headers } });
}

function errorBody(status: number, code: string, message: string, ack?: number): Response {
  return json(status, { code, ...(message ? { message } : {}), ...(ack !== undefined ? { ack } : {}) });
}

function fromErr(e: RunnerErr): Response {
  return errorBody(e.error.status, e.error.code, e.error.message, e.error.ack);
}

const isErr = (v: unknown): v is RunnerErr => typeof v === "object" && v !== null && "error" in v;

/** checkVersion: a different numeric major version is refused with 426. */
export function checkVersion(v: string | null): Response | undefined {
  if (!v) return undefined;
  const major = v.replace(/^v/, "").split(".")[0] ?? "";
  if (major === MAJOR || !/^\d+$/.test(major)) return undefined;
  return errorBody(426, "unsupported_runner_version", `runner version ${JSON.stringify(v)} is not supported, need major version ${MAJOR}`);
}

function bearer(req: Request): string | undefined {
  const h = req.headers.get("authorization") ?? "";
  if (!h.startsWith("Bearer ")) return undefined;
  const t = h.slice(7).trim();
  return t || undefined;
}

async function readJson<T>(req: Request): Promise<T | Response> {
  const len = Number(req.headers.get("content-length") ?? "0");
  if (len > MAX_BODY) return errorBody(413, "too_large", "request body is too large");
  const text = await req.text();
  if (text.length > MAX_BODY) return errorBody(413, "too_large", "request body is too large");
  try {
    return JSON.parse(text) as T;
  } catch (e) {
    return errorBody(400, "bad_request", `invalid JSON body: ${(e as Error).message}`);
  }
}

function workStub(env: Env, teamId: string, runId: string) {
  return env.WORK.getByName(doName.work(teamId, partitionOf(runId)));
}

/** The principal string of a run (run_as), the token subject. */
function subjectOf(run: Run): string {
  const p = run.run_as;
  if (!p) return "runner";
  return p.kind && (p.id || p.name || p.email) ? `${p.kind}:${p.id ?? p.name ?? p.email}` : "runner";
}

async function issue(env: Env, run: Run, session: number) {
  return mintSessionToken(await runnerKeys(env), { team: run.team_id, run: run.id, ses: session, sub: subjectOf(run) });
}

/** Handles every path under /runner/v1. */
export async function handleRunnerApi(request: Request, env: Env, ctx?: ExecutionContext): Promise<Response> {
  const url = new URL(request.url);
  const path = url.pathname;
  if (request.method === "GET" && path.startsWith(RUNNER_PREFIX + "/bin/")) return serveBinary(env, path.slice(RUNNER_PREFIX.length + 5));
  const bad = checkVersion(request.headers.get(HEADER_RUNNER_VERSION));
  if (bad) return bad;
  try {
    const route = `${request.method} ${path}`;
    switch (route) {
      case "POST /runner/v1/session/start":
        return await start(request, env);
      case "GET /runner/v1/session/secrets":
      case "POST /runner/v1/session/token":
      case "POST /runner/v1/session/events":
      case "GET /runner/v1/session/commands":
      case "POST /runner/v1/session/end":
      case "GET /runner/v1/session/conversation":
        return await sessionRoute(route, request, env, url, ctx);
    }
    if (path.startsWith(RUNNER_PREFIX + "/")) return errorBody(501, "not_implemented", "not implemented");
    return errorBody(404, "not_found", "not found");
  } catch (e) {
    const { error, known } = toOfError(e);
    if (!known) console.error("runner-api: internal error", path, e);
    if (error.code === "resource_not_found") return errorBody(401, "unauthorized", "unknown session");
    return errorBody(500, "internal", "internal error");
  }
}

async function start(request: Request, env: Env): Promise<Response> {
  const tok = bearer(request);
  if (!tok) return errorBody(401, "unauthorized", "missing bootstrap token");
  const body = await readJson<StartRequest>(request);
  if (body instanceof Response) return body;
  if (!body || typeof body.instance_id !== "string" || !body.instance_id || typeof body.runner_version !== "string") {
    return errorBody(400, "bad_request", "instance_id and runner_version are required");
  }
  let parsed: { teamId: string; runId: string };
  try {
    parsed = parseBootstrapToken(tok);
    partitionOf(parsed.runId);
  } catch {
    return errorBody(401, "unauthorized", "invalid bootstrap token");
  }
  const stub = workStub(env, parsed.teamId, parsed.runId);
  const res = (await stub.runnerStart({
    bootstrapHash: await sha256Hex(tok),
    instanceHash: await sha256Hex(body.instance_id),
    req: body,
  })) as { run: Run; session: RunSession } | RunnerErr;
  if (isErr(res)) return fromErr(res);
  let cfg;
  try {
    cfg = await buildStartConfig(env, res.run, res.session);
  } catch (e) {
    const { error } = toOfError(e);
    if (error.code === "invalid_request" || error.code === "resource_not_found") {
      return fromErr((await stub.runnerFailStart(res.run.id, res.session.session, error.detail || error.message)) as RunnerErr);
    }
    throw e;
  }
  const { token, expiresAt } = await issue(env, res.run, res.session.session);
  return json(200, { ...cfg, run_id: res.run.id, session: res.session.session, session_token: token, session_token_expires_at: expiresAt });
}

async function sessionRoute(route: string, request: Request, env: Env, url: URL, ctx?: ExecutionContext): Promise<Response> {
  const tok = bearer(request);
  if (!tok) return errorBody(401, "unauthorized", "missing session token");
  const claims = await verifySessionToken(await runnerKeys(env), tok);
  if (!claims) return errorBody(401, "unauthorized", "invalid session token");
  const stub = workStub(env, claims.team, claims.run);
  switch (route) {
    case "GET /runner/v1/session/secrets": {
      const a = (await stub.runnerAuth(claims.run, claims.ses)) as { run: Run; session: RunSession } | RunnerErr;
      if (isErr(a)) return fromErr(a);
      return json(200, await buildSecrets(env, a.run, a.session));
    }
    case "POST /runner/v1/session/token": {
      const a = (await stub.runnerAuth(claims.run, claims.ses)) as { run: Run; session: RunSession } | RunnerErr;
      if (isErr(a)) return fromErr(a);
      if (a.run.team_id !== claims.team) return errorBody(401, "unauthorized", "unknown run");
      const { token, expiresAt } = await issue(env, a.run, claims.ses);
      return json(200, { session_token: token, session_token_expires_at: expiresAt });
    }
    case "POST /runner/v1/session/events": {
      const body = await readJson<unknown>(request);
      if (body instanceof Response) return body;
      const res = (await stub.runnerEvents(claims.run, claims.ses, body)) as { ack: number } | RunnerErr;
      return isErr(res) ? fromErr(res) : json(200, res);
    }
    case "POST /runner/v1/session/end": {
      const body = await readJson<unknown>(request);
      if (body instanceof Response) return body;
      const res = await stub.runnerEnd(claims.run, claims.ses, body);
      return isErr(res) ? fromErr(res) : json(200, {});
    }
    case "GET /runner/v1/session/conversation": {
      const res = await stub.runnerConversation(claims.run, claims.ses, Number(url.searchParams.get("after") ?? "0") || 0);
      return isErr(res) ? fromErr(res) : json(200, res);
    }
    case "GET /runner/v1/session/commands":
      return commands(env, stub, claims, url, request.signal);
  }
  void ctx;
  return errorBody(404, "not_found", "not found");
}

async function commands(
  _env: Env,
  stub: ReturnType<typeof workStub>,
  c: SessionClaims,
  url: URL,
  signal: AbortSignal,
): Promise<Response> {
  const afterRaw = url.searchParams.get("after") ?? "0";
  const waitRaw = url.searchParams.get("wait") ?? "0";
  const after = Number(afterRaw);
  const wait = Number(waitRaw);
  if (!Number.isInteger(after) || after < 0) return errorBody(400, "bad_request", `invalid after ${JSON.stringify(afterRaw)}`);
  if (!Number.isInteger(wait)) return errorBody(400, "bad_request", `invalid wait ${JSON.stringify(waitRaw)}`);
  let failure: RunnerErr | undefined;
  const result = await longPoll<{ commands: unknown[] }>({
    read: async () => {
      const r = (await stub.runnerCommands(c.run, c.ses, after)) as { commands: unknown[] } | RunnerErr;
      if (isErr(r)) {
        failure = r;
        return { commands: [] };
      }
      return r.commands.length ? r : undefined;
    },
    subscribe: () => subscribe(stub, `/subscribe/commands?run=${encodeURIComponent(c.run)}&session=${c.ses}&after=${after}`),
    empty: { commands: [] },
    waitS: Math.max(0, Math.min(wait, MAX_WAIT_S)),
    signal,
  });
  if (failure) return fromErr(failure);
  return json(200, { commands: result.commands.map((x) => toProtoCommand(x)) });
}

function toProtoCommand(x: unknown): unknown {
  const c = x as { seq: number; type: string; data?: unknown };
  return { seq: c.seq, type: c.type, ...(c.data !== undefined ? { data: c.data } : {}) };
}

/** GET /runner/v1/bin/linux-{arch}[.sha256]: the published runner binary from R2. */
async function serveBinary(env: Env, name: string): Promise<Response> {
  const m = /^linux-(amd64|arm64)(\.sha256)?$/.exec(name);
  const v = env.OF_RUNNER_VERSION;
  if (!m || !v) return errorBody(404, "not_found", "not found");
  const key = `runner/${v}/openfactory-runtime-linux-${m[1]}${m[2] ?? ""}`;
  const obj = await env.DATA.get(key);
  if (!obj) return errorBody(404, "not_found", `runner ${v} for linux-${m[1]} is not published`);
  const headers: Record<string, string> = {
    [HEADER_RUNNER_VERSION]: v,
    "cache-control": "public, max-age=300",
    "content-type": m[2] ? "text/plain" : "application/octet-stream",
  };
  if (!m[2]) {
    const sum = await env.DATA.get(key + ".sha256");
    const text = sum ? (await sum.text()).trim().split(/\s+/)[0] : "";
    if (text) headers["x-checksum-sha256"] = text;
  }
  return new Response(obj.body, { headers });
}

export { OfError };
