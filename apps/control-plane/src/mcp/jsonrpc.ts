// Stateless MCP tool server over streamable HTTP (JSON responses only).
// The Factory MCP server, the built-in integration servers
// and the run MCP server are request/response tool servers. This module is
// the shared JSON-RPC 2.0 handler: initialize, notifications/initialized,
// ping, tools/list, tools/call and batch arrays. No SSE, no Mcp-Session-Id.

/** Protocol versions this server speaks, newest first. */
export const PROTOCOL_VERSIONS = ["2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"] as const;
export const LATEST_PROTOCOL_VERSION = PROTOCOL_VERSIONS[0];

/** JSON-RPC error codes. */
export const RPC_PARSE_ERROR = -32700;
export const RPC_INVALID_REQUEST = -32600;
export const RPC_METHOD_NOT_FOUND = -32601;
export const RPC_INVALID_PARAMS = -32602;
export const RPC_INTERNAL_ERROR = -32603;

export interface TextContent {
  type: "text";
  text: string;
}

export interface CallToolResult {
  content: TextContent[];
  isError?: boolean;
  _meta?: Record<string, unknown>;
}

export interface ToolAnnotations {
  readOnlyHint?: boolean;
  title?: string;
}

export type JsonSchema = Record<string, unknown>;

/** One tool. `C` is the per-request context the server passes to handlers. */
export interface Tool<C> {
  name: string;
  description: string;
  inputSchema: JsonSchema;
  annotations?: ToolAnnotations;
  handler: (args: Record<string, unknown>, ctx: C) => Promise<CallToolResult> | CallToolResult;
}

export interface ServerInfo {
  name: string;
  title?: string;
  version: string;
}

/** A server for one request: its identity, its instructions and its tools. */
export interface McpServer<C> {
  info: ServerInfo;
  instructions?: string;
  tools: readonly Tool<C>[];
}

type Id = string | number | null;

interface RpcRequest {
  jsonrpc?: unknown;
  id?: Id;
  method?: unknown;
  params?: unknown;
}

export interface RpcResponse {
  jsonrpc: "2.0";
  id: Id;
  result?: unknown;
  error?: { code: number; message: string; data?: unknown };
}

export function textResult(text: string, isError = false): CallToolResult {
  return isError ? { content: [{ type: "text", text }], isError: true } : { content: [{ type: "text", text }] };
}

/** Go json.MarshalIndent(v, "", "  ") of a tool result. */
export function jsonText(v: unknown): CallToolResult {
  return textResult(JSON.stringify(v, null, 2));
}

const isObject = (v: unknown): v is Record<string, unknown> => typeof v === "object" && v !== null && !Array.isArray(v);

function rpcError(id: Id, code: number, message: string): RpcResponse {
  return { jsonrpc: "2.0", id, error: { code, message } };
}

function negotiate(requested: unknown): string {
  return typeof requested === "string" && (PROTOCOL_VERSIONS as readonly string[]).includes(requested)
    ? requested
    : LATEST_PROTOCOL_VERSION;
}

/** Handles one JSON-RPC message. Returns null for a notification. */
export async function handleMessage<C>(server: McpServer<C>, msg: unknown, ctx: C): Promise<RpcResponse | null> {
  if (!isObject(msg)) return rpcError(null, RPC_INVALID_REQUEST, "invalid request");
  const req = msg as RpcRequest;
  const hasId = "id" in req && req.id !== undefined;
  const id: Id = hasId ? (req.id as Id) : null;
  if (req.jsonrpc !== "2.0" || typeof req.method !== "string") {
    // A response sent by the client (no method) needs no answer.
    if (!("method" in req) && ("result" in req || "error" in req)) return null;
    return rpcError(id, RPC_INVALID_REQUEST, "invalid request");
  }
  if (!hasId) return null; // notifications/initialized, notifications/cancelled, ...
  const params = isObject(req.params) ? req.params : {};
  switch (req.method) {
    case "initialize":
      return {
        jsonrpc: "2.0",
        id,
        result: {
          protocolVersion: negotiate(params.protocolVersion),
          capabilities: { tools: { listChanged: false } },
          serverInfo: server.info,
          ...(server.instructions ? { instructions: server.instructions } : {}),
        },
      };
    case "ping":
      return { jsonrpc: "2.0", id, result: {} };
    case "tools/list":
      return {
        jsonrpc: "2.0",
        id,
        result: {
          tools: server.tools.map((t) => ({
            name: t.name,
            description: t.description,
            inputSchema: t.inputSchema,
            ...(t.annotations ? { annotations: t.annotations } : {}),
          })),
        },
      };
    case "tools/call": {
      const name = params.name;
      const tool = typeof name === "string" ? server.tools.find((t) => t.name === name) : undefined;
      if (!tool) return rpcError(id, RPC_INVALID_PARAMS, `unknown tool "${String(name)}"`);
      const args = params.arguments === undefined || params.arguments === null ? {} : params.arguments;
      if (!isObject(args)) return rpcError(id, RPC_INVALID_PARAMS, "arguments must be an object");
      try {
        return { jsonrpc: "2.0", id, result: await tool.handler(args, ctx) };
      } catch (e) {
        return { jsonrpc: "2.0", id, result: textResult((e as Error).message ?? String(e), true) };
      }
    }
    case "resources/list":
      return { jsonrpc: "2.0", id, result: { resources: [] } };
    case "prompts/list":
      return { jsonrpc: "2.0", id, result: { prompts: [] } };
    default:
      return rpcError(id, RPC_METHOD_NOT_FOUND, `method "${req.method}" not found`);
  }
}

const JSON_HEADERS = { "content-type": "application/json" };

/** Upper bound of a request body (the Go merge guard reads up to 16 MiB). */
export const MAX_MCP_BODY = 16 << 20;

/**
 * Serves one streamable-HTTP request: POST with a JSON-RPC message or batch.
 * GET (the server-to-client stream) and DELETE (session end) answer 405,
 * because the server keeps no session.
 */
export async function serveMcp<C>(request: Request, server: McpServer<C>, ctx: C): Promise<Response> {
  if (request.method !== "POST") {
    return new Response("Method Not Allowed\n", { status: 405, headers: { allow: "POST" } });
  }
  const text = await request.text();
  if (text.length > MAX_MCP_BODY) return new Response("the MCP request is too large\n", { status: 413 });
  let body: unknown;
  try {
    body = JSON.parse(text);
  } catch {
    return Response.json(rpcError(null, RPC_PARSE_ERROR, "parse error"), { status: 400 });
  }
  if (Array.isArray(body)) {
    if (body.length === 0) return Response.json(rpcError(null, RPC_INVALID_REQUEST, "empty batch"), { status: 400 });
    const out: RpcResponse[] = [];
    for (const m of body) {
      const r = await handleMessage(server, m, ctx);
      if (r) out.push(r);
    }
    if (out.length === 0) return new Response(null, { status: 202 });
    return new Response(JSON.stringify(out), { status: 200, headers: JSON_HEADERS });
  }
  const r = await handleMessage(server, body, ctx);
  if (!r) return new Response(null, { status: 202 });
  return new Response(JSON.stringify(r), { status: 200, headers: JSON_HEADERS });
}
