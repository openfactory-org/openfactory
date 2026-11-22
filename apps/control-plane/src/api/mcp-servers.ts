// REST handlers: mcp-servers (WS11). Port of internal/api/mcpservers.go.
// CRUD runs in TeamDO (module mcp). The OAuth routes read in TeamDO (which
// also authorizes), run the discovery and token calls here in the Worker, and
// store the result in TeamDO.
// Keep the exported names: src/http/routes.ts maps each openapi operation to one of them.
import { OfError, toOfError, type RegisteredMCPServer } from "@openfactory/core";
import { callMeta, callTeam, json, opParams, teamApi, teamOf, type Handler } from "../http/handler";
import { completeOAuth, startOAuth } from "../mcp/registry/oauth";
import { mcpRuntime } from "../mcp/runtime";
import { teamOp } from "../mcp/team";

/** GET /api/v1/mcp-servers: List registered MCP servers and the built-in servers (owner DO: TeamDO). */
export const listMCPServers: Handler = teamApi();

/** POST /api/v1/mcp-servers: Register an MCP server (owner DO: TeamDO). */
export const createMCPServer: Handler = teamApi();

/** GET /api/v1/mcp-servers/oauth/callback: OAuth redirect target; finishes a connection and redirects to the dashboard (owner DO: TeamDO). */
export const mcpOAuthCallback: Handler = async (_request, env, ctx) => {
  const q = ctx.url.searchParams;
  let providerErr = q.get("error") ?? "";
  const d = q.get("error_description") ?? "";
  if (providerErr !== "" && d !== "") providerErr += ": " + d;
  const meta = callMeta(ctx);
  const read = await teamOp<{ envelope: string; userId: string }>(env, "mcpOAuthCallback", undefined, {
    meta,
    params: opParams(ctx),
    teamId: teamOf(ctx),
  });
  let server: RegisteredMCPServer | null = null;
  let err: OfError | undefined;
  try {
    const res = await completeOAuth(env, mcpRuntime(env), {
      teamId: teamOf(ctx),
      userId: read.userId,
      state: q.get("state") ?? "",
      envelope: read.envelope,
      code: q.get("code") ?? "",
      providerErr,
    });
    server = res.server;
    err = res.error;
  } catch (e) {
    const { error, known } = toOfError(e);
    if (!known || (error.code !== "invalid_request" && error.code !== "resource_not_found")) throw error;
    err = error;
  }
  let target = "/mcp-servers";
  if (server) target += "/" + encodeURIComponent(server.id);
  const v = new URLSearchParams();
  if (err) {
    v.set("message", err.detail || err.code);
    v.set("oauth", "error");
  } else v.set("oauth", "connected");
  // Go url.Values.Encode sorts the keys.
  const qs = new URLSearchParams([...v.entries()].sort(([a], [b]) => (a < b ? -1 : 1))).toString();
  return new Response(null, {
    status: 302,
    headers: { location: (env.OF_PUBLIC_URL ?? "").replace(/\/+$/, "") + target + "?" + qs },
  });
};

/** GET /api/v1/mcp-servers/{id}: Get a registered MCP server (owner DO: TeamDO). */
export const getMCPServer: Handler = teamApi();

/** PATCH /api/v1/mcp-servers/{id}: Update a registered MCP server (owner DO: TeamDO). */
export const updateMCPServer: Handler = teamApi();

/** DELETE /api/v1/mcp-servers/{id}: Delete a registered MCP server and its stored OAuth tokens (owner DO: TeamDO). */
export const deleteMCPServer: Handler = teamApi();

/** POST /api/v1/mcp-servers/{id}/oauth/connect: Start the OAuth connection; returns the provider's authorization URL (owner DO: TeamDO). */
export const connectMCPServer: Handler = async (_request, env, ctx) => {
  const meta = callMeta(ctx);
  const read = await teamOp<{ server: RegisteredMCPServer; userId: string }>(env, "connectMCPServer", undefined, {
    meta,
    params: opParams(ctx),
    teamId: teamOf(ctx),
  });
  let url: string;
  try {
    url = await startOAuth(env, mcpRuntime(env), meta, read.server, read.userId);
  } catch (e) {
    const { error, known } = toOfError(e);
    if (known && error.code !== "internal_error") throw error;
    // Discovery, registration or network failures at the provider.
    throw new OfError("integration_not_configured", `could not start the OAuth connection: ${(e as Error).message}`);
  }
  return json({ authorization_url: url });
};

/** POST /api/v1/mcp-servers/{id}/oauth/disconnect: Delete the stored OAuth tokens (owner DO: TeamDO). */
export const disconnectMCPServer: Handler = async (_request, env, ctx) => callTeam(env, ctx, "disconnectMCPServer");
