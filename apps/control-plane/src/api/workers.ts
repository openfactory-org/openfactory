// REST handlers: workers (WS6). The TeamDO owns the records; after a change
// the handler tells the worker's WorkerHubDO (configure, revoke), so a live
// connection sees drain, resume, revoke and label changes at once.
import { DEFAULT_TEAM, OfError, doName, invalid, problemResponse, toOfError } from "@openfactory/core";
import { WORKER_ID_RE, WORKER_SUBPROTOCOL } from "../../../../packages/sandbox/src/microvm/wire";
import type { Principal, TeamRpc } from "../contracts/do-messages";
import type { Env } from "../contracts/env";
import { bearerToken } from "../auth/apikeys";
import { apiKeyPrincipal, registerBearerAuthenticator } from "../auth/authenticate";
import { isOpError } from "../do/workerhub/op";
import type { WorkerRecord } from "../do/team/workers";
import { WORKER_TOKEN_PREFIX, verifyWorkerToken } from "../do/workerhub/token";
import { callMeta, callTeam, json, readJson, teamApi, teamOf, type Handler, type RouteContext } from "../http/handler";

// Worker access tokens (ofw_) authenticate routes with role "worker".
registerBearerAuthenticator(WORKER_TOKEN_PREFIX, async (env, token): Promise<Principal | null> => {
  const c = await verifyWorkerToken(env, token);
  return c ? { kind: "worker", id: c.workerId, teamId: c.teamId, role: "user", factoryRoles: {} } : null;
});

function team(env: Env, teamId: string): TeamRpc {
  return env.TEAM.getByName(doName.team(teamId)) as unknown as TeamRpc;
}

function hub(env: Env, teamId: string, workerId: string) {
  return env.WORKER_HUB.getByName(doName.workerHub(teamId, workerId));
}

/** The record of a worker for its hub; undefined when it is gone. */
async function recordFor(env: Env, ctx: RouteContext, workerId: string): Promise<WorkerRecord | undefined> {
  const r = await team(env, teamOf(ctx)).api(
    { requestId: ctx.requestId },
    "workers.hubConnect",
    { id: workerId },
    null,
  );
  return isOpError(r) ? undefined : (r as WorkerRecord);
}

/** Runs the TeamDO op, then pushes the new record to the hub (best effort). */
function withHubSync(after: "configure" | "revoke"): Handler {
  return async (request, env, ctx) => {
    const res = await teamApi()(request, env, ctx);
    if (!res.ok) return res;
    const id = ctx.params.id ?? "";
    try {
      const h = hub(env, teamOf(ctx), id);
      if (after === "revoke") await h.revoke();
      else {
        const rec = await recordFor(env, ctx, id);
        if (rec) await h.configure(rec);
      }
    } catch (e) {
      console.warn(`workers: ${after} of hub ${id} failed`, e);
    }
    return res;
  };
}

/** GET /api/v1/workers. */
export const listWorkers: Handler = teamApi();

/** POST /api/v1/workers/enrollments (admin): a single-use enrollment token. */
export const createWorkerEnrollment: Handler = teamApi();

/** POST /api/v1/workers/enroll (public): the host enrolls with the token, its public key and its facts. */
export const enrollWorker: Handler = async (request, env) => {
  const b = await readJson(request, false);
  if (typeof b !== "object" || b === null || Array.isArray(b)) throw invalid("malformed JSON body: expected an object");
  const o = b as Record<string, unknown>;
  const str = (k: string): string => {
    const v = o[k];
    if (v === undefined || v === null) return "";
    if (typeof v !== "string") throw invalid(`field "${k}" must be of type string`);
    return v;
  };
  const facts = o.facts;
  if (typeof facts !== "object" || facts === null || Array.isArray(facts)) throw invalid("facts is required");
  const labels = o.labels ?? {};
  if (
    typeof labels !== "object" ||
    labels === null ||
    Array.isArray(labels) ||
    !Object.values(labels).every((v) => typeof v === "string")
  ) {
    throw invalid('field "labels" must be of type map[string]string');
  }
  const out = await env.TEAM.getByName(doName.team(DEFAULT_TEAM)).workersEnroll({
    enrollmentToken: str("enrollment_token"),
    name: str("name"),
    publicKey: str("public_key"),
    facts: facts as Record<string, unknown>,
    labels: labels as Record<string, string>,
    slots: typeof o.slots === "number" ? o.slots : 0,
    slotSize: o.slot_size,
  });
  return json({ worker_id: out.workerId, control_url: (env.OF_PUBLIC_URL ?? "").replace(/\/+$/, "") }, 201);
};

/** GET /api/v1/workers/{id}. Also catches GET /api/v1/workers/connect until the Worker routes it first. */
export const getWorker: Handler = async (request, env, ctx) => {
  if (ctx.params.id === "connect" && (request.headers.get("upgrade") ?? "").toLowerCase() === "websocket") {
    return connectWorker(request, env, ctx);
  }
  return callTeam(env, ctx, "getWorker");
};

/** PATCH /api/v1/workers/{id}. */
export const updateWorker: Handler = withHubSync("configure");

/** DELETE /api/v1/workers/{id}: revokes the worker and removes its record. */
export const deleteWorker: Handler = async (request, env, ctx) => {
  const id = ctx.params.id ?? "";
  // Close the connection first: after the delete the hub no longer finds the record.
  const rec = await recordFor(env, ctx, id);
  const res = await teamApi()(request, env, ctx);
  if (res.ok && rec) {
    try {
      await hub(env, teamOf(ctx), id).revoke();
    } catch (e) {
      console.warn(`workers: revoke of hub ${id} failed`, e);
    }
  }
  return res;
};

/** POST /api/v1/workers/{id}/drain. */
export const drainWorker: Handler = withHubSync("configure");

/** POST /api/v1/workers/{id}/resume. */
export const resumeWorker: Handler = withHubSync("configure");

/** POST /api/v1/workers/{id}/revoke: closes the socket, refuses connections and git credentials. */
export const revokeWorker: Handler = withHubSync("revoke");

/** GET /api/v1/workers/{id}/git-credential (worker access token): read-only, single repository, audited. */
export const getWorkerGitCredential: Handler = async (_request, env, ctx) => {
  if (ctx.principal?.kind !== "worker") throw new OfError("not_authorized", "this route needs a worker access token");
  return callTeam(env, ctx, "getWorkerGitCredential");
};

/**
 * GET /api/v1/workers/connect: the worker protocol v2 WebSocket. Routed by
 * the OF-Worker-ID header to WorkerHubDO. An enrolled worker sends no
 * Authorization header (the hub checks its Ed25519 key); a development
 * process worker sends a team API key.
 */
export async function connectWorker(request: Request, env: Env, ctx: RouteContext): Promise<Response> {
  const problem = { instance: ctx.url.pathname, requestId: ctx.requestId };
  try {
    if ((request.headers.get("upgrade") ?? "").toLowerCase() !== "websocket") {
      return new Response("expected a WebSocket upgrade\n", { status: 426, headers: { upgrade: "websocket" } });
    }
    const protocols = (request.headers.get("sec-websocket-protocol") ?? "").split(",").map((s) => s.trim());
    if (!protocols.includes(WORKER_SUBPROTOCOL))
      throw invalid("unsupported worker protocol: offer the subprotocol openfactory.worker.v2");
    const workerId = (request.headers.get("of-worker-id") ?? "").trim();
    if (!WORKER_ID_RE.test(workerId)) throw invalid("the OF-Worker-ID header is missing or invalid");
    const token = bearerToken(request);
    let mode: "key" | "apikey" = "key";
    let teamId = DEFAULT_TEAM;
    if (token !== "") {
      const principal = await apiKeyPrincipal(env, token);
      teamId = principal.teamId || DEFAULT_TEAM;
      const r = await team(env, teamId).api(
        { ...callMeta(ctx), principal },
        "workers.devRegister",
        { id: workerId },
        null,
      );
      if (isOpError(r)) throw new OfError(r.$error.code as never, r.$error.detail);
      mode = "apikey";
    }
    const headers = new Headers({
      upgrade: "websocket",
      connection: "Upgrade",
      "sec-websocket-protocol": WORKER_SUBPROTOCOL,
      "x-of-worker-id": workerId,
      "x-of-auth-mode": mode,
    });
    return await hub(env, teamId, workerId).fetch(new Request("https://worker-hub/connect", { headers }));
  } catch (e) {
    return problemResponse(toOfError(e).error, problem);
  }
}
