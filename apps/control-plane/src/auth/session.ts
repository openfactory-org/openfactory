// Session and login-flow cookies (Worker side). Port of the cookie parts of
// internal/auth/service.go: of_session sealed with the web-session key,
// HttpOnly, SameSite=Lax, Secure for an https public URL, 7-day TTL.
import type { Env } from "../contracts/env";
import { LABELS, derivedSigningKeys, serverConfig } from "../config";
import type { SessionClaims } from "../do/team/auth";
import { Sealer } from "./seal";

export const SESSION_COOKIE = "of_session";
export const SESSION_TTL_MS = 7 * 24 * 3600_000;

const sealers = new Map<string, Promise<Sealer>>();

async function sealer(env: Env, purpose: "session" | "login-flow"): Promise<Sealer> {
  const ks = await derivedSigningKeys(env, LABELS.webSession);
  const id = purpose + ":" + [...ks.current].join(",");
  let p = sealers.get(id);
  if (!p) {
    p = Sealer.create(ks.current, purpose, ks.previous);
    sealers.set(id, p);
  }
  return p;
}

/** Reads one cookie value from a request. */
export function getCookie(req: Request, name: string): string | undefined {
  const h = req.headers.get("cookie");
  if (!h) return undefined;
  for (const part of h.split(";")) {
    const i = part.indexOf("=");
    if (i < 0) continue;
    if (part.slice(0, i).trim() === name) return part.slice(i + 1).trim();
  }
  return undefined;
}

interface CookieOpts {
  path: string;
  maxAgeS: number;
  secure: boolean;
  expires?: Date;
}

export function cookieString(name: string, value: string, o: CookieOpts): string {
  const parts = [`${name}=${value}`, `Path=${o.path}`];
  if (o.expires) parts.push(`Expires=${o.expires.toUTCString()}`);
  parts.push(o.maxAgeS < 0 ? "Max-Age=0" : `Max-Age=${o.maxAgeS}`);
  parts.push("HttpOnly");
  if (o.secure) parts.push("Secure");
  parts.push("SameSite=Lax");
  return parts.join("; ");
}

/** Set-Cookie value of a new session. */
export async function issueSessionCookie(env: Env, c: Omit<SessionClaims, "iat">, nowMs = Date.now()): Promise<string> {
  const claims: SessionClaims = { u: c.u, t: c.t, p: c.p, ...(c.k ? { k: c.k, kp: c.kp } : {}), iat: Math.floor(nowMs / 1000) };
  const exp = nowMs + SESSION_TTL_MS;
  const v = await (await sealer(env, "session")).seal(claims, SESSION_COOKIE, exp);
  return cookieString(SESSION_COOKIE, v, {
    path: "/",
    maxAgeS: SESSION_TTL_MS / 1000,
    secure: serverConfig(env).secureCookies,
    expires: new Date(exp),
  });
}

export function clearSessionCookie(env: Env): string {
  return cookieString(SESSION_COOKIE, "", { path: "/", maxAgeS: -1, secure: serverConfig(env).secureCookies });
}

/** The claims of a valid session cookie, or null. */
export async function readSession(env: Env, req: Request, nowMs = Date.now()): Promise<SessionClaims | null> {
  const v = getCookie(req, SESSION_COOKIE);
  if (!v) return null;
  return (await sealer(env, "session")).open<SessionClaims>(v, SESSION_COOKIE, nowMs);
}

const flowCookie = (provider: string) => "of_flow_" + provider;

/** SaveFlow: Set-Cookie of an encrypted login-flow state scoped to /auth/{provider}/. */
export async function saveFlow(env: Env, provider: string, v: unknown, ttlMs: number): Promise<string> {
  const name = flowCookie(provider);
  const val = await (await sealer(env, "login-flow")).seal(v, name, Date.now() + ttlMs);
  return cookieString(name, val, { path: `/auth/${provider}/`, maxAgeS: Math.floor(ttlMs / 1000), secure: serverConfig(env).secureCookies });
}

/** LoadFlow: the saved state (or null) and the Set-Cookie that clears it. */
export async function loadFlow<T>(env: Env, req: Request, provider: string): Promise<{ value: T | null; clear: string }> {
  const name = flowCookie(provider);
  const clear = cookieString(name, "", { path: `/auth/${provider}/`, maxAgeS: -1, secure: serverConfig(env).secureCookies });
  const v = getCookie(req, name);
  if (!v) return { value: null, clear };
  return { value: await (await sealer(env, "login-flow")).open<T>(v, name), clear };
}
