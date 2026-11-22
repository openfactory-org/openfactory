// Worker access token (prefix ofw_): a short-lived bearer token bound to one
// worker, for the worker's REST calls (GET /workers/{id}/git-credential).
// Stateless: base64url(JSON {t, w, e}) "." base64url(HMAC-SHA256). The owner
// DO checks the worker state (revoked) on each call.
import type { Env } from "../../contracts/env";
import { derivedSigningKeys } from "../../config";
import { b64url, fromB64url, fromUtf8, hmacSha256, timingSafeEqual, utf8 } from "../../lib/bytes";

export const WORKER_TOKEN_PREFIX = "ofw_";
export const WORKER_TOKEN_TTL_MS = 15 * 60_000;
const LABEL = "openfactory/worker-token-key";

interface Claims {
  t: string;
  w: string;
  e: number;
}

export async function mintWorkerToken(
  env: Env,
  teamId: string,
  workerId: string,
  nowMs = Date.now(),
): Promise<{ token: string; expiresAt: number }> {
  const keys = await derivedSigningKeys(env, LABEL);
  const e = nowMs + WORKER_TOKEN_TTL_MS;
  const body = b64url(utf8(JSON.stringify({ t: teamId, w: workerId, e } satisfies Claims)));
  const mac = b64url(await hmacSha256(keys.current, body));
  return { token: `${WORKER_TOKEN_PREFIX}${body}.${mac}`, expiresAt: e };
}

/** The team and worker of a valid, unexpired token, or null. */
export async function verifyWorkerToken(
  env: Env,
  token: string,
  nowMs = Date.now(),
): Promise<{ teamId: string; workerId: string } | null> {
  if (!token.startsWith(WORKER_TOKEN_PREFIX)) return null;
  const rest = token.slice(WORKER_TOKEN_PREFIX.length);
  const i = rest.indexOf(".");
  if (i <= 0) return null;
  const body = rest.slice(0, i);
  let mac: Uint8Array;
  try {
    mac = fromB64url(rest.slice(i + 1));
  } catch {
    return null;
  }
  const keys = await derivedSigningKeys(env, LABEL);
  let ok = false;
  for (const k of [keys.current, ...keys.previous]) {
    if (timingSafeEqual(await hmacSha256(k, body), mac)) {
      ok = true;
      break;
    }
  }
  if (!ok) return null;
  let c: Claims;
  try {
    c = JSON.parse(fromUtf8(fromB64url(body))) as Claims;
  } catch {
    return null;
  }
  if (typeof c.t !== "string" || typeof c.w !== "string" || typeof c.e !== "number" || c.e <= nowMs) return null;
  return { teamId: c.t, workerId: c.w };
}
