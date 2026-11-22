// Port of internal/mcpregistry/oauth.go: OAuth for registered MCP servers.
// Protected resource metadata (RFC 9728) names the authorization server,
// authorization server metadata (RFC 8414 / OpenID discovery) gives its
// endpoints, dynamic client registration (RFC 7591) supplies a client when
// none is configured, and the code flow uses PKCE (S256) and the resource
// indicator (RFC 8707). Discovery and token calls run here in the Worker;
// tokens are stored encrypted in TeamDO, which also serializes refreshes.
import { MCP_OAUTH_ERROR, OfError, type RegisteredMCPServer } from "@openfactory/core";
import type { CallMeta } from "../../contracts/do-messages";
import type { Env } from "../../contracts/env";
import { b64, b64url, fromB64, fromUtf8, randomBytes, sha256, utf8 } from "../../lib/bytes";
import { MCP_STATE_TYPE, MCP_TOKEN_TYPE } from "../../secrets/envelope";
import { teamOp, teamStub, type McpRuntime } from "../team";
import { CALLBACK_PATH } from "./store";
import { checkURL } from "./validate";

const STATE_TTL_MS = 10 * 60 * 1000;
const REFRESH_SKEW_MS = 60_000;
const MAX_METADATA_BYTES = 1 << 20;
const START_TIMEOUT_MS = 30_000;
const HTTP_TIMEOUT_MS = 30_000;
const BUSY_WAIT_MS = 250;
const BUSY_TRIES = 40;

/** A server has no usable OAuth token. */
export class NotConnectedError extends Error {
  constructor(detail = "") {
    super("mcp server is not connected" + (detail ? ": " + detail : ""));
  }
}

/** The stored OAuth state of one server. */
export interface TokenSet {
  access_token: string;
  refresh_token?: string;
  token_type?: string;
  /** RFC 3339; absent when the provider gave no expiry. */
  expires_at?: string;
  scope?: string;
  token_endpoint: string;
  client_id: string;
  client_secret?: string;
  /** none, client_secret_basic, client_secret_post */
  auth_method?: string;
  resource?: string;
}

/** An authorization in progress, keyed by state (stored encrypted). */
interface PendingAuth {
  server_id: string;
  team_id: string;
  user_id: string;
  verifier: string;
  token_endpoint: string;
  client_id: string;
  client_secret?: string;
  auth_method?: string;
  resource?: string;
  expires_at: string;
}

interface ResourceMetadata {
  resource?: string;
  authorization_servers?: string[];
  scopes_supported?: string[];
}

interface ASMetadata {
  issuer?: string;
  authorization_endpoint?: string;
  token_endpoint?: string;
  registration_endpoint?: string;
  token_endpoint_auth_methods_supported?: string[];
  code_challenge_methods_supported?: string[];
}

export const tokenName = (serverId: string) => `mcp-oauth:${serverId}`;
const stateName = (state: string) => `mcp-oauth-state:${state}`;

export function redirectURI(env: Env): string {
  return (env.OF_PUBLIC_URL ?? "").replace(/\/+$/, "") + CALLBACK_PATH;
}

const invalid = (msg: string) => new OfError("invalid_request", msg);

function randomString(n: number): string {
  return b64url(randomBytes(n));
}

async function withTimeout<T>(ms: number, fn: (signal: AbortSignal) => Promise<T>): Promise<T> {
  const c = new AbortController();
  const t = setTimeout(() => c.abort(new Error("timeout")), ms);
  try {
    return await fn(c.signal);
  } finally {
    clearTimeout(t);
  }
}

async function readLimited(res: Response): Promise<string> {
  const text = await res.text();
  return text.length > MAX_METADATA_BYTES ? text.slice(0, MAX_METADATA_BYTES) : text;
}

async function getJSON<T>(rt: McpRuntime, url: string, signal: AbortSignal): Promise<T | null> {
  try {
    const res = await rt.fetch(url, { headers: { accept: "application/json" }, signal });
    if (res.status !== 200) {
      await res.body?.cancel();
      return null;
    }
    return JSON.parse(await readLimited(res)) as T;
  } catch {
    return null;
  }
}

/** Extracts a parameter from Bearer WWW-Authenticate challenges. */
export function authParam(values: string[], name: string): string {
  for (const v of values) {
    for (let part of v.split(",")) {
      part = part.trim();
      const sp = part.indexOf(" ");
      if (sp > 0 && part.slice(0, sp).toLowerCase() === "bearer") part = part.slice(sp + 1).trim();
      const eq = part.indexOf("=");
      if (eq >= 0 && part.slice(0, eq).trim().toLowerCase() === name.toLowerCase()) {
        return part
          .slice(eq + 1)
          .trim()
          .replace(/^"+|"+$/g, "");
      }
    }
  }
  return "";
}

/** canonicalResource: no fragment or query, lower-case scheme and host. */
export function canonicalResource(raw: string): string {
  try {
    const u = new URL(raw);
    const hadPath = /^[a-z][a-z0-9+.-]*:\/\/[^/?#]+\//i.test(raw);
    return `${u.protocol}//${u.host}${hadPath ? u.pathname : ""}`;
  } catch {
    return raw;
  }
}

function originOf(raw: string): string {
  try {
    const u = new URL(raw);
    return `${u.protocol}//${u.host}`;
  } catch {
    return raw;
  }
}

/** Finds the protected resource metadata of an MCP server, or null. */
async function discoverResource(rt: McpRuntime, mcpURL: string, signal: AbortSignal): Promise<ResourceMetadata | null> {
  const candidates: string[] = [];
  const init = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"openfactory","version":"1"}}}`;
  try {
    const res = await rt.fetch(mcpURL, {
      method: "POST",
      headers: { "content-type": "application/json", accept: "application/json, text/event-stream" },
      body: init,
      signal,
    });
    await res.body?.cancel();
    if (res.status === 401) {
      const hdr = res.headers.get("www-authenticate");
      const u = hdr ? authParam([hdr], "resource_metadata") : "";
      if (u) candidates.push(u);
    }
  } catch {
    /* unreachable: fall back to the well-known locations */
  }
  try {
    const u = new URL(mcpURL);
    const origin = `${u.protocol}//${u.host}`;
    const p = u.pathname.replace(/\/+$/, "");
    if (p !== "") candidates.push(origin + "/.well-known/oauth-protected-resource" + p);
    candidates.push(origin + "/.well-known/oauth-protected-resource");
  } catch {
    /* invalid URL */
  }
  for (const c of candidates) {
    const md = await getJSON<ResourceMetadata>(rt, c, signal);
    if (md && (md.authorization_servers ?? []).length > 0) return md;
  }
  return null;
}

/**
 * Reads authorization server metadata. Without metadata it falls back to
 * /authorize, /token and /register on the issuer's origin (2025-03-26 spec).
 */
async function discoverAuthServer(rt: McpRuntime, issuer: string, signal: AbortSignal): Promise<ASMetadata> {
  let u: URL;
  try {
    u = new URL(issuer);
  } catch {
    throw new Error(`invalid authorization server "${issuer}"`);
  }
  if (u.host === "") throw new Error(`invalid authorization server "${issuer}"`);
  const origin = `${u.protocol}//${u.host}`;
  const p = u.pathname.replace(/\/+$/, "");
  const candidates = [
    origin + "/.well-known/oauth-authorization-server" + p,
    origin + "/.well-known/openid-configuration" + p,
  ];
  if (p !== "") candidates.push(origin + p + "/.well-known/openid-configuration");
  for (const c of candidates) {
    const md = await getJSON<ASMetadata>(rt, c, signal);
    if (md && md.authorization_endpoint && md.token_endpoint) return md;
  }
  return {
    issuer: origin,
    authorization_endpoint: origin + "/authorize",
    token_endpoint: origin + "/token",
    registration_endpoint: origin + "/register",
  };
}

function pickAuthMethod(supported: string[] | undefined): string {
  const s = supported ?? [];
  if (s.length > 0 && !s.includes("client_secret_basic") && s.includes("client_secret_post"))
    return "client_secret_post";
  return "client_secret_basic";
}

/** Dynamic client registration; fills p's client. */
async function registerClient(
  env: Env,
  rt: McpRuntime,
  endpoint: string,
  p: PendingAuth,
  signal: AbortSignal,
): Promise<void> {
  let res: Response;
  try {
    res = await rt.fetch(endpoint, {
      method: "POST",
      headers: { "content-type": "application/json", accept: "application/json" },
      body: JSON.stringify({
        client_name: "Open Factory",
        redirect_uris: [redirectURI(env)],
        grant_types: ["authorization_code", "refresh_token"],
        response_types: ["code"],
        token_endpoint_auth_method: "none",
      }),
      signal,
    });
  } catch (e) {
    throw new Error(`client registration: ${(e as Error).message}`, { cause: e });
  }
  let out: Record<string, unknown> = {};
  try {
    out = JSON.parse(await readLimited(res)) as Record<string, unknown>;
  } catch {
    /* empty or not JSON */
  }
  const str = (k: string) => (typeof out[k] === "string" ? (out[k] as string) : "");
  if (res.status >= 300 || str("client_id") === "") {
    const msg = `${str("error")} ${str("error_description")}`.trim() || `HTTP ${res.status}`;
    throw new Error(`client registration failed: ${msg}`);
  }
  p.client_id = str("client_id");
  if (str("client_secret")) p.client_secret = str("client_secret");
  if (str("client_secret") === "") p.auth_method = "none";
  else if (str("token_endpoint_auth_method") === "client_secret_post") p.auth_method = "client_secret_post";
  else p.auth_method = "client_secret_basic";
}

/** Reads a team secret (the OAuth client secret) through TeamDO. */
async function readTeamSecret(env: Env, meta: CallMeta, name: string): Promise<string> {
  const vals = await teamStub(env).secretsResolve(meta, { factoryId: "", names: [name] });
  const v = vals[name];
  if (v === undefined) throw new OfError("resource_not_found", `secret ${name} not found`);
  return v;
}

/**
 * StartOAuth: begins the authorization code flow for m on behalf of userId
 * and returns the provider's authorization URL.
 */
export async function startOAuth(
  env: Env,
  rt: McpRuntime,
  meta: CallMeta,
  m: RegisteredMCPServer,
  userId: string,
): Promise<string> {
  if (!m.oauth) throw invalid(`MCP server "${m.name}" has no oauth configuration`);
  if (!env.OF_PUBLIC_URL) throw new Error("OAuth needs the server's public URL (OF_PUBLIC_URL) for the redirect URI");
  const url = m.url ?? "";
  return withTimeout(START_TIMEOUT_MS, async (signal) => {
    let resource = canonicalResource(url);
    const prm = await discoverResource(rt, url, signal);
    let issuer = originOf(url);
    if (prm && (prm.authorization_servers ?? []).length > 0) {
      issuer = prm.authorization_servers![0]!;
      if (prm.resource) resource = prm.resource;
    }
    const as = await discoverAuthServer(rt, issuer, signal);
    if (m.oauth!.authorization_endpoint) as.authorization_endpoint = m.oauth!.authorization_endpoint;
    if (m.oauth!.token_endpoint) as.token_endpoint = m.oauth!.token_endpoint;
    for (const [name, v] of [
      ["authorization endpoint", as.authorization_endpoint ?? ""],
      ["token endpoint", as.token_endpoint ?? ""],
    ] as const) {
      const e = checkURL(v, true);
      if (e) throw new Error(`oauth ${name} "${v}": ${e}`);
    }
    const ccm = as.code_challenge_methods_supported ?? [];
    if (ccm.length > 0 && !ccm.includes("S256"))
      throw new Error("the authorization server does not support PKCE with S256");

    const now = rt.now();
    const p: PendingAuth = {
      server_id: m.id,
      team_id: m.team_id,
      user_id: userId,
      verifier: "",
      token_endpoint: as.token_endpoint!,
      client_id: "",
      resource,
      expires_at: new Date(now + STATE_TTL_MS).toISOString(),
    };
    if (m.oauth!.client_id) {
      p.client_id = m.oauth!.client_id;
      if (m.oauth!.client_secret_name) {
        try {
          p.client_secret = await readTeamSecret(env, meta, m.oauth!.client_secret_name);
        } catch (e) {
          throw new Error(`oauth client secret ${m.oauth!.client_secret_name}: ${(e as Error).message}`, { cause: e });
        }
        p.auth_method = pickAuthMethod(as.token_endpoint_auth_methods_supported);
      } else p.auth_method = "none";
    } else if (as.registration_endpoint) {
      const e = checkURL(as.registration_endpoint, true);
      if (e) throw new Error(`oauth registration endpoint "${as.registration_endpoint}": ${e}`);
      await registerClient(env, rt, as.registration_endpoint, p, signal);
    } else {
      throw invalid(
        "the authorization server does not support dynamic client registration: register a client with the provider and set oauth.client_id (and oauth.client_secret_name)",
      );
    }

    const verifier = randomString(48);
    const state = randomString(32);
    p.verifier = verifier;
    const enc = await rt.encrypter();
    const sealed = await enc.encrypt(m.team_id, "", stateName(state), MCP_STATE_TYPE, utf8(JSON.stringify(p)));
    await teamOp(env, "mcp.oauth.saveState", { state, envelope: b64(sealed), now });

    let scopes = m.oauth!.scopes ?? [];
    if (scopes.length === 0 && prm) scopes = prm.scopes_supported ?? [];
    const challenge = b64url(await sha256(verifier));
    const au = new URL(as.authorization_endpoint!);
    const q: Record<string, string> = {
      response_type: "code",
      client_id: p.client_id,
      redirect_uri: redirectURI(env),
      state,
      code_challenge: challenge,
      code_challenge_method: "S256",
    };
    if (resource) q.resource = resource;
    if (scopes.length > 0) q.scope = scopes.join(" ");
    for (const [k, v] of Object.entries(q)) au.searchParams.set(k, v);
    // Go url.Values.Encode sorts the keys.
    const sorted = [...au.searchParams.entries()].sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0));
    au.search = new URLSearchParams(sorted).toString();
    return au.toString();
  });
}

/**
 * CompleteOAuth: finishes the flow. `envelope` is the stored state that the
 * TeamDO handed out (single use). Returns the server on success; throws an
 * OfError(invalid_request) with the server attached for provider errors.
 */
export async function completeOAuth(
  env: Env,
  rt: McpRuntime,
  input: { teamId: string; userId: string; state: string; envelope: string; code: string; providerErr: string },
): Promise<{ server: RegisteredMCPServer | null; error?: OfError }> {
  if (input.state === "") throw invalid("missing state");
  if (input.envelope === "") throw invalid("unknown or expired authorization state; start the connection again");
  const enc = await rt.encrypter();
  let p: PendingAuth;
  try {
    const plain = await enc.decrypt(input.teamId, "", stateName(input.state), MCP_STATE_TYPE, fromB64(input.envelope));
    p = JSON.parse(fromUtf8(plain)) as PendingAuth;
  } catch {
    // The state is sealed to the team that started it.
    throw invalid("this authorization was started by another user");
  }
  if (p.team_id !== input.teamId || p.user_id !== input.userId)
    throw invalid("this authorization was started by another user");
  if (rt.now() > Date.parse(p.expires_at)) throw invalid("the authorization expired; start the connection again");
  const m = await teamOp<RegisteredMCPServer | null>(env, "mcp.server.get", { serverId: p.server_id });
  if (!m || m.team_id !== input.teamId) throw new OfError("resource_not_found", "MCP server no longer exists");
  if (!m.oauth) throw invalid(`MCP server "${m.name}" no longer uses OAuth`);
  if (input.providerErr !== "") {
    const after = await setStatus(env, rt, m.id, MCP_OAUTH_ERROR, "authorization failed: " + input.providerErr);
    return { server: after ?? m, error: invalid(`the provider returned an error: ${input.providerErr}`) };
  }
  if (input.code === "") throw invalid("missing authorization code");
  const ts: TokenSet = {
    access_token: "",
    token_endpoint: p.token_endpoint,
    client_id: p.client_id,
    ...(p.client_secret ? { client_secret: p.client_secret } : {}),
    ...(p.auth_method ? { auth_method: p.auth_method } : {}),
    ...(p.resource ? { resource: p.resource } : {}),
  };
  try {
    await tokenRequest(rt, ts, {
      grant_type: "authorization_code",
      code: input.code,
      redirect_uri: redirectURI(env),
      code_verifier: p.verifier,
    });
  } catch (e) {
    const msg = (e as Error).message;
    const after = await setStatus(env, rt, m.id, MCP_OAUTH_ERROR, msg);
    return { server: after ?? m, error: invalid(msg) };
  }
  const sealed = await sealToken(rt, m, ts);
  const done = await teamOp<RegisteredMCPServer>(env, "mcp.oauth.complete", {
    serverId: m.id,
    envelope: sealed,
    userId: input.userId,
    now: rt.now(),
  });
  console.log("mcp server connected", m.name, input.userId);
  return { server: done };
}

async function setStatus(env: Env, rt: McpRuntime, serverId: string, status: string, message: string) {
  return teamOp<RegisteredMCPServer | null>(env, "mcp.oauth.setStatus", { serverId, status, message, now: rt.now() });
}

async function sealToken(
  rt: McpRuntime,
  m: Pick<RegisteredMCPServer, "id" | "team_id">,
  ts: TokenSet,
): Promise<string> {
  const enc = await rt.encrypter();
  return b64(await enc.encrypt(m.team_id, "", tokenName(m.id), MCP_TOKEN_TYPE, utf8(JSON.stringify(ts))));
}

async function openToken(
  rt: McpRuntime,
  m: Pick<RegisteredMCPServer, "id" | "team_id">,
  envelope: string,
): Promise<TokenSet> {
  if (envelope === "") throw new NotConnectedError();
  const enc = await rt.encrypter();
  let ts: TokenSet;
  try {
    ts = JSON.parse(
      fromUtf8(await enc.decrypt(m.team_id, "", tokenName(m.id), MCP_TOKEN_TYPE, fromB64(envelope))),
    ) as TokenSet;
  } catch (e) {
    throw new Error(`stored oauth token: ${(e as Error).message}`, { cause: e });
  }
  if (!ts.access_token) throw new NotConnectedError();
  return ts;
}

const expiring = (rt: McpRuntime, ts: TokenSet) =>
  !!ts.expires_at && rt.now() + REFRESH_SKEW_MS > Date.parse(ts.expires_at);

/**
 * AccessToken: a valid access token for m, refreshed when it is about to
 * expire. The TeamDO grants one refresh lease at a time (single flight).
 */
export async function accessToken(env: Env, rt: McpRuntime, m: RegisteredMCPServer): Promise<string> {
  const { envelope } = await teamOp<{ envelope: string }>(env, "mcp.oauth.token", { serverId: m.id });
  const ts = await openToken(rt, m, envelope);
  if (!expiring(rt, ts)) return ts.access_token;
  for (let i = 0; i < BUSY_TRIES; i++) {
    const begin = await teamOp<{ status: string; lease?: string; envelope: string }>(env, "mcp.oauth.refreshBegin", {
      serverId: m.id,
      now: rt.now(),
    });
    if (begin.status === "missing") throw new NotConnectedError();
    const cur = await openToken(rt, m, begin.envelope);
    if (begin.status === "busy") {
      // Another request refreshes now; use its result when it lands.
      if (!expiring(rt, cur)) return cur.access_token;
      await new Promise((r) => setTimeout(r, BUSY_WAIT_MS));
      continue;
    }
    const lease = begin.lease!;
    if (!expiring(rt, cur)) {
      await teamOp(env, "mcp.oauth.refreshEnd", { serverId: m.id, lease, envelope: "", now: rt.now() });
      return cur.access_token;
    }
    if (!cur.refresh_token) {
      const msg = "the access token expired and the provider issued no refresh token; reconnect the server";
      await teamOp(env, "mcp.oauth.refreshEnd", { serverId: m.id, lease, envelope: "", error: msg, now: rt.now() });
      throw new NotConnectedError(msg);
    }
    const used = cur.refresh_token;
    try {
      await tokenRequest(rt, cur, { grant_type: "refresh_token", refresh_token: used });
    } catch (e) {
      const msg = "token refresh failed: " + (e as Error).message;
      await teamOp(env, "mcp.oauth.refreshEnd", { serverId: m.id, lease, envelope: "", error: msg, now: rt.now() });
      throw new NotConnectedError((e as Error).message);
    }
    if (!cur.refresh_token) cur.refresh_token = used; // providers may omit an unrotated refresh token
    const sealed = await sealToken(rt, m, cur);
    await teamOp(env, "mcp.oauth.refreshEnd", { serverId: m.id, lease, envelope: sealed, now: rt.now() });
    return cur.access_token;
  }
  throw new NotConnectedError("the token refresh did not finish in time");
}

function expiresIn(v: unknown): number {
  if (typeof v === "number") return Math.trunc(v);
  if (typeof v === "string" && /^-?\d+$/.test(v)) return Number(v);
  return 0;
}

/** Posts form to the token endpoint and stores the response in ts. */
export async function tokenRequest(rt: McpRuntime, ts: TokenSet, form: Record<string, string>): Promise<void> {
  const f = new URLSearchParams(form);
  if (ts.resource) f.set("resource", ts.resource);
  const headers: Record<string, string> = {
    "content-type": "application/x-www-form-urlencoded",
    accept: "application/json",
  };
  switch (ts.auth_method) {
    case "client_secret_basic":
      headers.authorization =
        "Basic " + b64(utf8(`${encodeURIComponent(ts.client_id)}:${encodeURIComponent(ts.client_secret ?? "")}`));
      break;
    case "client_secret_post":
      f.set("client_id", ts.client_id);
      f.set("client_secret", ts.client_secret ?? "");
      break;
    default:
      f.set("client_id", ts.client_id);
  }
  const sorted = new URLSearchParams([...f.entries()].sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0)));
  let res: Response;
  try {
    res = await withTimeout(HTTP_TIMEOUT_MS, (signal) =>
      rt.fetch(ts.token_endpoint, { method: "POST", headers, body: sorted.toString(), signal }),
    );
  } catch (e) {
    throw new Error(`token endpoint: ${(e as Error).message}`, { cause: e });
  }
  const trimmed = (await readLimited(res)).trim();
  let out: Record<string, unknown> = {};
  const ct = (res.headers.get("content-type") ?? "").split(";")[0]!.trim().toLowerCase();
  if (ct === "application/x-www-form-urlencoded" || (trimmed.length > 0 && trimmed[0] !== "{")) {
    // Some providers answer with a form-encoded body.
    const v = new URLSearchParams(trimmed);
    out = Object.fromEntries(
      ["access_token", "token_type", "refresh_token", "scope", "expires_in", "error", "error_description"].map((k) => [
        k,
        v.get(k) ?? "",
      ]),
    );
  } else {
    try {
      out = JSON.parse(trimmed) as Record<string, unknown>;
    } catch (e) {
      if (res.status < 300) throw new Error(`token endpoint: invalid response: ${(e as Error).message}`, { cause: e });
    }
  }
  const str = (k: string) => (typeof out[k] === "string" ? (out[k] as string) : "");
  if (res.status >= 300 || str("error") !== "" || str("access_token") === "") {
    let msg = str("error");
    if (str("error_description")) msg += ": " + str("error_description");
    if (msg === "") msg = `HTTP ${res.status}`;
    throw new Error(`token endpoint: ${msg}`);
  }
  if (str("token_type") !== "" && str("token_type").toLowerCase() !== "bearer") {
    throw new Error(`token endpoint: unsupported token type "${str("token_type")}"`);
  }
  ts.access_token = str("access_token");
  if (str("token_type")) ts.token_type = str("token_type");
  else delete ts.token_type;
  if (str("scope")) ts.scope = str("scope");
  else delete ts.scope;
  if (str("refresh_token")) ts.refresh_token = str("refresh_token");
  else delete ts.refresh_token;
  delete ts.expires_at;
  const secs = expiresIn(out.expires_in);
  if (secs > 0) ts.expires_at = new Date(rt.now() + secs * 1000).toISOString();
}
