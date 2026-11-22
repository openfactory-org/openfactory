// TeamDO side of the MCP server registry (internal/api/mcpservers.go and the
// store half of internal/mcpregistry). Every function here is synchronous and
// runs inside the TeamDO; call writers inside kit.tx.
import {
  BUILTIN_MCP_PREFIX,
  BUILTIN_MCP_PROVIDERS,
  MCP_OAUTH_CONNECTED,
  MCP_OAUTH_ERROR,
  MCP_OAUTH_NOT_CONNECTED,
  OfError,
  builtinMCPProvider,
  newId,
  sameIntegrationProvider,
  type MCPOAuthConfig,
  type MCPServer,
  type RegisteredMCPServer,
} from "@openfactory/core";
import type { TeamKit } from "../../do/team/index";
import { bodyObject, optString, optStringArray, optStringMap, type JsonObject } from "../../lib/body";
import { all, first } from "../../lib/sql/query";
import { msOf } from "../../lib/page";
import { defaultConnection, privateHostWarning, proxied, validate } from "./validate";

/** Paths of the run endpoints. */
export const PROXY_PATH_PREFIX = "/mcp/proxy/";
export const INTEGRATION_PATH_PREFIX = "/mcp/integrations/";
/** The OAuth redirect path below the public URL. */
export const CALLBACK_PATH = "/api/v1/mcp-servers/oauth/callback";

export type ServerWithWarnings = RegisteredMCPServer & { warnings?: string[] };

export function out(m: RegisteredMCPServer, ...warnings: string[]): ServerWithWarnings {
  const ws = warnings.filter((w) => w !== "");
  return ws.length > 0 ? { ...m, warnings: ws } : { ...m };
}

// ---------- reads ----------

export function getServer(kit: TeamKit, id: string): RegisteredMCPServer | undefined {
  const r = first<{ data: string }>(kit.sql, "SELECT data FROM mcp_servers WHERE id = ?", id);
  return r ? (JSON.parse(r.data) as RegisteredMCPServer) : undefined;
}

/** ListMCPServers: ORDER BY name, id. */
export function listServers(kit: TeamKit): RegisteredMCPServer[] {
  return all<{ data: string }>(kit.sql, "SELECT data FROM mcp_servers ORDER BY name, id").map(
    (r) => JSON.parse(r.data) as RegisteredMCPServer,
  );
}

/** mcpServerFor (REST): by id, then by exact name. */
export function serverForRef(kit: TeamKit, ref: string): RegisteredMCPServer {
  const m = getServer(kit, ref) ?? listServers(kit).find((x) => x.name === ref);
  if (!m) throw new OfError("resource_not_found", `MCP server ${ref} not found`);
  return m;
}

/** Lookup (registry): by name, then by id. */
export function lookup(kit: TeamKit, ref: string): RegisteredMCPServer {
  const r = ref.trim();
  if (r === "") throw new OfError("invalid_request", "empty MCP server reference");
  const m = listServers(kit).find((x) => x.name === r) ?? getServer(kit, r);
  if (!m) throw new OfError("resource_not_found", `no MCP server "${r}" is registered for this team`);
  return m;
}

// ---------- writes ----------

export function putServer(kit: TeamKit, m: RegisteredMCPServer, create: boolean): void {
  try {
    if (create) {
      kit.sql.exec(
        "INSERT INTO mcp_servers (id, name, created_ms, data) VALUES (?, ?, ?, ?)",
        m.id,
        m.name,
        msOf(m.created_at),
        JSON.stringify(m),
      );
    } else {
      kit.sql.exec("UPDATE mcp_servers SET name = ?, data = ? WHERE id = ?", m.name, JSON.stringify(m), m.id);
    }
  } catch (e) {
    if (/UNIQUE/i.test(String(e))) throw new OfError("conflict", `an MCP server named "${m.name}" already exists`);
    throw e;
  }
}

/** Deletes the record and its stored tokens. */
export function deleteServer(kit: TeamKit, id: string): void {
  kit.sql.exec("DELETE FROM mcp_servers WHERE id = ?", id);
  forget(kit, id);
}

/** Forget: deletes stored tokens without touching the record. */
export function forget(kit: TeamKit, serverId: string): void {
  kit.sql.exec("DELETE FROM mcp_oauth_tokens WHERE server_id = ?", serverId);
}

/** Names are unique without regard to case (checkMCPServerNameFree). */
export function checkNameFree(kit: TeamKit, name: string, exceptId: string): void {
  for (const m of listServers(kit)) {
    if (m.id !== exceptId && m.name.toLowerCase() === name.toLowerCase()) {
      throw new OfError("conflict", `an MCP server named "${m.name}" already exists`);
    }
  }
}

/** setStatus: records the OAuth state of a server (message capped at 500 bytes). */
export function setStatus(kit: TeamKit, serverId: string, status: string, msg: string, nowMs: number): RegisteredMCPServer | undefined {
  const m = getServer(kit, serverId);
  if (!m?.oauth) return m;
  m.oauth.status = status;
  const e = msg.length > 500 ? msg.slice(0, 500) : msg;
  if (e) m.oauth.error = e;
  else delete m.oauth.error;
  m.updated_at = new Date(nowMs).toISOString();
  putServer(kit, m, false);
  if (status === MCP_OAUTH_ERROR) console.warn("mcp server oauth error", m.name, e);
  return m;
}

export function markConnected(kit: TeamKit, serverId: string, userId: string, nowMs: number): RegisteredMCPServer | undefined {
  const m = getServer(kit, serverId);
  if (!m?.oauth) return m;
  const now = new Date(nowMs).toISOString();
  m.oauth.status = MCP_OAUTH_CONNECTED;
  delete m.oauth.error;
  m.oauth.connected_by = userId;
  m.oauth.connected_at = now;
  m.updated_at = now;
  putServer(kit, m, false);
  return m;
}

/** Disconnect: deletes the token and marks the server not connected. */
export function disconnect(kit: TeamKit, serverId: string, nowMs: number): RegisteredMCPServer | undefined {
  forget(kit, serverId);
  const m = getServer(kit, serverId);
  if (m?.oauth) {
    m.oauth = notConnected(m.oauth);
    m.updated_at = new Date(nowMs).toISOString();
    putServer(kit, m, false);
  }
  return m;
}

function notConnected(o: MCPOAuthConfig): MCPOAuthConfig {
  const next: MCPOAuthConfig = { ...o, status: MCP_OAUTH_NOT_CONNECTED };
  delete next.connected_by;
  delete next.connected_at;
  delete next.error;
  return next;
}

// ---------- request bodies (mcpServerRequest) ----------

const OAUTH_FIELDS = new Set(["scopes", "client_id", "client_secret_name", "authorization_endpoint", "token_endpoint"]);

const sameList = (a: string[] | undefined, b: string[] | undefined) =>
  (a ?? []).length === (b ?? []).length && (a ?? []).every((x, i) => x === (b ?? [])[i]);

/**
 * Copies set fields into m. Returns whether a change invalidates a stored
 * OAuth token (url, oauth client or endpoints).
 */
export function applyRequest(m: RegisteredMCPServer, o: JsonObject): boolean {
  let authChanged = false;
  const name = optString(o, "name");
  if (name !== undefined) m.name = name;
  const desc = optString(o, "description");
  if (desc !== undefined) {
    const d = desc.trim();
    if (d) m.description = d;
    else delete m.description;
  }
  const url = optString(o, "url");
  if (url !== undefined) {
    authChanged ||= url.trim() !== (m.url ?? "");
    m.url = url;
  }
  const headers = optStringMap(o, "headers");
  if (headers !== undefined) m.headers = headers;
  const command = optString(o, "command");
  if (command !== undefined) m.command = command;
  const args = optStringArray(o, "args");
  if (args !== undefined) m.args = args;
  const env = optStringMap(o, "env");
  if (env !== undefined) m.env = env;
  if ("oauth" in o) {
    const v = o.oauth;
    if (v === null) {
      authChanged ||= !!m.oauth;
      delete m.oauth;
    } else if (v !== undefined) {
      if (typeof v !== "object" || Array.isArray(v)) throw new OfError("invalid_request", "oauth: must be an object");
      const req = v as JsonObject;
      for (const k of Object.keys(req)) {
        if (!OAUTH_FIELDS.has(k)) throw new OfError("invalid_request", `oauth: json: unknown field "${k}"`);
      }
      const next: MCPOAuthConfig = { status: MCP_OAUTH_NOT_CONNECTED };
      const scopes = optStringArray(req, "scopes");
      if (scopes && scopes.length > 0) next.scopes = scopes;
      for (const k of ["client_id", "client_secret_name", "authorization_endpoint", "token_endpoint"] as const) {
        const s = optString(req, k);
        if (s) next[k] = s;
      }
      const prev = m.oauth;
      if (prev) {
        const same =
          (prev.client_id ?? "") === (next.client_id ?? "") &&
          (prev.client_secret_name ?? "") === (next.client_secret_name ?? "") &&
          (prev.authorization_endpoint ?? "") === (next.authorization_endpoint ?? "") &&
          (prev.token_endpoint ?? "") === (next.token_endpoint ?? "") &&
          sameList(prev.scopes, next.scopes);
        if (same) {
          next.status = prev.status;
          if (prev.connected_by) next.connected_by = prev.connected_by;
          if (prev.connected_at) next.connected_at = prev.connected_at;
          if (prev.error) next.error = prev.error;
        } else authChanged = true;
      }
      m.oauth = next;
    }
  }
  const conn = optString(o, "connection");
  if (conn !== undefined) m.connection = conn.trim().toLowerCase();
  // Go omitempty for maps and lists.
  for (const k of ["headers", "env"] as const) if (m[k] && Object.keys(m[k]!).length === 0) delete m[k];
  if (m.args && m.args.length === 0) delete m.args;
  return authChanged;
}

// ---------- REST operations ----------

export function createServer(kit: TeamKit, body: unknown, actorId: string, nowMs: number): ServerWithWarnings {
  const o = bodyObject(body);
  if (optString(o, "name") === undefined) throw new OfError("invalid_request", "name is required");
  const now = new Date(nowMs).toISOString();
  const m: RegisteredMCPServer = { id: newId("mcp"), team_id: kit.teamId(), name: "", created_at: now, updated_at: now };
  if (actorId) m.created_by = actorId;
  applyRequest(m, o);
  const warning = defaultConnection(m);
  validate(m);
  return kit.tx(() => {
    checkNameFree(kit, m.name, "");
    putServer(kit, m, true);
    return out(m, warning, privateHostWarning(m));
  });
}

export function updateServer(kit: TeamKit, ref: string, body: unknown, nowMs: number): ServerWithWarnings {
  const o = bodyObject(body);
  return kit.tx(() => {
    const m = serverForRef(kit, ref);
    const connected = m.oauth?.status === MCP_OAUTH_CONNECTED;
    const wasURL = (m.url ?? "") !== "";
    const authChanged = applyRequest(m, o);
    if ((m.url ?? "") === "" && optString(o, "connection") === undefined) {
      // A url server that becomes a command server has no connection mode.
      delete m.connection;
    }
    // A command server that becomes a url server gets the default mode.
    const warning = wasURL ? "" : defaultConnection(m);
    validate(m);
    checkNameFree(kit, m.name, m.id);
    if (authChanged && connected && m.oauth) m.oauth = notConnected(m.oauth);
    m.updated_at = new Date(nowMs).toISOString();
    putServer(kit, m, false);
    if (authChanged) forget(kit, m.id);
    return out(m, warning, privateHostWarning(m));
  });
}

/** Built-in servers in list responses. `providers` are the team's integration providers. */
export function builtinList(providers: readonly string[]): { managed_id: string; provider: string; configured: boolean }[] {
  return BUILTIN_MCP_PROVIDERS.map((p) => ({
    managed_id: BUILTIN_MCP_PREFIX + p,
    provider: p,
    configured: providers.some((x) => sameIntegrationProvider(x, p)),
  }));
}

// ---------- resolve (planner) ----------

export interface RunRef {
  id: string;
  /** The run's declared mcpServers (Run.config.mcpServers). */
  mcpServers?: Record<string, MCPServer>;
}

/**
 * Resolve: turns an agent's mcpServers.<name>.managedId into a transport for
 * the planner. Control-plane servers carry no Authorization header: the
 * runner sends its session token. `sandboxURL` is OF_PUBLIC_URL.
 */
export function resolveManaged(kit: TeamKit, sandboxURL: string, run: RunRef, managedId: string): MCPServer {
  const base = sandboxURL.replace(/\/+$/, "");
  const b = builtinMCPProvider(managedId);
  if (managedId.startsWith(BUILTIN_MCP_PREFIX)) {
    if (!b.known) throw new OfError("invalid_request", `unknown built-in MCP server "${managedId}"`);
    if (base === "") throw new OfError("internal_error", "built-in MCP servers need the server's public URL (OF_PUBLIC_URL)");
    return { url: `${base}${INTEGRATION_PATH_PREFIX}${b.provider}/${run.id}` };
  }
  const m = lookup(kit, managedId);
  if (proxied(m)) {
    if (base === "") {
      throw new OfError("internal_error", `MCP server "${m.name}" is proxied, which needs the server's public URL (OF_PUBLIC_URL)`);
    }
    if (m.oauth && m.oauth.status !== MCP_OAUTH_CONNECTED) {
      throw new OfError(
        "invalid_request",
        `MCP server "${m.name}" is not connected: an editor must connect it (OAuth) in the dashboard`,
      );
    }
    return { url: `${base}${PROXY_PATH_PREFIX}${run.id}/${m.id}` };
  }
  const res: MCPServer = {};
  if (m.url) res.url = m.url;
  if (m.command) res.command = m.command;
  if (m.args && m.args.length > 0) res.args = [...m.args];
  if (m.headers && Object.keys(m.headers).length > 0) res.headers = { ...m.headers };
  if (m.env && Object.keys(m.env).length > 0) res.env = { ...m.env };
  return res;
}

/**
 * grantedTo: whether the run declares an MCP server whose managedId selects
 * one of refs, and whether any such declaration allows writes.
 */
export function grantedTo(mcpServers: Record<string, MCPServer> | undefined, ...refs: string[]): { granted: boolean; allowWrite: boolean } {
  let granted = false;
  let allowWrite = false;
  for (const srv of Object.values(mcpServers ?? {})) {
    for (const r of refs) {
      if (r !== "" && srv.managedId === r) {
        granted = true;
        allowWrite ||= !!srv.allowWrite;
      }
    }
  }
  return { granted, allowWrite };
}

/** Names and IDs of the registered servers (for the managedId preflight of a definition). */
export function knownRefs(kit: TeamKit): Set<string> {
  const s = new Set<string>();
  for (const m of listServers(kit)) {
    s.add(m.name);
    s.add(m.id);
  }
  return s;
}
