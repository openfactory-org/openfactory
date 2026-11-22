// REST handlers: mcp-sign-ins. C0 stubs that answer 501. Owner: WS11.
// Keep the exported names: src/http/routes.ts maps each openapi operation to one of them.
import { notImplementedHandler, type Handler } from "../http/handler";

/** GET /api/v1/mcp-sign-ins: Active Factory MCP browser sign-ins (owner DO: TeamDO). */
export const listMCPSignIns: Handler = notImplementedHandler;

/** DELETE /api/v1/mcp-sign-ins/{id}: End a Factory MCP sign-in (owner DO: TeamDO). */
export const revokeMCPSignIn: Handler = notImplementedHandler;
