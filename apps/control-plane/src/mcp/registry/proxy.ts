// Port of internal/mcpregistry/proxy.go: /mcp/proxy/{runId}/{serverId}
// forwards streamable-HTTP MCP traffic of a run to a registered server and
// adds the server's headers and OAuth access token there, so credentials
// never enter the sandbox. Known limitation (plan): a proxy-mode server on a
// private network cannot be reached from Cloudflare.
import { MCP_OAUTH_ERROR, type RegisteredMCPServer } from "@openfactory/core";
import type { Env } from "../../contracts/env";
import { runError, type McpRunBackend, type RunView } from "../run";
import { teamOp, type McpRuntime } from "../team";
import { NotConnectedError, accessToken } from "./oauth";
import { grantedTo } from "./store";
import { proxied } from "./validate";

/**
 * The request headers passed from the run to the upstream server. Everything
 * else, notably the run's Authorization header and cookies, is dropped.
 */
const FORWARDED_HEADERS = ["accept", "content-type", "mcp-session-id", "mcp-protocol-version", "last-event-id", "user-agent"];

const STATUS_TEXT: Record<number, string> = { 401: "Unauthorized", 403: "Forbidden" };

export async function serveProxy(
  request: Request,
  env: Env,
  rt: McpRuntime,
  backend: McpRunBackend,
  run: RunView,
  serverId: string,
  /** The request body, already read by the merge guard. */
  body: string,
): Promise<Response> {
  const m = await teamOp<RegisteredMCPServer | null>(env, "mcp.server.get", { serverId }, { teamId: run.teamId });
  if (!m || m.team_id !== run.teamId) return runError(404, "not_found", "MCP server not found");
  if (!grantedTo(run.mcpServers, m.id, m.name).granted) {
    return runError(403, "forbidden", "this run was not granted MCP server " + m.name);
  }
  if (!proxied(m) || !m.url) return runError(400, "not_proxied", "MCP server " + m.name + " is not proxied");

  const headers = new Headers();
  for (const h of FORWARDED_HEADERS) {
    const v = request.headers.get(h);
    if (v !== null) headers.set(h, v);
  }
  for (const [k, v0] of Object.entries(m.headers ?? {})) {
    let v = v0;
    if (v.includes("{{")) {
      try {
        v = await backend.expandSecrets(env, run, v);
      } catch (e) {
        console.warn("mcp proxy: header secret", m.name, run.id, k, (e as Error).message);
        return runError(502, "secrets", "a secret referenced by MCP server " + m.name + " is missing");
      }
    }
    headers.set(k, v);
  }
  if (m.oauth) {
    try {
      headers.set("authorization", "Bearer " + (await accessToken(env, rt, m)));
    } catch (e) {
      const code = e instanceof NotConnectedError ? "not_connected" : "upstream_auth";
      return runError(502, code, "MCP server " + m.name + " is not connected: an editor must reconnect it in the dashboard");
    }
  }

  let res: Response;
  try {
    const init: RequestInit = { method: request.method, headers, redirect: "manual" };
    if (request.method !== "GET" && request.method !== "HEAD" && body !== "") init.body = body;
    res = await rt.fetch(m.url, init);
  } catch (e) {
    console.warn("mcp proxy: upstream error", m.name, run.id, (e as Error).message);
    return runError(502, "upstream", "MCP server " + m.name + " is unreachable");
  }
  const out = new Headers(res.headers);
  out.delete("set-cookie");
  if (res.status === 401 || res.status === 403) {
    // Do not hand the upstream's OAuth challenge to the agent's MCP client:
    // credentials are managed here, not in the sandbox.
    out.delete("www-authenticate");
    console.warn("mcp proxy: upstream rejected credentials", m.name, run.id, res.status);
    if (m.oauth) {
      await teamOp(env, "mcp.oauth.setStatus", {
        serverId: m.id,
        status: MCP_OAUTH_ERROR,
        message: `the server rejected the access token (HTTP ${STATUS_TEXT[res.status]}); reconnect it`,
        now: rt.now(),
      }, { teamId: run.teamId }).catch(() => undefined);
    }
  }
  // The body streams through (SSE responses arrive as they are written).
  return new Response(res.body, { status: res.status, statusText: res.statusText, headers: out });
}
