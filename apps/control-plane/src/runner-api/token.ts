// Run session tokens (internal/runnertoken/session.go):
//   ofs_ + base64url(JSON claims) + "." + base64url(HMAC-SHA256(key[kid], payloadB64))
// The key is HMAC-SHA256(OF_SIGNING_KEY, "openfactory/runner-token-key"); kid is
// hex(SHA-256(key)[:4]). TTL 1 h, clock skew 60 s, max length 4096. Verify
// against the received payload bytes, never re-encode.

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

const enc = new TextEncoder();

function b64url(bytes: Uint8Array): string {
  let s = "";
  for (const b of bytes) s += String.fromCharCode(b);
  return btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

function unb64url(s: string): Uint8Array | undefined {
  if (!/^[A-Za-z0-9_-]*$/.test(s)) return undefined;
  try {
    const bin = atob(s.replace(/-/g, "+").replace(/_/g, "/") + "===".slice((s.length + 3) % 4));
    return Uint8Array.from(bin, (c) => c.charCodeAt(0));
  } catch {
    return undefined;
  }
}

async function hmac(key: Uint8Array, data: string): Promise<Uint8Array> {
  const k = await crypto.subtle.importKey("raw", key, { name: "HMAC", hash: "SHA-256" }, false, ["sign"]);
  return new Uint8Array(await crypto.subtle.sign("HMAC", k, enc.encode(data)));
}

export interface RunnerKey {
  kid: string;
  key: Uint8Array;
}

/** Derives the runner token key from a signing key secret. */
export async function deriveRunnerKey(signingKey: string): Promise<RunnerKey> {
  const key = await hmac(enc.encode(signingKey), LABEL);
  const sum = new Uint8Array(await crypto.subtle.digest("SHA-256", key));
  const kid = [...sum.slice(0, 4)].map((b) => b.toString(16).padStart(2, "0")).join("");
  return { kid, key };
}

export async function runnerKeys(env: { OF_SIGNING_KEY: string; OF_SIGNING_KEY_PREVIOUS?: string }): Promise<RunnerKey[]> {
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
  const claims: SessionClaims = { v: 1, kid: k.kid, team: c.team, run: c.run, ses: c.ses, sub: c.sub, iat, exp: iat + SESSION_TTL_S };
  const p = b64url(enc.encode(JSON.stringify(claims)));
  const s = b64url(await hmac(k.key, p));
  return { token: SESSION_PREFIX + p + "." + s, expiresAt: new Date(claims.exp * 1000).toISOString() };
}

/** Verifies a session token. Returns undefined for any failure. */
export async function verifySessionToken(keys: RunnerKey[], token: string, nowMs = Date.now()): Promise<SessionClaims | undefined> {
  if (token.length > MAX_LEN || !token.startsWith(SESSION_PREFIX)) return undefined;
  const rest = token.slice(SESSION_PREFIX.length);
  const dot = rest.indexOf(".");
  if (dot <= 0) return undefined;
  const p = rest.slice(0, dot);
  const s = rest.slice(dot + 1);
  if (!s || s.includes(".")) return undefined;
  const raw = unb64url(p);
  const sig = unb64url(s);
  if (!raw || !sig) return undefined;
  let c: SessionClaims;
  try {
    c = JSON.parse(new TextDecoder().decode(raw)) as SessionClaims;
  } catch {
    return undefined;
  }
  if (c.v !== 1) return undefined;
  const k = keys.find((x) => x.kid === c.kid);
  if (!k) return undefined;
  const want = await hmac(k.key, p);
  if (want.length !== sig.length) return undefined;
  let diff = 0;
  for (let i = 0; i < want.length; i++) diff |= want[i]! ^ sig[i]!;
  if (diff !== 0) return undefined;
  const now = Math.floor(nowMs / 1000);
  if (now > c.exp + SKEW_S || now + SKEW_S < c.iat) return undefined;
  if (!c.team || !c.run || !(c.ses >= 1)) return undefined;
  return c;
}

export async function sha256Hex(s: string): Promise<string> {
  const d = await crypto.subtle.digest("SHA-256", enc.encode(s));
  return [...new Uint8Array(d)].map((b) => b.toString(16).padStart(2, "0")).join("");
}
