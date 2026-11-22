// TeamDO module: mcp (WS11). The team MCP server registry (with the stored
// OAuth tokens of registered servers, refresh serialized here) and the state
// of the Factory MCP authorization server (mcpauth). Network calls (OAuth
// discovery, token requests, the proxy) happen in the Worker; this module
// only stores and decides. Migrations use the range 200-299.
import { BUILTIN_MCP_PREFIX, MCP_OAUTH_ERROR, OfError, newId, type MCPServer } from "@openfactory/core";
import type { CompiledDefinition } from "@openfactory/definition";
import { registerPreflight, type DefinitionCheck } from "./factories";
import { inUseError, teamResourceUses } from "./settings";
import { actorId, roleRank } from "../../auth/roles";
import * as authStore from "../../mcp/auth/store";
import * as reg from "../../mcp/registry/store";
import type { Migration } from "../../lib/sql/migrate";
import { listResponse } from "../../lib/page";
import { all, first } from "../../lib/sql/query";
import { noContent, type OpCall, type TeamKit, type TeamOp } from "./index";
import { MCP_STATE_TYPE, MCP_TOKEN_TYPE } from "../../mcp/envelope";
import { b64, fromB64 } from "../../lib/bytes";
import { registerRewrapSource, type RewrapSource } from "./secrets";

export const migrations: readonly Migration[] = [
  {
    version: 200,
    name: "mcp",
    sql: `
CREATE TABLE mcp_servers (id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, created_ms INTEGER NOT NULL, data TEXT NOT NULL);
CREATE TABLE mcp_oauth_tokens (server_id TEXT PRIMARY KEY, envelope TEXT NOT NULL, updated_ms INTEGER NOT NULL,
  lease TEXT NOT NULL DEFAULT '', lease_until_ms INTEGER NOT NULL DEFAULT 0);
CREATE TABLE mcp_oauth_states (state TEXT PRIMARY KEY, envelope TEXT NOT NULL, expires_ms INTEGER NOT NULL);
CREATE TABLE mcp_connections (id TEXT PRIMARY KEY, provider TEXT NOT NULL, created_ms INTEGER NOT NULL);
CREATE TABLE mcpauth_clients (id TEXT PRIMARY KEY, created_ms INTEGER NOT NULL, data TEXT NOT NULL);
CREATE TABLE mcpauth_requests (id TEXT PRIMARY KEY, expires_ms INTEGER NOT NULL, data TEXT NOT NULL);
CREATE TABLE mcpauth_codes (hash TEXT PRIMARY KEY, expires_ms INTEGER NOT NULL, data TEXT NOT NULL);
CREATE TABLE mcpauth_grants (id TEXT PRIMARY KEY, user_id TEXT NOT NULL, access_hash TEXT NOT NULL, refresh_hash TEXT NOT NULL,
  created_ms INTEGER NOT NULL, expires_ms INTEGER NOT NULL, revoked INTEGER NOT NULL DEFAULT 0, data TEXT NOT NULL);
CREATE INDEX mcpauth_grants_access ON mcpauth_grants (access_hash);
CREATE INDEX mcpauth_grants_refresh ON mcpauth_grants (refresh_hash);
CREATE INDEX mcpauth_grants_user ON mcpauth_grants (user_id);
`,
  },
];

/** How long a refresh lease holds (the Worker's token request has a 30 s timeout). */
export const REFRESH_LEASE_MS = 45_000;
const STATE_TTL_MS = 10 * 60 * 1000;

type JsonObject = Record<string, unknown>;

const obj = (v: unknown): JsonObject => (typeof v === "object" && v !== null && !Array.isArray(v) ? (v as JsonObject) : {});
const s = (o: JsonObject, k: string): string => (typeof o[k] === "string" ? (o[k] as string) : "");
/** Clock of internal ops: the Worker passes `now` (tests move it); else Date.now(). */
const nowOf = (o: JsonObject): number => (typeof o.now === "number" ? o.now : Date.now());

/**
 * Providers of the team's integration records, for the `configured` flag of
 * the built-in servers. Reads the integrations module's table when it exists.
 */
function integrationProviders(kit: TeamKit): string[] {
  try {
    return all<{ provider: string }>(kit.sql, "SELECT provider FROM integrations").map((r) => String(r.provider));
  } catch {
    return [];
  }
}

function signedInUser(call: OpCall, what: string): string {
  const p = call.principal();
  if (p.kind !== "user" && p.kind !== "factory_mcp") throw new OfError("not_authorized", `${what} needs a signed-in user`);
  return p.userId ?? p.id;
}

// ---------- REST ops (TeamDO.api, keyed by operationId) ----------

const restOps: Record<string, TeamOp> = {
  listMCPServers: (kit) =>
    listResponse(reg.listServers(kit), "", { builtin: reg.builtinList(integrationProviders(kit)) }),

  createMCPServer: (kit, call) => {
    return { $status: 201, body: reg.createServer(kit, call.body, actorId(call.principal()), Date.now()) };
  },

  getMCPServer: (kit, call) => reg.out(reg.serverForRef(kit, call.params.id ?? "")),

  updateMCPServer: (kit, call) => reg.updateServer(kit, call.params.id ?? "", call.body, Date.now()),

  deleteMCPServer: (kit, call) => {
    kit.tx(() => {
      const m = reg.serverForRef(kit, call.params.id ?? "");
      // In-use check against the factories' compiled definitions (see mcpServerUses).
      const err = inUseError("MCP server", m.name, mcpServerUses(kit, m.id, m.name));
      if (err) throw err;
      reg.deleteServer(kit, m.id);
    });
    return noContent();
  },

  /** Read phase of POST /mcp-servers/{id}/oauth/connect; the Worker runs the discovery. */
  connectMCPServer: (kit, call) => {
    const userId = signedInUser(call, "connecting an MCP server");
    const m = reg.serverForRef(kit, call.params.id ?? "");
    if (!m.oauth) throw new OfError("invalid_request", `MCP server ${m.name} does not use OAuth`);
    return { server: m, userId };
  },

  disconnectMCPServer: (kit, call) =>
    kit.tx(() => {
      const m = reg.serverForRef(kit, call.params.id ?? "");
      return reg.out(reg.disconnect(kit, m.id, Date.now()) ?? m);
    }),

  /** Read phase of the OAuth callback: takes the state (single use). The Worker finishes. */
  mcpOAuthCallback: (kit, call) => {
    const userId = signedInUser(call, "the OAuth callback");
    const state = call.query.state ?? "";
    if (state === "") return { envelope: "", userId };
    return kit.tx(() => {
      const r = first<{ envelope: string; expires_ms: number }>(
        kit.sql,
        "SELECT envelope, expires_ms FROM mcp_oauth_states WHERE state = ?",
        state,
      );
      kit.sql.exec("DELETE FROM mcp_oauth_states WHERE state = ?", state);
      return { envelope: r ? r.envelope : "", userId };
    });
  },

  listMCPSignIns: (kit, call) => {
    const p = call.principal();
    const now = Date.now();
    if (call.query.scope === "team") {
      if (roleRank(p.role) < roleRank("admin")) {
        throw new OfError("not_authorized", "only admins can list the MCP sign-ins of the team");
      }
      return listResponse(authStore.listGrants(kit, "", now), "");
    }
    return listResponse(authStore.listGrants(kit, p.userId ?? p.id, now), "");
  },

  revokeMCPSignIn: (kit, call) => {
    const p = call.principal();
    const id = call.params.id ?? "";
    const g = authStore.getGrant(kit, id);
    if (!g || g.team_id !== kit.teamId()) throw new OfError("resource_not_found", `MCP sign-in ${id} not found`);
    if (g.user_id !== (p.userId ?? p.id) && roleRank(p.role) < roleRank("admin")) {
      throw new OfError("not_authorized", "only admins can end the MCP sign-ins of other users");
    }
    authStore.revokeGrant(kit, id, Date.now());
    return noContent();
  },
};

// ---------- internal ops (Worker -> TeamDO.api; not routes) ----------

const internalOps: Record<string, TeamOp> = {
  // Registry OAuth (src/mcp/registry/oauth.ts).
  "mcp.oauth.saveState": (kit, call) => {
    const o = obj(call.body);
    const now = nowOf(o);
    kit.tx(() => {
      kit.sql.exec("DELETE FROM mcp_oauth_states WHERE expires_ms <= ?", now);
      kit.sql.exec(
        "INSERT INTO mcp_oauth_states (state, envelope, expires_ms) VALUES (?, ?, ?)",
        s(o, "state"),
        s(o, "envelope"),
        now + STATE_TTL_MS,
      );
    });
    return { ok: true };
  },

  "mcp.oauth.complete": (kit, call) => {
    const o = obj(call.body);
    const now = nowOf(o);
    return kit.tx(() => {
      const m = reg.getServer(kit, s(o, "serverId"));
      if (!m) throw new OfError("resource_not_found", "MCP server no longer exists");
      if (!m.oauth) throw new OfError("invalid_request", `MCP server "${m.name}" no longer uses OAuth`);
      kit.sql.exec(
        `INSERT INTO mcp_oauth_tokens (server_id, envelope, updated_ms) VALUES (?, ?, ?)
         ON CONFLICT(server_id) DO UPDATE SET envelope = excluded.envelope, updated_ms = excluded.updated_ms, lease = '', lease_until_ms = 0`,
        m.id,
        s(o, "envelope"),
        now,
      );
      return reg.markConnected(kit, m.id, s(o, "userId"), now);
    });
  },

  "mcp.oauth.setStatus": (kit, call) => {
    const o = obj(call.body);
    return kit.tx(() => reg.setStatus(kit, s(o, "serverId"), s(o, "status"), s(o, "message"), nowOf(o)) ?? null);
  },

  "mcp.server.get": (kit, call) => reg.getServer(kit, s(obj(call.body), "serverId")) ?? null,

  /** The stored token envelope of a server ("" when none). */
  "mcp.oauth.token": (kit, call) => {
    const r = first<{ envelope: string }>(
      kit.sql,
      "SELECT envelope FROM mcp_oauth_tokens WHERE server_id = ?",
      s(obj(call.body), "serverId"),
    );
    return { envelope: r?.envelope ?? "" };
  },

  /**
   * Single-flight refresh (appendix 04 gap 19). Grants a lease to one caller;
   * others get "busy" and read the token again after a short wait.
   */
  "mcp.oauth.refreshBegin": (kit, call) => {
    const o = obj(call.body);
    const now = nowOf(o);
    return kit.tx(() => {
      const r = first<{ envelope: string; lease: string; lease_until_ms: number }>(
        kit.sql,
        "SELECT envelope, lease, lease_until_ms FROM mcp_oauth_tokens WHERE server_id = ?",
        s(o, "serverId"),
      );
      if (!r) return { status: "missing", envelope: "" };
      if (r.lease !== "" && r.lease_until_ms > now) return { status: "busy", envelope: r.envelope };
      const lease = newId("lease");
      kit.sql.exec(
        "UPDATE mcp_oauth_tokens SET lease = ?, lease_until_ms = ? WHERE server_id = ?",
        lease,
        now + REFRESH_LEASE_MS,
        s(o, "serverId"),
      );
      return { status: "lease", lease, envelope: r.envelope };
    });
  },

  /** Ends a lease: stores the refreshed envelope, or records the failure. */
  "mcp.oauth.refreshEnd": (kit, call) => {
    const o = obj(call.body);
    const now = nowOf(o);
    return kit.tx(() => {
      const id = s(o, "serverId");
      const r = first<{ lease: string }>(kit.sql, "SELECT lease FROM mcp_oauth_tokens WHERE server_id = ?", id);
      if (!r || r.lease !== s(o, "lease")) return { ok: false };
      const env = s(o, "envelope");
      if (env !== "") {
        kit.sql.exec(
          "UPDATE mcp_oauth_tokens SET envelope = ?, updated_ms = ?, lease = '', lease_until_ms = 0 WHERE server_id = ?",
          env,
          now,
          id,
        );
      } else {
        kit.sql.exec("UPDATE mcp_oauth_tokens SET lease = '', lease_until_ms = 0 WHERE server_id = ?", id);
      }
      if (s(o, "error") !== "") reg.setStatus(kit, id, MCP_OAUTH_ERROR, s(o, "error"), now);
      return { ok: true };
    });
  },

  /** Resolve (planner, WS5): managedId -> transport. Body {run: {id, mcpServers}, managedId}. */
  "mcp.resolve": (kit, call) => {
    const o = obj(call.body);
    const run = obj(o.run);
    return reg.resolveManaged(
      kit,
      kit.env.OF_PUBLIC_URL ?? "",
      { id: s(run, "id"), mcpServers: obj(run.mcpServers) as Record<string, MCPServer> },
      s(o, "managedId"),
    );
  },

  /** Registered names and IDs, for the managedId preflight of a definition. */
  "mcp.knownRefs": (kit) => [...reg.knownRefs(kit)].sort(),

  // Factory MCP start_connection / get_connection_status.
  "mcp.connection.start": (kit, call) => {
    const o = obj(call.body);
    const id = newId("conn");
    kit.tx(() => {
      kit.sql.exec("INSERT INTO mcp_connections (id, provider, created_ms) VALUES (?, ?, ?)", id, s(o, "provider"), nowOf(o));
    });
    return { id };
  },
  "mcp.connection.get": (kit, call) => {
    const r = first<{ provider: string }>(kit.sql, "SELECT provider FROM mcp_connections WHERE id = ?", s(obj(call.body), "id"));
    return r ? { provider: r.provider } : null;
  },

  // mcpauth (src/mcp/auth/handlers.ts).
  "mcpauth.register": (kit, call) => {
    const o = obj(call.body);
    return authStore.register(kit, o.metadata, nowOf(o));
  },
  "mcpauth.authorize": (kit, call) => {
    const o = obj(call.body);
    return authStore.authorize(kit, {
      publicURL: s(o, "publicURL"),
      query: obj(o.query) as Record<string, string>,
      rawQuery: s(o, "rawQuery"),
      userId: s(o, "userId"),
      now: nowOf(o),
    });
  },
  "mcpauth.consent": (kit, call) => {
    const o = obj(call.body);
    return authStore.consent(kit, {
      publicURL: s(o, "publicURL"),
      requestId: s(o, "requestId"),
      decision: s(o, "decision"),
      userId: s(o, "userId"),
      now: nowOf(o),
    });
  },
  "mcpauth.token": (kit, call) => {
    const o = obj(call.body);
    return authStore.token(kit, obj(o.form) as Record<string, string>, nowOf(o));
  },
  "mcpauth.revoke": async (kit, call) => {
    const o = obj(call.body);
    await authStore.revokeToken(kit, s(o, "token"), nowOf(o));
    return { ok: true };
  },
  "mcpauth.verify": (kit, call) => {
    const o = obj(call.body);
    return authStore.verify(kit, s(o, "accessHash"), nowOf(o));
  },
  "mcpauth.grants": (kit, call) => {
    const o = obj(call.body);
    return authStore.listGrants(kit, s(o, "userId"), nowOf(o));
  },
  "mcpauth.revokeGrant": (kit, call) => {
    const o = obj(call.body);
    return { ok: authStore.revokeGrant(kit, s(o, "id"), nowOf(o)) };
  },
};

export const ops: Record<string, TeamOp> = { ...restOps, ...internalOps };

export function setup(_kit: TeamKit): void {
  registerPreflight("mcp servers", preflightMCPServers);
  registerRewrapSource(mcpEnvelopes("mcp_oauth_tokens", "server_id", (id) => `mcp-oauth:${id}`, MCP_TOKEN_TYPE));
  registerRewrapSource(mcpEnvelopes("mcp_oauth_states", "state", (id) => `mcp-oauth-state:${id}`, MCP_STATE_TYPE));
}

/**
 * The MCP OAuth token and state envelopes (base64 text, src/mcp/envelope.ts):
 * the same format as secret envelopes, so POST /secrets/rewrap includes them.
 */
function mcpEnvelopes(table: string, keyCol: string, nameOf: (key: string) => string, type: string): RewrapSource {
  const sameBytes = (a: Uint8Array, b: Uint8Array) => a.length === b.length && a.every((x, i) => x === b[i]);
  return {
    name: table,
    list: (k) =>
      all<{ key: string; envelope: string }>(k.sql, `SELECT ${keyCol} AS key, envelope FROM ${table} WHERE envelope != ''`).map((r) => ({
        key: r.key,
        id: { teamId: k.teamId(), factoryId: "", name: nameOf(r.key), type },
        envelope: fromB64(r.envelope),
      })),
    replace: (k, key, old, next) => {
      const cur = first<{ envelope: string }>(k.sql, `SELECT envelope FROM ${table} WHERE ${keyCol} = ?`, key);
      if (!cur || !sameBytes(fromB64(cur.envelope), old)) return false;
      k.sql.exec(`UPDATE ${table} SET envelope = ? WHERE ${keyCol} = ?`, b64(next), key);
      return true;
    },
  };
}

type ServerMap = Record<string, MCPServer> | undefined;

/** Port of engine/preflight.go preflightMCPServers: managedId references must name a registered server. */
function preflightMCPServers(kit: TeamKit, _f: unknown, def: CompiledDefinition, _files: Record<string, string>, chk: DefinitionCheck): void {
  const refs: { file: string; name: string; id: string }[] = [];
  const add = (file: string, m: ServerMap) => {
    for (const n of Object.keys(m ?? {}).sort()) {
      const id = m![n]!.managedId ?? "";
      if (id !== "" && !id.startsWith(BUILTIN_MCP_PREFIX)) refs.push({ file, name: n, id });
    }
  };
  add("factory.yaml", def.mcpServers);
  for (const a of def.agents ?? []) add(`agents/${a.name}/agent.md`, (a as { mcpServers?: ServerMap }).mcpServers);
  for (const au of def.automations ?? []) add(`automations/${au.name}/automation.md`, (au.overrides as { mcpServers?: ServerMap } | undefined)?.mcpServers);
  for (const sc of def.scorers ?? []) add(`scorers/${(sc as { slug: string }).slug}/scorer.md`, (sc as { mcpServers?: ServerMap }).mcpServers);
  if (def.scorerDefaults) add("factory.yaml", (def.scorerDefaults as { mcpServers?: ServerMap }).mcpServers);
  if (refs.length === 0) return;
  const known = reg.knownRefs(kit);
  const seen = new Set<string>();
  for (const r of refs) {
    // Factory-wide servers also appear on each agent: report one time.
    const key = r.name + "/" + r.id;
    if (known.has(r.id) || seen.has(key)) continue;
    seen.add(key);
    chk.errors.push({
      file: r.file,
      field: `mcpServers.${r.name}.managedId`,
      message: `no MCP server "${r.id}" is registered for this team (register it under MCP servers first)`,
    } as DefinitionCheck["errors"][number]);
  }
}

/**
 * Where the factories' active definitions reference an MCP server (by id or
 * name), through the settings module's teamResourceUses.
 */
export function mcpServerUses(kit: TeamKit, id: string, name: string): string[] {
  const refs = [id, name];
  const selects = (m: ServerMap) => Object.values(m ?? {}).some((srv) => !!srv.managedId && refs.includes(srv.managedId));
  return teamResourceUses(kit, (agents, automations) => {
    const out: string[] = [];
    for (const a of agents) if (selects((a as { mcpServers?: ServerMap }).mcpServers)) out.push("agent " + a.name);
    for (const au of automations) {
      if (selects((au.overrides as { mcpServers?: ServerMap } | undefined)?.mcpServers)) out.push("automation " + au.name);
    }
    return out;
  });
}

// ---------- typed helpers for other TeamDO modules ----------

/** Resolve for the planner inside TeamDO (configSnapshot / session start). */
export function resolveManaged(kit: TeamKit, run: reg.RunRef, managedId: string): MCPServer {
  return reg.resolveManaged(kit, kit.env.OF_PUBLIC_URL ?? "", run, managedId);
}

/** Registered names and IDs (managedId preflight of definition apply). */
export function knownMcpServerRefs(kit: TeamKit): Set<string> {
  return reg.knownRefs(kit);
}
