// Entry of the MCP surface of the control plane (WS11). Stateless JSON-RPC
// in the Worker; there is no MCP session DO.
//
//   /mcp, /api/v1/mcp/factory              Factory MCP (API key or ofm_ token)
//   /mcp/proxy/{runId}/{serverId}          registry proxy (ofs_ session token)
//   /mcp/integrations/{provider}/{runId}   built-in integration servers (ofs_)
//   /.well-known/oauth-*, /oauth/*         Factory MCP browser sign-in
//
// /mcp/run/{runId} (the per-run factory tools) belongs to WS5 (src/orchestration).
import type { Principal } from "../contracts/do-messages";
import type { Env } from "../contracts/env";
import { apiKeyPrincipal, registerBearerAuthenticator } from "../auth/authenticate";
import { bearerToken, isApiKey } from "../auth/apikeys";
import { ACCESS_TOKEN_PREFIX, FACTORY_MCP_ALIAS_PATH, FACTORY_MCP_PATH } from "./auth/core";
import { handleOAuthRequest, resourceMetadataURL, verifyFactoryMcpToken } from "./auth/handlers";
import { serveMcp, type McpServer } from "./jsonrpc";
import { readGuarded } from "./mergeguard";
import { integrationContext, unavailableIntegrations, type IntegrationBackend } from "./registry/builtin";
import { serveProxy } from "./registry/proxy";
import { INTEGRATION_PATH_PREFIX, PROXY_PATH_PREFIX } from "./registry/store";
import { authErrorResponse, defaultRunBackend, runError, runToken, type McpRunBackend } from "./run";
import { mcpRuntime } from "./runtime";
import { defaultFactoryBackend, type FactoryMcpBackend } from "./server/backend";
import { FACTORY_MCP_INSTRUCTIONS, FACTORY_TOOLS, type FactoryCtx } from "./server/tools";

export { verifyFactoryMcpToken } from "./auth/handlers";
export { setMcpRuntime } from "./runtime";

// ofm_ bearers on /api/v1 authenticate through WS4's authenticate().
registerBearerAuthenticator(ACCESS_TOKEN_PREFIX, verifyFactoryMcpToken);

export interface McpBackends {
  run: McpRunBackend;
  integrations: IntegrationBackend;
  factory: FactoryMcpBackend;
}

const backends: McpBackends = {
  run: defaultRunBackend,
  integrations: unavailableIntegrations,
  factory: defaultFactoryBackend,
};

/** Replaces backends (wiring of the integration clients; fakes in tests). */
export function setMcpBackends(b: Partial<McpBackends>): void {
  Object.assign(backends, b);
}

export function resetMcpBackends(): void {
  Object.assign(backends, {
    run: defaultRunBackend,
    integrations: unavailableIntegrations,
    factory: defaultFactoryBackend,
  });
}

const FACTORY_SERVER: McpServer<FactoryCtx> = {
  info: { name: "openfactory", title: "Open Factory", version: "1.0.0" },
  instructions: FACTORY_MCP_INSTRUCTIONS,
  tools: FACTORY_TOOLS,
};

/** The Factory MCP server (tools/list snapshot tests). */
export const factoryMcpServer = FACTORY_SERVER;

function unauthorized(env: Env, path: string, desc: string): Response {
  const md = env.OF_PUBLIC_URL ? `, resource_metadata="${resourceMetadataURL(env, path)}"` : "";
  return new Response(desc + "\n", {
    status: 401,
    headers: {
      "www-authenticate": `Bearer error="invalid_token", error_description="${desc}"${md}`,
      "content-type": "text/plain",
    },
  });
}

/** Authenticates a Factory MCP request: API key (of_/ofa_) or browser sign-in token (ofm_). */
async function factoryPrincipal(request: Request, env: Env): Promise<Principal | string> {
  const tok = bearerToken(request);
  if (tok === "") return "no bearer token";
  if (tok.startsWith(ACCESS_TOKEN_PREFIX)) {
    const p = await verifyFactoryMcpToken(env, tok);
    return p ?? "invalid or expired sign-in token";
  }
  if (!isApiKey(tok)) return "invalid API key";
  try {
    return await apiKeyPrincipal(env, tok);
  } catch {
    return "invalid API key";
  }
}

async function serveFactory(request: Request, env: Env, path: string): Promise<Response> {
  const p = await factoryPrincipal(request, env);
  if (typeof p === "string") return unauthorized(env, path, p);
  const ctx: FactoryCtx = {
    env,
    principal: p,
    backend: backends.factory,
    publicURL: (env.OF_PUBLIC_URL ?? "").replace(/\/+$/, ""),
  };
  return serveMcp(request, FACTORY_SERVER, ctx);
}

/** Splits "/mcp/proxy/a/b" into ["a", "b"] (exactly two non-empty segments). */
function twoSegments(rest: string): [string, string] | null {
  const parts = rest.split("/");
  if (parts.length !== 2 || parts[0] === "" || parts[1] === "") return null;
  return [decodeURIComponent(parts[0]!), decodeURIComponent(parts[1]!)];
}

async function serveRunEndpoint(
  request: Request,
  env: Env,
  kind: "proxy" | "integration",
  a: string,
  b: string,
): Promise<Response> {
  const runId = kind === "proxy" ? a : b;
  const param = kind === "proxy" ? b : a; // serverId or provider
  const auth = await backends.run.authenticate(env, runId, runToken(request));
  if (!("run" in auth)) return authErrorResponse(auth);
  const run = auth.run;
  // Merge guard before the server (internal/mergeguard), then the handler.
  const guarded = await readGuarded(request, {
    allow: () => backends.run.allowAgentMerge(env, run),
    record: (tool, allowed) => backends.run.recordMergeCall(env, run, param, tool, allowed),
  });
  if ("response" in guarded) return guarded.response;
  const rt = mcpRuntime(env);
  if (kind === "proxy") return serveProxy(request, env, rt, backends.run, run, param, guarded.text);
  const ic = await integrationContext(env, backends.run, backends.integrations, run, param);
  if ("response" in ic) return ic.response;
  const replay = new Request(request.url, {
    method: request.method,
    headers: request.headers,
    body: request.method === "POST" ? guarded.text : null,
  });
  return serveMcp(replay, ic.server, ic.ctx);
}

/**
 * Serves the MCP paths of the control plane, or returns null when the path is
 * not one of them (so the caller tries the next handler).
 */
export async function handleMcpRequest(request: Request, env: Env): Promise<Response | null> {
  const url = new URL(request.url);
  const path = url.pathname;
  try {
    if (path === FACTORY_MCP_PATH || path === FACTORY_MCP_ALIAS_PATH) return await serveFactory(request, env, path);
    if (path.startsWith(PROXY_PATH_PREFIX)) {
      const seg = twoSegments(path.slice(PROXY_PATH_PREFIX.length));
      return seg
        ? await serveRunEndpoint(request, env, "proxy", seg[0], seg[1])
        : runError(404, "not_found", "not found");
    }
    if (path.startsWith(INTEGRATION_PATH_PREFIX)) {
      const seg = twoSegments(path.slice(INTEGRATION_PATH_PREFIX.length));
      return seg
        ? await serveRunEndpoint(request, env, "integration", seg[0], seg[1])
        : runError(404, "not_found", "not found");
    }
    if (path.startsWith("/.well-known/oauth-") || path.startsWith("/oauth/"))
      return await handleOAuthRequest(request, env);
  } catch (e) {
    console.error("mcp: internal error", request.method, path, e);
    return runError(500, "internal", "internal error");
  }
  return null;
}
