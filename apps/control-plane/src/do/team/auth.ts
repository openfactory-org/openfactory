// TeamDO module: auth (WS4). Team record, users, external identities, API
// keys and service accounts. Ports internal/api/platform.go (me, teams,
// users, API keys, bootstrap), internal/api/serviceaccounts.go, the session
// and API-key checks of internal/auth/service.go and internal/api/auth.go,
// and user provisioning of auth.Service.provision.
import { OfError, newId } from "@openfactory/core";
import type { ApiKeyLookup, Principal, Role } from "../../contracts/do-messages";
import { SERVICE_ACCOUNT_KEY_PREFIX, generateKey, KEY_PREFIX } from "../../auth/apikeys";
import { effectiveRole, normalizeRole, roleRank, validRole } from "../../auth/roles";
import { bodyObject, optBool, optNullableStringMap, optString, str, type JsonObject } from "../../lib/body";
import type { Migration } from "../../lib/sql/migrate";
import { all, first } from "../../lib/sql/query";
import { listResponse, msOf } from "../../lib/page";
import { findFactory } from "./factories";
import { created, noContent, type OpCall, type TeamKit, type TeamOp } from "./index";

export const migrations: readonly Migration[] = [
  {
    version: 1,
    name: "auth",
    sql: `
CREATE TABLE team (k INTEGER PRIMARY KEY CHECK (k = 1), id TEXT NOT NULL, name TEXT NOT NULL, created_ms INTEGER NOT NULL, data TEXT NOT NULL);
CREATE TABLE users (id TEXT PRIMARY KEY, email TEXT NOT NULL DEFAULT '', kind TEXT NOT NULL DEFAULT '',
  role TEXT NOT NULL DEFAULT '', name TEXT NOT NULL DEFAULT '', created_ms INTEGER NOT NULL, data TEXT NOT NULL);
CREATE UNIQUE INDEX users_email ON users (email) WHERE email <> '';
CREATE INDEX users_created ON users (created_ms, id);
CREATE TABLE external_identities (provider TEXT NOT NULL, external_id TEXT NOT NULL, user_id TEXT NOT NULL DEFAULT '',
  data TEXT NOT NULL, PRIMARY KEY (provider, external_id));
CREATE INDEX external_identities_user ON external_identities (user_id);
CREATE TABLE api_keys (id TEXT PRIMARY KEY, user_id TEXT NOT NULL DEFAULT '', prefix TEXT NOT NULL UNIQUE,
  hashed_key TEXT NOT NULL UNIQUE, kind TEXT NOT NULL DEFAULT 'of_', last_used_ms INTEGER NOT NULL DEFAULT 0,
  created_ms INTEGER NOT NULL, data TEXT NOT NULL);
CREATE INDEX api_keys_user ON api_keys (user_id);
CREATE INDEX api_keys_created ON api_keys (created_ms, id);
`,
  },
];

export const USER_KIND_SERVICE_ACCOUNT = "service_account";
export const SERVICE_ACCOUNT_ID_PREFIX = "sa";
const TOUCH_RESOLUTION_MS = 60_000;

// ---------- records ----------

export interface Team {
  id: string;
  name: string;
  created_at: string;
}

export interface User {
  id: string;
  team_id: string;
  email: string;
  name: string;
  role: string;
  factory_roles?: Record<string, string>;
  kind?: string;
  created_at: string;
}

export interface ApiKey {
  id: string;
  team_id: string;
  user_id: string;
  name: string;
  prefix: string;
  created_at: string;
  last_used_at?: string;
}

const isServiceAccount = (u: User) => u.kind === USER_KIND_SERVICE_ACCOUNT;

export function getTeamRecord(kit: TeamKit): Team | undefined {
  const r = first<{ data: string }>(kit.sql, "SELECT data FROM team WHERE k = 1");
  return r ? (JSON.parse(r.data) as Team) : undefined;
}

function putTeam(kit: TeamKit, t: Team): void {
  kit.sql.exec(
    "INSERT INTO team (k, id, name, created_ms, data) VALUES (1, ?, ?, ?, ?) ON CONFLICT(k) DO UPDATE SET name = excluded.name, data = excluded.data",
    t.id,
    t.name,
    msOf(t.created_at),
    JSON.stringify(t),
  );
}

export function getUser(kit: TeamKit, id: string): User | undefined {
  const r = first<{ data: string }>(kit.sql, "SELECT data FROM users WHERE id = ?", id);
  return r ? (JSON.parse(r.data) as User) : undefined;
}

function getUserByEmail(kit: TeamKit, email: string): User | undefined {
  const e = email.trim().toLowerCase();
  if (!e) return undefined;
  const r = first<{ data: string }>(kit.sql, "SELECT data FROM users WHERE email = ?", e);
  return r ? (JSON.parse(r.data) as User) : undefined;
}

export function listUsers(kit: TeamKit): User[] {
  return all<{ data: string }>(kit.sql, "SELECT data FROM users ORDER BY created_ms, id").map(
    (r) => JSON.parse(r.data) as User,
  );
}

function cleanUser(u: User): User {
  const out: Partial<User> = { id: u.id, team_id: u.team_id, email: u.email, name: u.name, role: u.role };
  if (u.factory_roles && Object.keys(u.factory_roles).length > 0) out.factory_roles = u.factory_roles;
  if (u.kind) out.kind = u.kind;
  out.created_at = u.created_at;
  return out as User;
}

function insertUser(kit: TeamKit, u: User): void {
  const email = u.email.trim().toLowerCase();
  if (email && first(kit.sql, "SELECT 1 FROM users WHERE email = ?", email)) {
    throw new OfError("conflict", `a user with email ${email} already exists`);
  }
  const c = cleanUser(u);
  kit.sql.exec(
    "INSERT INTO users (id, email, kind, role, name, created_ms, data) VALUES (?, ?, ?, ?, ?, ?, ?)",
    c.id,
    email,
    c.kind ?? "",
    c.role,
    c.name,
    msOf(c.created_at) || Date.now(),
    JSON.stringify(c),
  );
}

function updateUserRow(kit: TeamKit, u: User): User {
  const cur = getUser(kit, u.id);
  if (!cur) throw new OfError("resource_not_found", `user ${u.id} not found`);
  const c = cleanUser({ ...u, created_at: cur.created_at });
  kit.sql.exec(
    "UPDATE users SET email = ?, kind = ?, role = ?, name = ?, data = ? WHERE id = ?",
    c.email.trim().toLowerCase(),
    c.kind ?? "",
    c.role,
    c.name,
    JSON.stringify(c),
    c.id,
  );
  return c;
}

function keyRow(r: { data: string; last_used_ms: number }): ApiKey {
  const k = JSON.parse(r.data) as ApiKey;
  if (r.last_used_ms > 0) k.last_used_at = new Date(r.last_used_ms).toISOString();
  return k;
}

export function listApiKeys(kit: TeamKit): ApiKey[] {
  return all<{ data: string; last_used_ms: number }>(
    kit.sql,
    "SELECT data, last_used_ms FROM api_keys ORDER BY created_ms, id",
  ).map(keyRow);
}

function getApiKey(kit: TeamKit, id: string): ApiKey | undefined {
  const r = first<{ data: string; last_used_ms: number }>(
    kit.sql,
    "SELECT data, last_used_ms FROM api_keys WHERE id = ?",
    id,
  );
  return r ? keyRow(r) : undefined;
}

/** NewAPIKey / NewServiceAccountKey + CreateAPIKey. Call inside tx. */
async function mintKey(kind: typeof KEY_PREFIX | typeof SERVICE_ACCOUNT_KEY_PREFIX) {
  return generateKey(kind);
}

function insertKey(
  kit: TeamKit,
  g: { key: string; prefix: string; hash: string },
  kind: string,
  userId: string,
  name: string,
): ApiKey {
  const k: ApiKey = {
    id: newId("key"),
    team_id: kit.teamId(),
    user_id: userId,
    name,
    prefix: g.prefix,
    created_at: new Date().toISOString(),
  };
  kit.sql.exec(
    "INSERT INTO api_keys (id, user_id, prefix, hashed_key, kind, created_ms, data) VALUES (?, ?, ?, ?, ?, ?, ?)",
    k.id,
    userId,
    g.prefix,
    g.hash,
    kind,
    msOf(k.created_at),
    JSON.stringify(k),
  );
  return k;
}

// ---------- principals ----------

function validFactoryRoles(u: User): Record<string, Role> {
  const out: Record<string, Role> = {};
  for (const [fid, r] of Object.entries(u.factory_roles ?? {})) if (validRole(r)) out[fid] = normalizeRole(r) as Role;
  return out;
}

export function principalOf(kit: TeamKit, u: User, keyId?: string): Principal {
  return {
    kind: isServiceAccount(u) ? "service_account" : "user",
    id: u.id,
    teamId: kit.teamId(),
    role: effectiveRole(u.role),
    factoryRoles: validFactoryRoles(u),
    userId: u.id,
    ...(keyId ? { keyId } : {}),
  };
}

/** TeamRpc.authLookupApiKey: key by SHA-256 hash, its user, and a touch once a minute. */
export async function lookupApiKey(kit: TeamKit, keyHash: string): Promise<ApiKeyLookup | null> {
  return kit.tx(() => {
    const r = first<{ id: string; user_id: string; kind: string; last_used_ms: number }>(
      kit.sql,
      "SELECT id, user_id, kind, last_used_ms FROM api_keys WHERE hashed_key = ?",
      keyHash,
    );
    if (!r) return null;
    const u = getUser(kit, r.user_id);
    if (!u) return null;
    // A user key never authenticates a service account, and the reverse.
    if ((r.kind === SERVICE_ACCOUNT_KEY_PREFIX) !== isServiceAccount(u)) return null;
    const now = Date.now();
    if (now - r.last_used_ms >= TOUCH_RESOLUTION_MS) {
      kit.sql.exec("UPDATE api_keys SET last_used_ms = ? WHERE id = ?", now, r.id);
    }
    return { keyId: r.id, principal: principalOf(kit, u, r.id) };
  });
}

/** Claims of the of_session cookie (auth.sessionClaims). */
export interface SessionClaims {
  u: string;
  t: string;
  p: string;
  k?: string;
  kp?: string;
  iat: number;
}

/**
 * TeamRpc.authSession. `sessionId` is the JSON of the cookie claims. Null
 * for a deleted user, another team, or a deleted key (sessions end with the
 * key that created them).
 */
export async function sessionPrincipal(kit: TeamKit, sessionId: string): Promise<Principal | null> {
  let c: SessionClaims;
  try {
    c = JSON.parse(sessionId) as SessionClaims;
  } catch {
    return null;
  }
  if (typeof c?.u !== "string") return null;
  const u = getUser(kit, c.u);
  if (!u || c.t !== kit.teamId()) return null;
  if (c.k) {
    const k = first<{ user_id: string }>(kit.sql, "SELECT user_id FROM api_keys WHERE id = ?", c.k);
    if (!k || k.user_id !== u.id) return null;
  }
  return principalOf(kit, u, c.k);
}

// ---------- provisioning (auth.Service.provision) ----------

export interface Identity {
  provider: string;
  subject: string;
  email: string;
  emailVerified: boolean;
  name?: string;
  handle?: string;
  groups?: string[];
  role?: string;
  roleManaged?: boolean;
  defaultRole?: string;
}

export class LoginError extends OfError {
  constructor(
    readonly loginCode: string,
    detail: string,
  ) {
    super("invalid_request", detail, { reason: loginCode });
  }
}

function displayName(id: Identity): string {
  return id.name || id.handle || id.email;
}

/** Finds or creates the user of an identity and links it. Single team: the team is created when missing. */
export function provision(kit: TeamKit, id: Identity): User {
  if (!id.provider || !id.subject) throw new OfError("invalid_request", "auth: identity without provider or subject");
  return kit.tx(() => {
    if (!getTeamRecord(kit)) putTeam(kit, { id: kit.teamId(), name: "Default", created_at: new Date().toISOString() });
    let u: User | undefined;
    const x = first<{ user_id: string }>(
      kit.sql,
      "SELECT user_id FROM external_identities WHERE provider = ? AND external_id = ?",
      id.provider,
      id.subject,
    );
    if (x) u = getUser(kit, x.user_id);
    if (!u && id.email) {
      const existing = getUserByEmail(kit, id.email);
      if (existing) {
        if (!id.emailVerified) {
          throw new OfError("invalid_request", `email ${id.email} is not verified by the identity provider`, {
            reason: "email_not_verified",
          });
        }
        u = existing;
      }
    }
    if (!u) {
      u = {
        id: newId("usr"),
        team_id: kit.teamId(),
        email: id.email,
        name: displayName(id),
        role: effectiveRole(id.roleManaged ? id.role : id.defaultRole),
        created_at: new Date().toISOString(),
      };
      insertUser(kit, u);
    } else {
      let changed = false;
      const n = displayName(id);
      if (n && n !== u.name) {
        u.name = n;
        changed = true;
      }
      if (id.roleManaged && validRole(id.role) && id.role !== u.role) {
        u.role = id.role!;
        changed = true;
      }
      if (changed) u = updateUserRow(kit, u);
    }
    const link = { user_id: u.id, provider: id.provider, external_id: id.subject, handle: id.handle || id.email };
    kit.sql.exec(
      `INSERT INTO external_identities (provider, external_id, user_id, data) VALUES (?, ?, ?, ?)
       ON CONFLICT(provider, external_id) DO UPDATE SET user_id = excluded.user_id, data = excluded.data`,
      id.provider,
      id.subject,
      u.id,
      JSON.stringify(link),
    );
    return u;
  });
}

// ---------- validation helpers ----------

/** normalizeEmail: one bare address, lower-cased. */
export function normalizeEmail(e: string): string {
  const t = e.trim();
  // net/mail.ParseAddress accepts more, but the API also requires a.Address == input (a bare address).
  if (!/^[^\s@<>()[\]\\,;:"]+@[^\s@<>()[\]\\,;:"]+$/.test(t) || t !== e.trim()) {
    throw new OfError("invalid_request", `invalid email address ${JSON.stringify(e)}`);
  }
  return t.toLowerCase();
}

function validServiceAccountName(n: string): void {
  if (n === "" || n.length > 64 || /[ \t\n/:]/.test(n)) {
    throw new OfError("invalid_request", "name must be 1-64 characters without spaces, '/' or ':'");
  }
}

function applyFactoryRoles(kit: TeamKit, u: User, input: Record<string, string | null> | undefined): void {
  for (const [fid, role] of Object.entries(input ?? {})) {
    const f = findFactory(kit, fid);
    if (!f || f.id !== fid) throw new OfError("invalid_request", `factory_roles: factory ${fid} does not exist`);
    if (role === null) {
      if (u.factory_roles) delete u.factory_roles[fid];
      continue;
    }
    if (!validRole(role))
      throw new OfError("invalid_request", `factory_roles.${fid}: role must be admin, editor or user`);
    u.factory_roles ??= {};
    u.factory_roles[fid] = normalizeRole(role);
  }
  if (u.factory_roles && Object.keys(u.factory_roles).length === 0) delete u.factory_roles;
}

function userOf(kit: TeamKit, id: string): User {
  const u = getUser(kit, id);
  if (!u) throw new OfError("resource_not_found", `user ${id} not found`);
  return u;
}

function findServiceAccount(kit: TeamKit, ref: string): User | undefined {
  const u = getUser(kit, ref);
  if (u && isServiceAccount(u)) return u;
  return listUsers(kit).find((x) => isServiceAccount(x) && x.name === ref);
}

function serviceAccountOf(kit: TeamKit, ref: string): User {
  const u = findServiceAccount(kit, ref);
  if (!u) throw new OfError("resource_not_found", `service account ${ref} not found`);
  return u;
}

function viewServiceAccount(u: User) {
  return {
    id: u.id,
    team_id: u.team_id,
    name: u.name,
    role: normalizeRole(u.role),
    ...(u.factory_roles && Object.keys(u.factory_roles).length ? { factory_roles: u.factory_roles } : {}),
    created_at: u.created_at,
  };
}

const isAdmin = (c: OpCall) => roleRank(c.principal().role) >= roleRank("admin");

// ---------- ops ----------

export const ops: Record<string, TeamOp> = {
  getMe: (kit, c) => {
    const p = c.principal();
    const team = getTeamRecord(kit);
    if (!team) throw new OfError("resource_not_found", `team ${kit.teamId()} not found`);
    const u = p.userId ? getUser(kit, p.userId) : undefined;
    const body: JsonObject = {
      user: u ?? null,
      team,
      role: normalizeRole(p.role),
      factory_roles: u ? validFactoryRoles(u) : p.factoryRoles,
    };
    if (p.keyId && c.meta.principal && c.query["auth"] === "api_key") {
      const k = getApiKey(kit, p.keyId);
      if (k) body["api_key"] = k;
    }
    return body;
  },

  listTeams: (kit) => {
    const team = getTeamRecord(kit);
    if (!team) throw new OfError("resource_not_found", `team ${kit.teamId()} not found`);
    return listResponse([team], "");
  },

  getTeam: (kit, c) => {
    const team = getTeamRecord(kit);
    if (!team || c.params["id"] !== team.id)
      throw new OfError("resource_not_found", `team ${c.params["id"]} not found`);
    return team;
  },

  listUsers: (kit) =>
    listResponse(
      listUsers(kit).filter((u) => !isServiceAccount(u)),
      "",
    ),

  getUser: (kit, c) => userOf(kit, c.params["id"]!),

  createUser: async (kit, c) => {
    const b = bodyObject(c.body);
    const email = normalizeEmail(str(b, "email"));
    let role = str(b, "role");
    if (role === "") role = "editor";
    if (!validRole(role)) throw new OfError("invalid_request", "role must be admin, editor or user");
    const withKey = optBool(b, "create_api_key") ?? false;
    const g = withKey ? await mintKey(KEY_PREFIX) : undefined;
    return kit.tx(() => {
      if (getUserByEmail(kit, email)) throw new OfError("conflict", `a user with email ${email} already exists`);
      const u: User = {
        id: newId("usr"),
        team_id: kit.teamId(),
        email,
        name: str(b, "name").trim() || email,
        role: normalizeRole(role),
        created_at: new Date().toISOString(),
      };
      insertUser(kit, u);
      const resp: JsonObject = { user: cleanUser(u) };
      if (g) {
        resp["api_key"] = insertKey(kit, g, KEY_PREFIX, u.id, "default");
        resp["key"] = g.key;
      }
      return created(resp);
    });
  },

  updateUser: (kit, c) => {
    const b = bodyObject(c.body);
    const name = optString(b, "name");
    const role = optString(b, "role");
    const fr = optNullableStringMap(b, "factory_roles");
    return kit.tx(() => {
      const u = userOf(kit, c.params["id"]!);
      if (name !== undefined) {
        const n = name.trim();
        if (n === "" || n.length > 200) throw new OfError("invalid_request", "name must be 1-200 characters");
        u.name = n;
      }
      if (role !== undefined) {
        if (!validRole(role)) throw new OfError("invalid_request", "role must be admin, editor or user");
        const r = normalizeRole(role);
        if (u.role === "admin" && r !== "admin") {
          const admins = listUsers(kit).filter((x) => x.role === "admin" && !isServiceAccount(x)).length;
          if (admins <= 1) throw new OfError("conflict", "cannot demote the team's last admin");
        }
        u.role = r;
      }
      applyFactoryRoles(kit, u, fr);
      return updateUserRow(kit, u);
    });
  },

  listAPIKeys: (kit, c) => {
    const p = c.principal();
    let ks = listApiKeys(kit);
    if (!isAdmin(c)) ks = ks.filter((k) => k.user_id === (p.userId ?? ""));
    return listResponse(ks, "");
  },

  createAPIKey: async (kit, c) => {
    const p = c.principal();
    const b = bodyObject(c.body);
    const name = str(b, "name").trim();
    if (name === "" || name.length > 100) throw new OfError("invalid_request", "name must be 1-100 characters");
    const reqUser = str(b, "user_id");
    const g = await mintKey(KEY_PREFIX);
    return kit.tx(() => {
      let uid = p.userId ?? "";
      if (reqUser !== "" && reqUser !== uid) {
        if (!isAdmin(c)) throw new OfError("not_authorized", "only admins can create API keys for other users");
        const u = userOf(kit, reqUser);
        if (isServiceAccount(u)) {
          throw new OfError(
            "invalid_request",
            `user ${u.id} is a service account: use POST /service-accounts/{id}/keys`,
          );
        }
        uid = u.id;
      }
      if (p.kind === "service_account" && uid === p.userId) {
        throw new OfError(
          "not_authorized",
          "a service account cannot create its own API keys: an admin uses POST /service-accounts/{id}/keys",
        );
      }
      const k = insertKey(kit, g, KEY_PREFIX, uid, name);
      return created({ api_key: k, key: g.key });
    });
  },

  deleteAPIKey: (kit, c) =>
    kit.tx(() => {
      const p = c.principal();
      const id = c.params["id"]!;
      const k = getApiKey(kit, id);
      if (!k) throw new OfError("resource_not_found", `API key ${id} not found`);
      if (k.user_id !== (p.userId ?? "") && !isAdmin(c)) {
        throw new OfError("not_authorized", "only admins can revoke other users' API keys");
      }
      kit.sql.exec("DELETE FROM api_keys WHERE id = ?", id);
      return noContent();
    }),

  listServiceAccounts: (kit) => listResponse(listUsers(kit).filter(isServiceAccount).map(viewServiceAccount), ""),

  createServiceAccount: async (kit, c) => {
    const b = bodyObject(c.body);
    const name = str(b, "name").trim();
    validServiceAccountName(name);
    let role = str(b, "role");
    if (role === "") role = "user";
    if (!validRole(role)) throw new OfError("invalid_request", "role must be admin, editor or user");
    const g = optBool(b, "create_api_key") ? await mintKey(SERVICE_ACCOUNT_KEY_PREFIX) : undefined;
    return kit.tx(() => {
      if (findServiceAccount(kit, name))
        throw new OfError("conflict", `a service account named ${name} already exists`);
      const u: User = {
        id: newId(SERVICE_ACCOUNT_ID_PREFIX),
        team_id: kit.teamId(),
        email: "",
        name,
        role: normalizeRole(role),
        kind: USER_KIND_SERVICE_ACCOUNT,
        created_at: new Date().toISOString(),
      };
      insertUser(kit, u);
      const resp: JsonObject = { service_account: viewServiceAccount(u) };
      if (g) {
        resp["api_key"] = insertKey(kit, g, SERVICE_ACCOUNT_KEY_PREFIX, u.id, "default");
        resp["key"] = g.key;
      }
      return created(resp);
    });
  },

  getServiceAccount: (kit, c) => viewServiceAccount(serviceAccountOf(kit, c.params["id"]!)),

  updateServiceAccount: (kit, c) => {
    const b = bodyObject(c.body);
    const name = optString(b, "name");
    const role = optString(b, "role");
    const fr = optNullableStringMap(b, "factory_roles");
    return kit.tx(() => {
      const u = serviceAccountOf(kit, c.params["id"]!);
      if (name !== undefined) {
        const n = name.trim();
        validServiceAccountName(n);
        if (n !== u.name && findServiceAccount(kit, n))
          throw new OfError("conflict", `a service account named ${n} already exists`);
        u.name = n;
      }
      if (role !== undefined) {
        if (!validRole(role)) throw new OfError("invalid_request", "role must be admin, editor or user");
        u.role = normalizeRole(role);
      }
      applyFactoryRoles(kit, u, fr);
      return viewServiceAccount(updateUserRow(kit, u));
    });
  },

  deleteServiceAccount: (kit, c) =>
    kit.tx(() => {
      const u = serviceAccountOf(kit, c.params["id"]!);
      kit.sql.exec("DELETE FROM api_keys WHERE user_id = ?", u.id);
      kit.sql.exec("DELETE FROM users WHERE id = ?", u.id);
      return noContent();
    }),

  listServiceAccountKeys: (kit, c) => {
    const u = serviceAccountOf(kit, c.params["id"]!);
    return listResponse(
      listApiKeys(kit).filter((k) => k.user_id === u.id),
      "",
    );
  },

  createServiceAccountKey: async (kit, c) => {
    const b = bodyObject(c.body);
    const name = str(b, "name").trim();
    serviceAccountOf(kit, c.params["id"]!);
    if (name === "" || name.length > 100) throw new OfError("invalid_request", "name must be 1-100 characters");
    const g = await mintKey(SERVICE_ACCOUNT_KEY_PREFIX);
    return kit.tx(() => {
      const u = serviceAccountOf(kit, c.params["id"]!);
      return created({ api_key: insertKey(kit, g, SERVICE_ACCOUNT_KEY_PREFIX, u.id, name), key: g.key });
    });
  },

  deleteServiceAccountKey: (kit, c) =>
    kit.tx(() => {
      const u = serviceAccountOf(kit, c.params["id"]!);
      const id = c.params["keyId"]!;
      if (kit.sql.exec("DELETE FROM api_keys WHERE id = ? AND user_id = ?", id, u.id).rowsWritten === 0) {
        throw new OfError("resource_not_found", `API key ${id} not found`);
      }
      return noContent();
    }),

  /** POST /bootstrap: first team, admin and API key. TeamDO serializes it. */
  bootstrap: async (kit, c) => {
    const b = bodyObject(c.body);
    const teamName = str(b, "team_name").trim();
    const g = await mintKey(KEY_PREFIX);
    return kit.tx(() => {
      if (getTeamRecord(kit)) throw new OfError("conflict", "this server is already bootstrapped");
      if (teamName === "") throw new OfError("invalid_request", "team_name is required");
      const email = normalizeEmail(str(b, "admin_email"));
      const now = new Date().toISOString();
      const team: Team = { id: kit.teamId(), name: teamName, created_at: now };
      putTeam(kit, team);
      const u: User = {
        id: newId("usr"),
        team_id: team.id,
        email,
        name: str(b, "admin_name").trim() || email,
        role: "admin",
        created_at: now,
      };
      insertUser(kit, u);
      const k = insertKey(kit, g, KEY_PREFIX, u.id, str(b, "key_name") || "bootstrap");
      return created({ team, user: cleanUser(u), api_key: k, key: g.key });
    });
  },

  /** Internal: login provisioning (Worker after an OIDC callback). */
  authProvision: (kit, c) => {
    const id = c.body as Identity;
    const u = provision(kit, id);
    return { user: u, principal: principalOf(kit, u) };
  },

  /** Internal: the team record, or null before bootstrap. */
  teamGet: (kit) => getTeamRecord(kit) ?? null,

  /** Internal: the team record for /auth/session responses. */
  authMe: (kit, c) => {
    const p = c.principal();
    const u = p.userId ? getUser(kit, p.userId) : undefined;
    return { user: u ?? null, team: getTeamRecord(kit) ?? null, role: p.role };
  },
};

export function setup(_kit: TeamKit): void {}
