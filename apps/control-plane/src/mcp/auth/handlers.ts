// Worker endpoints of the Factory MCP authorization server (port of
// internal/mcpauth/handlers.go). State and decisions live in TeamDO
// (team:default: ofm_ tokens carry no team); this file parses requests,
// finds the signed-in dashboard user and renders the answers.
import type { Principal } from "../../contracts/do-messages";
import type { Env } from "../../contracts/env";
import { authenticate, sessionPrincipal } from "../../auth/authenticate";
import { bearerToken } from "../../auth/apikeys";
import { mcpRuntime } from "../runtime";
import { teamOp } from "../team";
import {
  ACCESS_TOKEN_PREFIX,
  AUTHORIZE_PATH,
  REGISTER_PATH,
  RESOURCE_METADATA_PATH,
  RESOURCE_PATHS,
  REVOKE_PATH,
  SCOPE,
  SERVER_METADATA_PATH,
  TOKEN_PATH,
  oauthError,
  renderPage,
  type Outcome,
} from "./core";
import { hashOf } from "./store";

const publicURL = (env: Env) => (env.OF_PUBLIC_URL ?? "").replace(/\/+$/, "");

/** The metadata URL of a protected resource, for WWW-Authenticate of a 401. */
export function resourceMetadataURL(env: Env, path: string): string {
  return publicURL(env) + RESOURCE_METADATA_PATH + path;
}

function json(status: number, body: unknown, headers: Record<string, string> = {}): Response {
  return new Response(JSON.stringify(body) + "\n", { status, headers: { "content-type": "application/json", ...headers } });
}

export function outcomeResponse(o: Outcome, extra: Record<string, string> = {}): Response {
  switch (o.kind) {
    case "json":
      return json(o.status, o.body, extra);
    case "redirect":
      return new Response(null, { status: 302, headers: { location: o.location, ...extra } });
    case "page":
      return new Response(renderPage(o.page), {
        status: o.status,
        headers: {
          "content-type": "text/html; charset=utf-8",
          "cache-control": "no-store",
          "x-frame-options": "DENY",
          "content-security-policy": "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'",
        },
      });
  }
}

function resourceMetadata(env: Env, path: string): Response {
  const u = publicURL(env);
  const p = path === "" ? RESOURCE_PATHS[0] : path;
  return json(200, {
    resource: u + p,
    authorization_servers: [u],
    scopes_supported: [SCOPE],
    bearer_methods_supported: ["header"],
    resource_name: "Open Factory Factory MCP",
    resource_documentation: u + "/docs/factory-mcp",
  });
}

function serverMetadata(env: Env): Response {
  const u = publicURL(env);
  return json(200, {
    issuer: u,
    authorization_endpoint: u + AUTHORIZE_PATH,
    token_endpoint: u + TOKEN_PATH,
    registration_endpoint: u + REGISTER_PATH,
    revocation_endpoint: u + REVOKE_PATH,
    scopes_supported: [SCOPE],
    response_types_supported: ["code"],
    response_modes_supported: ["query"],
    grant_types_supported: ["authorization_code", "refresh_token"],
    token_endpoint_auth_methods_supported: ["none"],
    revocation_endpoint_auth_methods_supported: ["none"],
    code_challenge_methods_supported: ["S256"],
    authorization_response_iss_parameter_supported: true,
  });
}

/** The signed-in dashboard user of a browser request (session login only), or "". */
async function sessionUser(request: Request, env: Env): Promise<string> {
  if (bearerToken(request) !== "") return "";
  let p: Principal | null = null;
  try {
    // A POST must pass the CSRF check of the dashboard session.
    p = request.method === "GET" ? await sessionPrincipal(env, request) : await authenticate(request, env);
  } catch (e) {
    console.warn("session for MCP sign-in", (e as Error).message);
    return "";
  }
  if (!p || p.kind !== "user") return "";
  return p.userId ?? p.id;
}

async function form(request: Request): Promise<Record<string, string> | null> {
  try {
    const text = new TextDecoder().decode(await request.arrayBuffer());
    const out: Record<string, string> = {};
    for (const [k, v] of new URLSearchParams(text)) if (!(k in out)) out[k] = v;
    return out;
  } catch {
    return null;
  }
}

function firstValues(sp: URLSearchParams): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [k, v] of sp) if (!(k in out)) out[k] = v;
  return out;
}

/**
 * Serves the authorization server paths, or returns null for other paths.
 * Paths: /.well-known/oauth-protected-resource[/mcp|/api/v1/mcp/factory],
 * /.well-known/oauth-authorization-server, /oauth/{register,authorize,token,revoke}.
 */
export async function handleOAuthRequest(request: Request, env: Env): Promise<Response | null> {
  const url = new URL(request.url);
  const path = url.pathname;
  const m = request.method;
  const rt = mcpRuntime(env);
  if (m === "GET" && path.startsWith(RESOURCE_METADATA_PATH)) {
    const rest = path.slice(RESOURCE_METADATA_PATH.length);
    if (rest === "" || (RESOURCE_PATHS as readonly string[]).includes(rest)) return resourceMetadata(env, rest);
    return null;
  }
  if (m === "GET" && path === SERVER_METADATA_PATH) return serverMetadata(env);
  if (path === REGISTER_PATH && m === "POST") {
    const text = await request.text();
    if (text.length > 64 << 10) return outcomeResponse(oauthError(400, "invalid_client_metadata", "the body must be a JSON client metadata document"));
    let metadata: unknown;
    try {
      metadata = JSON.parse(text);
    } catch {
      return outcomeResponse(oauthError(400, "invalid_client_metadata", "the body must be a JSON client metadata document"));
    }
    return outcomeResponse(await teamOp<Outcome>(env, "mcpauth.register", { metadata, now: rt.now() }));
  }
  if (path === AUTHORIZE_PATH && m === "GET") {
    const userId = await sessionUser(request, env);
    const o = await teamOp<Outcome>(env, "mcpauth.authorize", {
      publicURL: publicURL(env),
      query: firstValues(url.searchParams),
      rawQuery: url.search,
      userId,
      now: rt.now(),
    });
    return outcomeResponse(o);
  }
  if (path === AUTHORIZE_PATH && m === "POST") {
    const f = await form(request.clone());
    if (!f) {
      return outcomeResponse({ kind: "page", status: 400, page: { title: "Sign-in failed", message: "The form could not be read." } });
    }
    const userId = await sessionUser(request, env);
    const o = await teamOp<Outcome>(env, "mcpauth.consent", {
      publicURL: publicURL(env),
      requestId: f.request_id ?? "",
      decision: f.decision ?? "",
      userId,
      now: rt.now(),
    });
    return outcomeResponse(o);
  }
  if (path === TOKEN_PATH && m === "POST") {
    const f = await form(request);
    if (!f) return outcomeResponse(oauthError(400, "invalid_request", "the body must be form encoded"), { "cache-control": "no-store" });
    const o = await teamOp<Outcome>(env, "mcpauth.token", { form: f, now: rt.now() });
    return outcomeResponse(o, { "cache-control": "no-store" });
  }
  if (path === REVOKE_PATH && m === "POST") {
    const f = await form(request);
    if (!f) return outcomeResponse(oauthError(400, "invalid_request", "the body must be form encoded"));
    try {
      await teamOp(env, "mcpauth.revoke", { token: f.token ?? "", now: rt.now() });
    } catch (e) {
      console.warn("revoke MCP sign-in", (e as Error).message);
    }
    return new Response(null, { status: 200 });
  }
  return null;
}

/**
 * Checks a Factory MCP access token (ofm_) and returns the principal of its
 * user, or null. Registered with WS4's authenticate() for ofm_ bearers.
 */
export async function verifyFactoryMcpToken(env: Env, token: string): Promise<Principal | null> {
  if (!token.startsWith(ACCESS_TOKEN_PREFIX)) return null;
  const res = await teamOp<{ principal: Principal; grantId: string } | null>(env, "mcpauth.verify", {
    accessHash: await hashOf(token),
    now: mcpRuntime(env).now(),
  });
  return res?.principal ?? null;
}
