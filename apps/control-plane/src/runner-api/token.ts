// Run session tokens (internal/runnertoken/session.go):
//   ofs_ + base64url(JSON claims) + "." + base64url(HMAC-SHA256(key[kid], payloadB64))
// The key is HMAC-SHA256(OF_SIGNING_KEY, "openfactory/runner-token-key"); kid is
// hex(SHA-256(key)[:4]). TTL 1 h, clock skew 60 s, max length 4096. Verify
// against the received payload bytes, never re-encode.

import { b64url, fromUtf8, hmacSha256, sha256, timingSafeEqual, toHex, tryFromB64url, utf8 } from "../lib/bytes";

export { sha256Hex } from "../lib/bytes";

export const SESSION_PREFIX = "ofs_";
export const SESSION_TTL_S = 3600;
const MAX_LEN = 4096;
const SKEW_S = 60;
const LABEL = "openfactory/runner-token-key";

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

export interface RunnerKey {
  kid: string;
  key: Uint8Array;
}

/** Derives the runner token key from a signing key secret. */
export async function deriveRunnerKey(signingKey: string): Promise<RunnerKey> {
  const key = await hmacSha256(signingKey, LABEL);
  const kid = toHex((await sha256(key)).subarray(0, 4));
  return { kid, key };
}

export async function runnerKeys(env: {
  OF_SIGNING_KEY: string;
  OF_SIGNING_KEY_PREVIOUS?: string;
}): Promise<RunnerKey[]> {
  const out = [await deriveRunnerKey(env.OF_SIGNING_KEY)];
  if (env.OF_SIGNING_KEY_PREVIOUS) out.push(await deriveRunnerKey(env.OF_SIGNING_KEY_PREVIOUS));
  return out;
}

export async function mintSessionToken(
  keys: RunnerKey[],
  c: { team: string; run: string; ses: number; sub: string },
  nowMs = Date.now(),
): Promise<{ token: string; expiresAt: string }> {
  const k = keys[0]!;
  const iat = Math.floor(nowMs / 1000);
  // Field order matches the Go struct (v, kid, team, run, ses, sub, iat, exp).
  const claims: SessionClaims = {
    v: 1,
    kid: k.kid,
    team: c.team,
    run: c.run,
    ses: c.ses,
    sub: c.sub,
    iat,
    exp: iat + SESSION_TTL_S,
  };
  const p = b64url(utf8(JSON.stringify(claims)));
  const s = b64url(await hmacSha256(k.key, p));
  return { token: SESSION_PREFIX + p + "." + s, expiresAt: new Date(claims.exp * 1000).toISOString() };
}

/** Verifies a session token. Returns undefined for any failure. */
export async function verifySessionToken(
  keys: RunnerKey[],
  token: string,
  nowMs = Date.now(),
): Promise<SessionClaims | undefined> {
  if (token.length > MAX_LEN || !token.startsWith(SESSION_PREFIX)) return undefined;
  const rest = token.slice(SESSION_PREFIX.length);
  const dot = rest.indexOf(".");
  if (dot <= 0) return undefined;
  const p = rest.slice(0, dot);
  const s = rest.slice(dot + 1);
  if (!s || s.includes(".")) return undefined;
  const raw = tryFromB64url(p);
  const sig = tryFromB64url(s);
  if (!raw || !sig) return undefined;
  let c: SessionClaims;
  try {
    c = JSON.parse(fromUtf8(raw)) as SessionClaims;
  } catch {
    return undefined;
  }
  if (c.v !== 1) return undefined;
  const k = keys.find((x) => x.kid === c.kid);
  if (!k) return undefined;
  if (!timingSafeEqual(await hmacSha256(k.key, p), sig)) return undefined;
  const now = Math.floor(nowMs / 1000);
  if (now > c.exp + SKEW_S || now + SKEW_S < c.iat) return undefined;
  if (!c.team || !c.run || !(c.ses >= 1)) return undefined;
  return c;
}
