// Worker-side authentication: turns a request into a Principal. Order of the
// Go server: session cookie (with CSRF on unsafe methods) when no API-key
// bearer is present, then the bearer API key (of_/ofa_). The owner DO
// authorizes (plan rule 6).
import { DEFAULT_TEAM, OfError, doName } from "@openfactory/core";
import type { ApiKeyLookup, Principal } from "../contracts/do-messages";
import type { Env } from "../contracts/env";
import { serverConfig } from "../config";
import { csrfOk } from "../http/csrf";
import { bearerToken, hashKey, isApiKey, parseKey } from "./apikeys";
import { isSafeMethod } from "./roles";
import { readSession, SESSION_COOKIE, getCookie } from "./session";

/** Extra bearer schemes (for example ofm_ Factory MCP tokens, WS11). Return null to pass. */
export type BearerAuthenticator = (env: Env, token: string) => Promise<Principal | null>;

const bearerAuthenticators: { prefix: string; fn: BearerAuthenticator }[] = [];

/** Registers an authenticator for bearer tokens with a prefix (module load time). */
export function registerBearerAuthenticator(prefix: string, fn: BearerAuthenticator): void {
  bearerAuthenticators.push({ prefix, fn });
}

const KEY_CACHE_TTL_MS = 10_000;
const keyCache = new Map<string, { at: number; v: ApiKeyLookup | null }>();

/** Drops a cached API-key lookup (after DELETE /api-keys/{id} in this isolate). */
export function invalidateKeyCache(): void {
  keyCache.clear();
}

/** Principal of an API key, or throws 401 "invalid API key". */
export async function apiKeyPrincipal(env: Env, key: string): Promise<Principal> {
  if (!parseKey(key)) throw new OfError("authentication_required", "invalid API key");
  const hash = await hashKey(key);
  const now = Date.now();
  const hit = keyCache.get(hash);
  let v: ApiKeyLookup | null;
  if (hit && now - hit.at < KEY_CACHE_TTL_MS) v = hit.v;
  else {
    v = await env.TEAM.getByName(doName.team(DEFAULT_TEAM)).authLookupApiKey(hash);
    keyCache.set(hash, { at: now, v });
    if (keyCache.size > 1000) keyCache.delete(keyCache.keys().next().value!);
  }
  if (!v) throw new OfError("authentication_required", "invalid API key");
  return v.principal;
}

/** Principal of a valid session cookie, or null. */
export async function sessionPrincipal(env: Env, req: Request): Promise<Principal | null> {
  const c = await readSession(env, req);
  if (!c) return null;
  return env.TEAM.getByName(doName.team(c.t || DEFAULT_TEAM)).authSession(JSON.stringify(c));
}

/**
 * Authenticate: null without credentials; throws 401 for a bad bearer key
 * and 403 for a cookie-authenticated unsafe request without the CSRF marker.
 */
export async function authenticate(req: Request, env: Env): Promise<Principal | null> {
  const tok = bearerToken(req);
  if (isApiKey(tok)) return apiKeyPrincipal(env, tok);
  for (const a of bearerAuthenticators) {
    if (tok.startsWith(a.prefix)) {
      const p = await a.fn(env, tok);
      if (!p) throw new OfError("authentication_required", "invalid token");
      return p;
    }
  }
  if (tok !== "") throw new OfError("authentication_required", "invalid API key");
  const p = await sessionPrincipal(env, req);
  if (!p) return null;
  if (!isSafeMethod(req.method) && !csrfOk(req, serverConfig(env).origin)) {
    throw new OfError("not_authorized", "missing X-OF-CSRF: 1 header");
  }
  return p;
}

/** True when the request carries a session cookie (to clear a stale one). */
export function hasSessionCookie(req: Request): boolean {
  return !!getCookie(req, SESSION_COOKIE);
}
