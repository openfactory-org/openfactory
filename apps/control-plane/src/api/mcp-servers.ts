// REST handlers: mcp-servers. C0 stubs that answer 501. Owner: WS11.
// Keep the exported names: src/http/routes.ts maps each openapi operation to one of them.
import { notImplementedHandler, type Handler } from "../http/handler";

/** GET /api/v1/mcp-servers: List registered MCP servers and the built-in servers (owner DO: TeamDO). */
export const listMCPServers: Handler = notImplementedHandler;

/** POST /api/v1/mcp-servers: Register an MCP server (owner DO: TeamDO). */
export const createMCPServer: Handler = notImplementedHandler;

/** GET /api/v1/mcp-servers/oauth/callback: OAuth redirect target; finishes a connection and redirects to the dashboard (owner DO: TeamDO). */
export const mcpOAuthCallback: Handler = notImplementedHandler;

/** GET /api/v1/mcp-servers/{id}: Get a registered MCP server (owner DO: TeamDO). */
export const getMCPServer: Handler = notImplementedHandler;

/** PATCH /api/v1/mcp-servers/{id}: Update a registered MCP server (owner DO: TeamDO). */
export const updateMCPServer: Handler = notImplementedHandler;

/** DELETE /api/v1/mcp-servers/{id}: Delete a registered MCP server and its stored OAuth tokens (owner DO: TeamDO). */
export const deleteMCPServer: Handler = notImplementedHandler;

/** POST /api/v1/mcp-servers/{id}/oauth/connect: Start the OAuth connection; returns the provider's authorization URL (owner DO: TeamDO). */
export const connectMCPServer: Handler = notImplementedHandler;

/** POST /api/v1/mcp-servers/{id}/oauth/disconnect: Delete the stored OAuth tokens (owner DO: TeamDO). */
export const disconnectMCPServer: Handler = notImplementedHandler;
