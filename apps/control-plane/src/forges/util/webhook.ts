// Webhook helpers: signatures, body limits and JSON error bodies. Port of
// internal/forge/forgeutil/webhook.go. Dedupe moved to IngressDO (exact and
// durable), so the in-memory Dedupe of Go is not ported.
import { fromHex, hmacSha256, sha256, timingSafeEqual, toHex, utf8 } from "../../lib/bytes";

/** Largest inbound forge webhook body. */
export const MAX_WEBHOOK_BODY = 10 << 20;

/** Reads at most max bytes of a request body; throws when it is larger. */
export async function readBody(req: Request, max = MAX_WEBHOOK_BODY): Promise<Uint8Array<ArrayBuffer>> {
  const len = Number(req.headers.get("content-length") ?? "0");
  if (len > max) throw new Error("request body too large");
  const b = new Uint8Array(await req.arrayBuffer());
  if (b.length > max) throw new Error("request body too large");
  return b;
}

/** The standard JSON error envelope {"error":{"code","message"}}. */
export function errorJSON(status: number, code: string, message: string, headers?: Record<string, string>): Response {
  return jsonResponse(status, { error: { code, message } }, headers);
}

export function jsonResponse(status: number, v: unknown, headers?: Record<string, string>): Response {
  return new Response(JSON.stringify(v) + "\n", {
    status,
    headers: { "content-type": "application/json", ...(headers ?? {}) },
  });
}

/** hex(HMAC-SHA256(secret, data)). */
export async function hmacSha256Hex(secret: Uint8Array, data: Uint8Array | string): Promise<string> {
  return toHex(await hmacSha256(secret, data));
}

/** Checks a hex HMAC-SHA256 signature; an optional "sha256=" prefix is stripped. */
export async function verifyHmacSha256Hex(
  secret: Uint8Array,
  body: Uint8Array | string,
  sig: string,
): Promise<boolean> {
  if (secret.length === 0) return false;
  let s = sig.trim();
  if (s.startsWith("sha256=")) s = s.slice(7);
  let got: Uint8Array;
  try {
    got = fromHex(s);
  } catch {
    return false;
  }
  return timingSafeEqual(await hmacSha256(secret, body), got);
}

/** Constant-time comparison of two secrets (SHA-256 digests hide the lengths). Empty never matches. */
export async function constantTimeEqual(a: string | Uint8Array, b: string | Uint8Array): Promise<boolean> {
  const x = typeof a === "string" ? utf8(a) : a;
  const y = typeof b === "string" ? utf8(b) : b;
  if (x.length === 0 || y.length === 0) return false;
  return timingSafeEqual(await sha256(x), await sha256(y));
}
