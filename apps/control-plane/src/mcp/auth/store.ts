// TeamDO side of the Factory MCP authorization server (internal/mcpauth
// Service). Async functions hash and generate tokens first, then do all
// reads and writes in one kit.tx (no await inside). The Go service kept this
// state in the KV space behind a process mutex; the TeamDO is single-threaded.
import { newId } from "@openfactory/core";
import type { Principal } from "../../contracts/do-messages";
import { USER_KIND_SERVICE_ACCOUNT, getUser, principalOf, type User } from "../../do/team/auth";
import type { TeamKit } from "../../do/team/index";
import { b64url, sha256, sha256Hex, timingSafeEqual } from "../../lib/bytes";
import { all, first } from "../../lib/sql/query";
import {
  ACCESS_TOKEN_PREFIX,
  ACCESS_TTL_MS,
  AUTHORIZE_PATH,
  CODE_TTL_MS,
  LOGIN_PATH,
  MAX_REDIRECT_URIS,
  REFRESH_TOKEN_PREFIX,
  REFRESH_TTL_MS,
  REQUEST_TTL_MS,
  RESOURCE_PATHS,
  SCOPE,
  TOUCH_EVERY_MS,
  checkRedirectURI,
  errorRedirect,
  oauthError,
  randomToken,
  redirectAllowed,
  redirectHost,
  withQuery,
  type AuthRequest,
  type Client,
  type CodeRecord,
  type Grant,
  type Outcome,
} from "./core";

const iso = (ms: number) => new Date(ms).toISOString();
const ms = (s: string | undefined) => (s ? Date.parse(s) : 0);
const isServiceAccount = (u: User) => u.kind === USER_KIND_SERVICE_ACCOUNT;

/** The stored form of a Grant, with the token hashes. */
interface GrantRecord extends Grant {
  access_hash: string;
  access_expires: string;
  refresh_hash: string;
}

function publicGrant(g: GrantRecord): Grant {
  const out: Grant = {
    id: g.id,
    user_id: g.user_id,
    team_id: g.team_id,
    client_id: g.client_id,
    client_name: g.client_name,
    created_at: g.created_at,
    expires_at: g.expires_at,
  };
  if (g.last_used_at) out.last_used_at = g.last_used_at;
  if (g.revoked_at) out.revoked_at = g.revoked_at;
  return out;
}

export const active = (g: Grant, nowMs: number) => !g.revoked_at && nowMs < ms(g.expires_at);

/** Hex SHA-256 of a token or code (hashOf). */
export const hashOf = (s: string): Promise<string> => sha256Hex(s);

// ---------- records ----------

function getClient(kit: TeamKit, id: string): Client | undefined {
  const r = first<{ data: string }>(kit.sql, "SELECT data FROM mcpauth_clients WHERE id = ?", id);
  return r ? (JSON.parse(r.data) as Client) : undefined;
}

function loadGrant(kit: TeamKit, id: string): GrantRecord | undefined {
  const r = first<{ data: string }>(kit.sql, "SELECT data FROM mcpauth_grants WHERE id = ?", id);
  return r ? (JSON.parse(r.data) as GrantRecord) : undefined;
}

function grantByHash(kit: TeamKit, column: "access_hash" | "refresh_hash", h: string): GrantRecord | undefined {
  const r = first<{ data: string }>(kit.sql, `SELECT data FROM mcpauth_grants WHERE ${column} = ?`, h);
  return r ? (JSON.parse(r.data) as GrantRecord) : undefined;
}

function putGrant(kit: TeamKit, g: GrantRecord): void {
  kit.sql.exec(
    `INSERT INTO mcpauth_grants (id, user_id, access_hash, refresh_hash, created_ms, expires_ms, revoked, data)
     VALUES (?, ?, ?, ?, ?, ?, ?, ?)
     ON CONFLICT(id) DO UPDATE SET access_hash = excluded.access_hash, refresh_hash = excluded.refresh_hash,
       expires_ms = excluded.expires_ms, revoked = excluded.revoked, data = excluded.data`,
    g.id,
    g.user_id,
    g.access_hash,
    g.refresh_hash,
    ms(g.created_at),
    ms(g.expires_at),
    g.revoked_at ? 1 : 0,
    JSON.stringify(g),
  );
}

/** Deletes expired requests, codes and ended sign-ins (called from writers). */
function sweep(kit: TeamKit, nowMs: number): void {
  kit.sql.exec("DELETE FROM mcpauth_requests WHERE expires_ms <= ?", nowMs);
  kit.sql.exec("DELETE FROM mcpauth_codes WHERE expires_ms <= ?", nowMs);
  // A sign-in that ended more than 30 days ago is not listed or usable.
  kit.sql.exec("DELETE FROM mcpauth_grants WHERE expires_ms <= ?", nowMs - REFRESH_TTL_MS);
}

function audit(action: string, outcome: string, g: Partial<Grant>, detail?: Record<string, string>): void {
  console.log(
    JSON.stringify({ msg: "audit", action, outcome, user_id: g.user_id, team_id: g.team_id, client_id: g.client_id, sign_in_id: g.id, ...detail }),
  );
}

interface NewTokens {
  access: string;
  refresh: string;
  accessHash: string;
  refreshHash: string;
}

async function newTokens(): Promise<NewTokens> {
  const access = randomToken(ACCESS_TOKEN_PREFIX);
  const refresh = randomToken(REFRESH_TOKEN_PREFIX);
  return { access, refresh, accessHash: await hashOf(access), refreshHash: await hashOf(refresh) };
}

/** issue: new token pair for g; the old hashes stop working. */
function issue(kit: TeamKit, g: GrantRecord, t: NewTokens, nowMs: number): Outcome {
  g.access_hash = t.accessHash;
  g.refresh_hash = t.refreshHash;
  g.access_expires = iso(nowMs + ACCESS_TTL_MS);
  g.expires_at = iso(nowMs + REFRESH_TTL_MS);
  putGrant(kit, g);
  return {
    kind: "json",
    status: 200,
    body: {
      access_token: t.access,
      token_type: "Bearer",
      expires_in: ACCESS_TTL_MS / 1000,
      refresh_token: t.refresh,
      scope: SCOPE,
    },
  };
}

// ---------- registration ----------

export function register(kit: TeamKit, body: unknown, nowMs: number): Outcome {
  if (typeof body !== "object" || body === null || Array.isArray(body)) {
    return oauthError(400, "invalid_client_metadata", "the body must be a JSON client metadata document");
  }
  const o = body as Record<string, unknown>;
  const uris = Array.isArray(o.redirect_uris) ? o.redirect_uris : o.redirect_uris == null ? [] : null;
  const strs = (v: unknown) => (Array.isArray(v) ? v : v == null ? [] : null);
  const grants = strs(o.grant_types);
  if (
    uris === null || !uris.every((x) => typeof x === "string") || grants === null || !grants.every((x) => typeof x === "string") ||
    (o.client_name != null && typeof o.client_name !== "string") ||
    (o.token_endpoint_auth_method != null && typeof o.token_endpoint_auth_method !== "string")
  ) {
    return oauthError(400, "invalid_client_metadata", "the body must be a JSON client metadata document");
  }
  if (uris.length === 0 || uris.length > MAX_REDIRECT_URIS) {
    return oauthError(400, "invalid_redirect_uri", "give 1 to 10 redirect_uris");
  }
  for (const u of uris as string[]) {
    const e = checkRedirectURI(u);
    if (e) return oauthError(400, "invalid_redirect_uri", e);
  }
  const m = (o.token_endpoint_auth_method as string | undefined) ?? "";
  if (m !== "" && m !== "none") {
    return oauthError(400, "invalid_client_metadata", "only public clients are supported: token_endpoint_auth_method must be none");
  }
  for (const g of grants as string[]) {
    if (g !== "authorization_code" && g !== "refresh_token") return oauthError(400, "invalid_client_metadata", "unsupported grant type " + g);
  }
  let name = ((o.client_name as string | undefined) ?? "").trim();
  if (name.length > 100) name = name.slice(0, 100);
  if (name === "") name = "MCP client";
  const c: Client = { client_id: randomToken("ofc_"), client_name: name, redirect_uris: uris as string[], created_at: iso(nowMs) };
  kit.tx(() => {
    kit.sql.exec("INSERT INTO mcpauth_clients (id, created_ms, data) VALUES (?, ?, ?)", c.client_id, nowMs, JSON.stringify(c));
  });
  return {
    kind: "json",
    status: 201,
    body: {
      client_id: c.client_id,
      client_id_issued_at: Math.floor(nowMs / 1000),
      client_name: c.client_name,
      redirect_uris: c.redirect_uris,
      token_endpoint_auth_method: "none",
      grant_types: ["authorization_code", "refresh_token"],
      response_types: ["code"],
      scope: SCOPE,
    },
  };
}

// ---------- authorization ----------

function validResource(publicURL: string, res: string): boolean {
  if (res === "") return true;
  const r = res.replace(/\/+$/, "");
  if (r === publicURL) return true;
  return RESOURCE_PATHS.some((p) => r === publicURL + p);
}

function userLabel(u: User): string {
  return u.email || u.name || u.id;
}

export interface AuthorizeInput {
  publicURL: string;
  /** Query parameters of GET /oauth/authorize. */
  query: Record<string, string>;
  /** "?..." part of the request, for the login return_to. */
  rawQuery: string;
  /** The signed-in dashboard user (session login), or "". */
  userId: string;
  now: number;
}

/** GET /oauth/authorize: the consent page, a login redirect, or an error. */
export function authorize(kit: TeamKit, input: AuthorizeInput): Outcome {
  const q = input.query;
  const get = (k: string) => q[k] ?? "";
  return kit.tx(() => {
    const c = getClient(kit, get("client_id"));
    // Errors before the redirect URI is known go to the user, not to the client.
    if (!c) {
      return page(400, "Unknown MCP client", "This client is not registered. Remove the server from your MCP client and add it again.");
    }
    let redirect = get("redirect_uri");
    if (redirect === "" && c.redirect_uris.length === 1) redirect = c.redirect_uris[0]!;
    if (!redirectAllowed(c, redirect)) {
      return page(400, "Wrong redirect address", "The redirect_uri of this request is not registered for the client.");
    }
    const state = get("state");
    const fail = (code: string, desc: string): Outcome => ({
      kind: "redirect",
      location: errorRedirect(redirect, state, input.publicURL, code, desc),
    });
    if (get("response_type") !== "code") return fail("unsupported_response_type", "response_type must be code");
    if (get("code_challenge") === "" || get("code_challenge_method") !== "S256") {
      return fail("invalid_request", "PKCE with code_challenge_method S256 is required");
    }
    if (!validResource(input.publicURL, get("resource"))) {
      return fail("invalid_target", "the resource is not this server's Factory MCP endpoint");
    }
    const sc = get("scope");
    if (sc !== "" && sc.split(/\s+/).filter(Boolean).some((x) => x !== SCOPE)) {
      return fail("invalid_scope", "the only scope is " + SCOPE);
    }
    const u = input.userId ? getUser(kit, input.userId) : undefined;
    if (!u) {
      // Sign in to the dashboard first (session login or OIDC), then come back.
      const back = AUTHORIZE_PATH + "?" + input.rawQuery.replace(/^\?/, "");
      return { kind: "redirect", location: `${LOGIN_PATH}?return_to=${encodeURIComponent(back)}` };
    }
    if (isServiceAccount(u)) return page(403, "Not allowed", "A service account cannot sign in. Use an API key.");
    sweep(kit, input.now);
    const req: AuthRequest = {
      id: randomToken("ofq_"),
      client_id: c.client_id,
      redirect_uri: redirect,
      code_challenge: get("code_challenge"),
      user_id: u.id,
      expires: iso(input.now + REQUEST_TTL_MS),
    };
    if (state) req.state = state;
    if (get("resource")) req.resource = get("resource");
    kit.sql.exec(
      "INSERT INTO mcpauth_requests (id, expires_ms, data) VALUES (?, ?, ?)",
      req.id,
      input.now + REQUEST_TTL_MS,
      JSON.stringify(req),
    );
    const name = c.client_name ?? "MCP client";
    return {
      kind: "page",
      status: 200,
      page: {
        title: `Allow ${name}?`,
        consent: true,
        requestId: req.id,
        clientName: name,
        userLabel: userLabel(u),
        redirectHost: redirectHost(redirect),
        action: AUTHORIZE_PATH,
      },
    };
  });
}

function page(status: number, title: string, message: string): Outcome {
  return { kind: "page", status, page: { title, message } };
}

export interface ConsentInput {
  publicURL: string;
  requestId: string;
  decision: string;
  userId: string;
  now: number;
}

/** POST /oauth/authorize: records the answer; redirects with a code or an error. */
export async function consent(kit: TeamKit, input: ConsentInput): Promise<Outcome> {
  const code = randomToken("ofk_");
  const codeHash = await hashOf(code);
  return kit.tx(() => {
    const r = first<{ data: string; expires_ms: number }>(
      kit.sql,
      "SELECT data, expires_ms FROM mcpauth_requests WHERE id = ?",
      input.requestId,
    );
    if (!r || !(input.now < r.expires_ms)) {
      return page(400, "Request expired", "Start the sign-in again from your MCP client.");
    }
    kit.sql.exec("DELETE FROM mcpauth_requests WHERE id = ?", input.requestId); // one answer per request
    const req = JSON.parse(r.data) as AuthRequest;
    const u = input.userId ? getUser(kit, input.userId) : undefined;
    if (!u || u.id !== req.user_id) {
      return page(403, "Not allowed", "Sign in to the dashboard as the same user and start again.");
    }
    if (input.decision !== "allow") {
      audit("mcp.signin.authorize", "failure", { user_id: u.id, team_id: kit.teamId(), client_id: req.client_id }, { reason: "denied" });
      return { kind: "redirect", location: errorRedirect(req.redirect_uri, req.state ?? "", input.publicURL, "access_denied", "the user denied the request") };
    }
    const rec: CodeRecord = {
      client_id: req.client_id,
      redirect_uri: req.redirect_uri,
      code_challenge: req.code_challenge,
      user_id: u.id,
      team_id: kit.teamId(),
      expires: iso(input.now + CODE_TTL_MS),
    };
    kit.sql.exec(
      "INSERT INTO mcpauth_codes (hash, expires_ms, data) VALUES (?, ?, ?)",
      codeHash,
      input.now + CODE_TTL_MS,
      JSON.stringify(rec),
    );
    const v: Record<string, string> = { code, iss: input.publicURL };
    if (req.state) v.state = req.state;
    return { kind: "redirect", location: withQuery(req.redirect_uri, v) };
  });
}

// ---------- token endpoint ----------

/** S256 of a PKCE verifier (43 to 128 characters), or "" when the length is wrong. */
async function pkceChallenge(verifier: string): Promise<string> {
  if (verifier.length < 43 || verifier.length > 128) return "";
  return b64url(await sha256(verifier));
}

/** POST /oauth/token. `form` holds the form fields. */
export async function token(kit: TeamKit, form: Record<string, string>, now: number): Promise<Outcome> {
  switch (form.grant_type ?? "") {
    case "authorization_code":
      return exchangeCode(kit, form, now);
    case "refresh_token":
      return refresh(kit, form, now);
    default:
      return oauthError(400, "unsupported_grant_type", "grant_type must be authorization_code or refresh_token");
  }
}

async function exchangeCode(kit: TeamKit, f: Record<string, string>, now: number): Promise<Outcome> {
  const codeHash = await hashOf(f.code ?? "");
  const challenge = await pkceChallenge(f.code_verifier ?? "");
  const t = await newTokens();
  return kit.tx(() => {
    const r = first<{ data: string }>(kit.sql, "SELECT data FROM mcpauth_codes WHERE hash = ?", codeHash);
    if (r) kit.sql.exec("DELETE FROM mcpauth_codes WHERE hash = ?", codeHash); // a code works once
    const rec = r ? (JSON.parse(r.data) as CodeRecord) : undefined;
    if (!rec || !(now < ms(rec.expires))) return oauthError(400, "invalid_grant", "the code is unknown, used or expired");
    if ((f.client_id ?? "") !== rec.client_id) return oauthError(400, "invalid_grant", "the code was issued to another client");
    if ((f.redirect_uri ?? "") !== "" && f.redirect_uri !== rec.redirect_uri) {
      return oauthError(400, "invalid_grant", "redirect_uri does not match the authorization request");
    }
    if (challenge === "" || !timingSafeEqual(challenge, rec.code_challenge)) {
      return oauthError(400, "invalid_grant", "the code_verifier does not match the code_challenge");
    }
    const c = getClient(kit, rec.client_id);
    if (!c) return oauthError(400, "invalid_client", "unknown client");
    const g: GrantRecord = {
      id: newId("mcps"),
      user_id: rec.user_id,
      team_id: rec.team_id,
      client_id: c.client_id,
      client_name: c.client_name ?? "MCP client",
      created_at: iso(now),
      expires_at: "",
      access_hash: "",
      access_expires: "",
      refresh_hash: "",
    };
    const res = issue(kit, g, t, now);
    audit("mcp.signin.authorize", "success", publicGrant(g));
    return res;
  });
}

async function refresh(kit: TeamKit, f: Record<string, string>, now: number): Promise<Outcome> {
  const tok = f.refresh_token ?? "";
  if (!tok.startsWith(REFRESH_TOKEN_PREFIX)) return oauthError(400, "invalid_grant", "unknown refresh token");
  const h = await hashOf(tok);
  const t = await newTokens();
  return kit.tx(() => {
    const g = grantByHash(kit, "refresh_hash", h);
    if (!g) return oauthError(400, "invalid_grant", "the refresh token is unknown, used or revoked");
    if (!active(g, now)) return oauthError(400, "invalid_grant", "the sign-in ended; sign in again");
    const cid = f.client_id ?? "";
    if (cid !== "" && cid !== g.client_id) return oauthError(400, "invalid_grant", "the refresh token was issued to another client");
    const u = getUser(kit, g.user_id);
    if (!u || u.team_id !== g.team_id) return oauthError(400, "invalid_grant", "the user of this sign-in no longer exists");
    return issue(kit, g, t, now);
  });
}

/** POST /oauth/revoke (RFC 7009): 200 also for unknown tokens. */
export async function revokeToken(kit: TeamKit, tok: string, now: number): Promise<void> {
  let column: "access_hash" | "refresh_hash" | "" = "";
  if (tok.startsWith(ACCESS_TOKEN_PREFIX)) column = "access_hash";
  else if (tok.startsWith(REFRESH_TOKEN_PREFIX)) column = "refresh_hash";
  if (column === "") return;
  const h = await hashOf(tok);
  kit.tx(() => {
    const g = grantByHash(kit, column, h);
    if (g) revokeRecord(kit, g, now);
  });
}

function revokeRecord(kit: TeamKit, g: GrantRecord, now: number): void {
  if (!g.revoked_at) g.revoked_at = iso(now);
  // Hashes that cannot match a token: both tokens stop working at once.
  g.access_hash = "revoked:" + g.id;
  g.refresh_hash = "revoked:" + g.id;
  putGrant(kit, g);
  audit("mcp.signin.revoke", "success", publicGrant(g));
}

// ---------- verify, list, revoke ----------

/**
 * Verify: checks an access token hash and returns the principal of its user
 * (kind "factory_mcp"). The user must still exist in the team of the sign-in.
 */
export function verify(kit: TeamKit, accessHash: string, now: number): { principal: Principal; grantId: string } | null {
  return kit.tx(() => {
    const g = grantByHash(kit, "access_hash", accessHash);
    if (!g || !(now < ms(g.access_expires)) || !active(g, now)) return null;
    const u = getUser(kit, g.user_id);
    if (!u || u.team_id !== g.team_id || isServiceAccount(u)) return null;
    if (!g.last_used_at || now - ms(g.last_used_at) >= TOUCH_EVERY_MS) {
      g.last_used_at = iso(now);
      putGrant(kit, g);
    }
    const p = principalOf(kit, u);
    return { principal: { ...p, kind: "factory_mcp", keyId: g.id }, grantId: g.id };
  });
}

/** Active sign-ins of a user ("" = the whole team), newest first. */
export function listGrants(kit: TeamKit, userId: string, now: number): Grant[] {
  const rows = userId
    ? all<{ data: string }>(kit.sql, "SELECT data FROM mcpauth_grants WHERE user_id = ? AND revoked = 0", userId)
    : all<{ data: string }>(kit.sql, "SELECT data FROM mcpauth_grants WHERE revoked = 0");
  return rows
    .map((r) => JSON.parse(r.data) as GrantRecord)
    .filter((g) => active(g, now))
    .sort((a, b) => ms(b.created_at) - ms(a.created_at))
    .map(publicGrant);
}

export function getGrant(kit: TeamKit, id: string): Grant | undefined {
  const g = loadGrant(kit, id);
  return g ? publicGrant(g) : undefined;
}

/** Revoke: ends a sign-in; its access token and refresh token stop working at once. */
export function revokeGrant(kit: TeamKit, id: string, now: number): boolean {
  return kit.tx(() => {
    const g = loadGrant(kit, id);
    if (!g) return false;
    revokeRecord(kit, g, now);
    return true;
  });
}
