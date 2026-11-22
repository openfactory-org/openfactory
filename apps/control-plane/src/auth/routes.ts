// Server routes of the auth service outside /api/v1 (internal/auth/service.go):
//   POST /auth/session            API-key login, JSON {"api_key": "of_..."}
//   POST /auth/logout             clears the session
//   GET  /auth/oidc/login         ?return_to=/path
//   GET  /auth/oidc/callback
import { DEFAULT_TEAM, OfError, doName } from "@openfactory/core";
import type { Env } from "../contracts/env";
import { serverConfig } from "../config";
import { csrfOk } from "../http/csrf";
import { errorResponse, json, opResult } from "../http/handler";
import type { Identity } from "../do/team/auth";
import { apiKeyPrincipal } from "./authenticate";
import { parseKey } from "./apikeys";
import { finishLogin, LoginFailure, OIDC_FLOW_TTL_MS, oidcConfigFromEnv, safeReturnTo, startLogin, type OIDCFlow } from "./oidc";
import { clearSessionCookie, issueSessionCookie, loadFlow, readSession, saveFlow } from "./session";

export const LOGIN_ERROR_URL = "/login";

export interface LoginProviderInfo {
  name: string;
  display_name: string;
  login_url: string;
}

/** LoginProviders: the interactive providers plus "api_key". */
export function loginProviders(env: Env): LoginProviderInfo[] {
  const out: LoginProviderInfo[] = [];
  try {
    const c = oidcConfigFromEnv(env);
    if (c) out.push({ name: "oidc", display_name: c.displayName, login_url: "/auth/oidc/login" });
  } catch (e) {
    console.error("auth: OIDC configuration", (e as Error).message);
  }
  out.push({ name: "api_key", display_name: "API key", login_url: "/auth/session" });
  return out;
}

function audit(action: string, outcome: "success" | "failure", detail: Record<string, unknown>): void {
  const line = { audit: true, action, outcome, ...detail };
  if (outcome === "failure") console.warn("audit: " + action, line);
  else console.log("audit: " + action, line);
}

const redirect = (to: string, cookies: string[] = [], status = 303) => {
  const h = new Headers({ location: to, "cache-control": "no-store" });
  for (const c of cookies) h.append("set-cookie", c);
  return new Response(null, { status, headers: h });
};

const csrfProblem = () => new OfError("not_authorized", "missing X-OF-CSRF: 1 header");

async function sessionLogin(req: Request, env: Env, instance: string): Promise<Response> {
  if (!csrfOk(req, serverConfig(env).origin)) throw csrfProblem();
  let key = "";
  try {
    const text = await req.text();
    if (text.length > 4 << 10) throw new Error("too large");
    const b = JSON.parse(text) as { api_key?: unknown };
    key = typeof b.api_key === "string" ? b.api_key.trim() : "";
  } catch {
    key = "";
  }
  if (key === "") throw new OfError("invalid_request", 'body must be JSON {"api_key": "of_..."}');
  let p;
  try {
    p = await apiKeyPrincipal(env, key);
  } catch (e) {
    audit("auth.session", "failure", { provider: "api_key", reason: (e as Error).message });
    throw e;
  }
  const cookie = await issueSessionCookie(env, { u: p.userId ?? p.id, t: p.teamId, p: "api_key", k: p.keyId ?? "", kp: parseKey(key)?.prefix ?? "" });
  audit("auth.session", "success", { provider: "api_key", user_id: p.userId, team_id: p.teamId, api_key_id: p.keyId });
  const me = await env.TEAM.getByName(doName.team(p.teamId)).api({ requestId: instance, principal: p }, "authMe", {}, undefined);
  const res = opResult(me);
  const body = (await res.json()) as Record<string, unknown>;
  return json({ ...body, auth_method: "session", provider: "api_key", login_providers: loginProviders(env) }, 200, {
    "set-cookie": cookie,
    "cache-control": "no-store",
  });
}

async function logout(req: Request, env: Env): Promise<Response> {
  if (!csrfOk(req, serverConfig(env).origin)) throw csrfProblem();
  const c = await readSession(env, req);
  audit("auth.logout", "success", c ? { user_id: c.u, team_id: c.t, provider: c.p } : {});
  return new Response(null, { status: 204, headers: { "set-cookie": clearSessionCookie(env) } });
}

function failLogin(provider: string, err: unknown, cookies: string[] = []): Response {
  const code = err instanceof LoginFailure ? err.code : err instanceof OfError && err.reason ? err.reason : "internal";
  audit("auth.login", "failure", { provider, reason: (err as Error).message });
  if (code === "internal") console.error("auth: login failed", provider, err);
  return redirect(`${LOGIN_ERROR_URL}?error=${encodeURIComponent(code)}`, cookies);
}

async function oidcLogin(req: Request, env: Env, url: URL): Promise<Response> {
  const c = oidcConfigFromEnv(env);
  if (!c) return new Response("not found\n", { status: 404 });
  try {
    const { flow, url: to } = await startLogin(c, safeReturnTo(url.searchParams.get("return_to")));
    const cookie = await saveFlow(env, "oidc", flow, OIDC_FLOW_TTL_MS);
    return redirect(to, [cookie], 302);
  } catch (e) {
    return failLogin("oidc", e instanceof LoginFailure ? e : new LoginFailure("provider_unavailable", (e as Error).message));
  }
}

async function oidcCallback(req: Request, env: Env, url: URL): Promise<Response> {
  const c = oidcConfigFromEnv(env);
  if (!c) return new Response("not found\n", { status: 404 });
  const { value: flow, clear } = await loadFlow<OIDCFlow>(env, req, "oidc");
  if (!flow) return failLogin("oidc", new LoginFailure("invalid_state", "login flow cookie missing (expired, or cookies blocked)"), [clear]);
  let id: Identity;
  try {
    id = await finishLogin(c, url.searchParams, flow);
  } catch (e) {
    return failLogin("oidc", e, [clear]);
  }
  try {
    const out = (await env.TEAM.getByName(doName.team(DEFAULT_TEAM)).api({ requestId: "" }, "authProvision", {}, id)) as {
      $error?: { code: string; detail: string; reason?: string };
      user: { id: string; team_id: string; role: string };
    };
    if (out.$error) throw new OfError("invalid_request", out.$error.detail, out.$error.reason ? { reason: out.$error.reason } : {});
    const cookie = await issueSessionCookie(env, { u: out.user.id, t: out.user.team_id, p: "oidc" });
    audit("auth.login", "success", { provider: "oidc", user_id: out.user.id, team_id: out.user.team_id, subject: id.subject, role: out.user.role });
    return redirect(safeReturnTo(flow.r), [clear, cookie]);
  } catch (e) {
    return failLogin("oidc", e, [clear]);
  }
}

/** Handles /auth/*; returns null for other paths. */
export async function handleAuthRoute(req: Request, env: Env, url: URL, requestId: string): Promise<Response | null> {
  const p = url.pathname;
  try {
    if (p === "/auth/session" && req.method === "POST") return await sessionLogin(req, env, requestId);
    if (p === "/auth/logout" && req.method === "POST") return await logout(req, env);
    if (p === "/auth/oidc/login" && req.method === "GET") return await oidcLogin(req, env, url);
    if (p === "/auth/oidc/callback" && req.method === "GET") return await oidcCallback(req, env, url);
  } catch (e) {
    return errorResponse(e, { instance: p, requestId });
  }
  return null;
}
