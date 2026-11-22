// REST handlers: mcp-sign-ins (WS11). Port of internal/api/mcpsignins.go;
// the sign-ins (grants of the Factory MCP authorization server) live in TeamDO.
// Keep the exported names: src/http/routes.ts maps each openapi operation to one of them.
import { teamApi, type Handler } from "../http/handler";

/** GET /api/v1/mcp-sign-ins: Active Factory MCP browser sign-ins (owner DO: TeamDO). */
export const listMCPSignIns: Handler = teamApi();

/** DELETE /api/v1/mcp-sign-ins/{id}: End a Factory MCP sign-in (owner DO: TeamDO). */
export const revokeMCPSignIn: Handler = teamApi();
