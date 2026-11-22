// Run tokens. Port of internal/runnertoken (session tokens, bootstrap
// hashes) and internal/runauth (the run-path check). The HMAC check runs in
// the Worker; the session state check (active, not superseded, touch) runs
// in WorkDO, which the caller passes as `checkSession`.
//
//   session token ofs_<base64url(JSON claims)>.<base64url(HMAC-SHA256(key[kid], payloadB64))>
//   claims {v:1, kid, team, run, ses, sub, iat, exp}; TTL 1 h; skew 60 s; max 4096 chars
//   kid = hex(SHA-256(derived runner key)[:4])
//   bootstrap ofb_<team>.<run>.<secret43> (new) or ofb_<base64url(32 bytes)> (Go); stored as hex(SHA-256(token))
import { OfError } from "@openfactory/core";
import type { Env } from "../contracts/env";
import { LABELS, derivedSigningKeys, keyVersion } from "../config";
import { b64url, fromB64url, fromUtf8, hmacSha256, sha256Hex, timingSafeEqual, utf8 } from "../lib/bytes";

export const SESSION_PREFIX = "ofs_";
export const SESSION_TTL_S = 3600;
export const SESSION_VERSION = 1;
const MAX_TOKEN_LEN = 4096;
const MIN_KEY_LEN = 32;
const CLOCK_SKEW_S = 60;

export interface SessionClaims {
  v: number;
  kid: string;
  team: string;
  run: string;
  ses: number;
  sub: string;
  iat: number;
  exp: number;
}

export type RunTokenErrorKind = "malformed" | "unknown_key" | "bad_signature" | "expired" | "not_yet_valid";

export class RunTokenError extends Error {
  constructor(
    readonly kind: RunTokenErrorKind,
    message: string,
  ) {
    super(`runnertoken: ${message}`);
  }
}

export interface KeySet {
  current: { kid: string; key: Uint8Array };
  lookup(kid: string): Uint8Array | undefined;
}

export function staticKeys(current: string, keys: Record<string, Uint8Array>): KeySet {
  if (!current) throw new Error("runnertoken: current key ID is empty");
  if (!keys[current]) throw new Error(`runnertoken: current key "${current}" is not in the key set`);
  for (const [kid, k] of Object.entries(keys)) {
    if (k.length < MIN_KEY_LEN) throw new Error(`runnertoken: key "${kid}" is shorter than ${MIN_KEY_LEN} bytes`);
  }
  return { current: { kid: current, key: keys[current]! }, lookup: (kid) => keys[kid] };
}

/** The runner key set of this deployment: derived keys of the signing keys (server/runnermode.go). */
export async function runnerKeys(env: Env): Promise<KeySet> {
  const ks = await derivedSigningKeys(env, LABELS.runnerToken);
  const keys: Record<string, Uint8Array> = {};
  const cur = await keyVersion(ks.current);
  keys[cur] = ks.current;
  for (const p of ks.previous) keys[await keyVersion(p)] = p;
  return staticKeys(cur, keys);
}

/** Mint: sets version, key ID and times; returns the token and its expiry (unix seconds). */
export async function mintSessionToken(
  keys: KeySet,
  c: { team: string; run: string; ses: number; sub: string },
  nowMs = Date.now(),
): Promise<{ token: string; exp: number }> {
  if (!c.team || !c.run || !c.sub) throw new Error("runnertoken: team, run and subject are required");
  if (c.ses < 1) throw new Error("runnertoken: session must be 1 or more");
  const now = Math.floor(nowMs / 1000);
  const claims: SessionClaims = {
    v: SESSION_VERSION,
    kid: keys.current.kid,
    team: c.team,
    run: c.run,
    ses: c.ses,
    sub: c.sub,
    iat: now,
    exp: now + SESSION_TTL_S,
  };
  const p = b64url(utf8(JSON.stringify(claims)));
  const s = b64url(await hmacSha256(keys.current.key, p));
  return { token: `${SESSION_PREFIX}${p}.${s}`, exp: claims.exp };
}

/** Verify: checks the token against the received payload bytes, never re-encoded. */
export async function verifySessionToken(keys: KeySet, token: string, nowMs = Date.now()): Promise<SessionClaims> {
  if (token.length > MAX_TOKEN_LEN) throw new RunTokenError("malformed", "malformed token: token too long");
  if (!token.startsWith(SESSION_PREFIX)) throw new RunTokenError("malformed", "malformed token: wrong prefix");
  const rest = token.slice(SESSION_PREFIX.length);
  const i = rest.indexOf(".");
  const p = i < 0 ? "" : rest.slice(0, i);
  const s = i < 0 ? "" : rest.slice(i + 1);
  if (i < 0 || p === "" || s === "" || s.includes(".")) throw new RunTokenError("malformed", "malformed token: wrong shape");
  let c: SessionClaims;
  try {
    c = JSON.parse(fromUtf8(fromB64url(p))) as SessionClaims;
  } catch {
    throw new RunTokenError("malformed", "malformed token: payload");
  }
  if (c.v !== SESSION_VERSION) throw new RunTokenError("malformed", `malformed token: unsupported version ${c.v}`);
  const key = keys.lookup(String(c.kid));
  if (!key) throw new RunTokenError("unknown_key", `unknown key: "${c.kid}"`);
  let sig: Uint8Array;
  try {
    sig = fromB64url(s);
  } catch {
    throw new RunTokenError("malformed", "malformed token: signature encoding");
  }
  if (!timingSafeEqual(sig, await hmacSha256(key, p))) throw new RunTokenError("bad_signature", "bad signature");
  if (!c.team || !c.run || !c.sub || !(c.ses >= 1)) throw new RunTokenError("malformed", "malformed token: missing claims");
  const n = Math.floor(nowMs / 1000);
  if (n >= c.exp) throw new RunTokenError("expired", "token expired");
  if (c.iat > n + CLOCK_SKEW_S) throw new RunTokenError("not_yet_valid", "token not yet valid");
  return c;
}

export const BOOTSTRAP_PREFIX = "ofb_";

/**
 * HashBootstrap: hex SHA-256 of the whole token. Accepts the new
 * ofb_<team>.<run>.<secret43> form and the Go form ofb_<base64url(32 bytes)>.
 */
export async function hashBootstrap(token: string): Promise<string> {
  if (!token.startsWith(BOOTSTRAP_PREFIX)) throw new RunTokenError("malformed", "malformed token: wrong prefix");
  const rest = token.slice(BOOTSTRAP_PREFIX.length);
  const parts = rest.split(".");
  if (parts.length === 3) {
    if (!parts[0] || !parts[1] || !/^[A-Za-z0-9_-]{43}$/.test(parts[2]!)) throw new RunTokenError("malformed", "malformed token: shape");
  } else {
    let b: Uint8Array;
    try {
      b = fromB64url(rest);
    } catch {
      throw new RunTokenError("malformed", "malformed token: encoding");
    }
    if (b.length !== 32) throw new RunTokenError("malformed", "malformed token: wrong length");
  }
  return sha256Hex(token);
}

export const hashInstanceId = (id: string): Promise<string> => sha256Hex(id);
export const equalHash = (a: string, b: string): boolean => timingSafeEqual(a, b);

// ---------- runauth ----------

export type SessionState = "active" | "ended" | "missing" | "superseded";

/**
 * Verify for a run path: the token must be a session token of `runId`, and
 * `checkSession` (WorkDO: GetRunSession, LatestRunSession, Touch) must say
 * "active". Throws OfError authentication_required or conflict (reason
 * session_superseded).
 */
export async function verifyRunToken(
  env: Env,
  runId: string,
  token: string,
  checkSession: (claims: SessionClaims) => Promise<SessionState>,
): Promise<SessionClaims> {
  const t = token.trim();
  if (!t || !runId || !t.startsWith(SESSION_PREFIX)) throw runUnauthorized();
  let c: SessionClaims;
  try {
    c = await verifySessionToken(await runnerKeys(env), t);
  } catch {
    throw runUnauthorized();
  }
  if (c.run !== runId) throw runUnauthorized();
  const st = await checkSession(c);
  if (st === "superseded") throw new OfError("conflict", "a newer session exists", { reason: "session_superseded" });
  if (st !== "active") throw runUnauthorized();
  return c;
}

function runUnauthorized(): OfError {
  return new OfError("authentication_required", "invalid or missing run token", { reason: "unauthorized" });
}

/** runauth.WriteError: the plain JSON answer of the run paths. */
export function runAuthErrorResponse(e: unknown): Response {
  const err = e instanceof OfError ? e : undefined;
  const body = (code: string, message: string) => JSON.stringify({ error: { code, message } }) + "\n";
  if (err?.code === "authentication_required") {
    return new Response(body("unauthorized", "invalid or missing run token"), {
      status: 401,
      headers: { "content-type": "application/json", "www-authenticate": 'Bearer realm="openfactory-run"' },
    });
  }
  if (err?.code === "conflict" && err.reason === "session_superseded") {
    return new Response(body("session_superseded", "a newer session exists"), { status: 409, headers: { "content-type": "application/json" } });
  }
  return new Response(body("internal", "could not check the token"), { status: 500, headers: { "content-type": "application/json" } });
}
