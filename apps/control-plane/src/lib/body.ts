// Request body helpers for ops (port of decodeJSON's type errors) and
// byte-capped reads of request and response bodies.
import { OfError, type ErrorCode } from "@openfactory/core";

export type JsonObject = Record<string, unknown>;

/** The body as an object; {} for an absent body when optional. */
export function bodyObject(body: unknown, optional = false): JsonObject {
  if (body === undefined || body === null) {
    if (optional) return {};
    throw new OfError("invalid_request", "request body is required");
  }
  if (typeof body !== "object" || Array.isArray(body))
    throw new OfError("invalid_request", "malformed JSON body: expected an object");
  return body as JsonObject;
}

function typeErr(field: string, type: string): OfError {
  return new OfError("invalid_request", `field "${field}" must be of type ${type}`);
}

export function optString(o: JsonObject, k: string): string | undefined {
  const v = o[k];
  if (v === undefined || v === null) return undefined;
  if (typeof v !== "string") throw typeErr(k, "string");
  return v;
}

export function str(o: JsonObject, k: string): string {
  return optString(o, k) ?? "";
}

export function optBool(o: JsonObject, k: string): boolean | undefined {
  const v = o[k];
  if (v === undefined || v === null) return undefined;
  if (typeof v !== "boolean") throw typeErr(k, "bool");
  return v;
}

export function optStringArray(o: JsonObject, k: string): string[] | undefined {
  const v = o[k];
  if (v === undefined || v === null) return undefined;
  if (!Array.isArray(v) || !v.every((x) => typeof x === "string")) throw typeErr(k, "[]string");
  return v as string[];
}

export function optStringMap(o: JsonObject, k: string): Record<string, string> | undefined {
  const v = o[k];
  if (v === undefined || v === null) return undefined;
  if (typeof v !== "object" || Array.isArray(v) || !Object.values(v).every((x) => typeof x === "string")) {
    throw typeErr(k, "map[string]string");
  }
  return v as Record<string, string>;
}

/** Map of string or null values (for "null removes"). */
export function optNullableStringMap(o: JsonObject, k: string): Record<string, string | null> | undefined {
  const v = o[k];
  if (v === undefined || v === null) return undefined;
  if (
    typeof v !== "object" ||
    Array.isArray(v) ||
    !Object.values(v).every((x) => x === null || typeof x === "string")
  ) {
    throw typeErr(k, "map[string]*string");
  }
  return v as Record<string, string | null>;
}

export function now(): string {
  return new Date().toISOString();
}

/** What readUpTo read: at most max bytes, and whether the body had more. */
interface Capped {
  bytes: Uint8Array<ArrayBuffer>;
  overflow: boolean;
}

/** Reads at most max bytes of a body stream, then cancels the stream if more bytes remain. */
async function readUpTo(r: Request | Response, max: number): Promise<Capped> {
  if (!r.body) return { bytes: new Uint8Array(0), overflow: false };
  const reader = r.body.getReader();
  const chunks: Uint8Array[] = [];
  let total = 0;
  let overflow = false;
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      const room = max - total;
      if (value.length > room) {
        if (room > 0) chunks.push(value.subarray(0, room));
        total += Math.max(room, 0);
        overflow = true;
        break;
      }
      chunks.push(value);
      total += value.length;
    }
  } finally {
    if (overflow) await reader.cancel().catch(() => {});
    reader.releaseLock();
  }
  const bytes = new Uint8Array(total);
  let off = 0;
  for (const c of chunks) {
    bytes.set(c, off);
    off += c.length;
  }
  return { bytes, overflow };
}

/**
 * Reads a request or response body as bytes. It rejects a body longer than
 * max bytes with an OfError (code `code`, default invalid_request) and does
 * not buffer more than max bytes. A Content-Length above max fails before
 * the read.
 */
export async function readBytesCapped(
  r: Request | Response,
  max: number,
  what = "body",
  code: ErrorCode = "invalid_request",
): Promise<Uint8Array<ArrayBuffer>> {
  const fail = (): OfError => new OfError(code, `${what} too large: more than ${max} bytes`);
  const len = Number(r.headers.get("content-length") ?? "");
  if (Number.isFinite(len) && len > max) {
    await r.body?.cancel().catch(() => {});
    throw fail();
  }
  const { bytes, overflow } = await readUpTo(r, max);
  if (overflow) throw fail();
  return bytes;
}

/** readBytesCapped decoded as UTF-8 text. */
export async function readTextCapped(
  r: Request | Response,
  max: number,
  what = "body",
  code: ErrorCode = "invalid_request",
): Promise<string> {
  return new TextDecoder().decode(await readBytesCapped(r, max, what, code));
}

/** The first max bytes of a body as text; the rest is dropped. For error bodies quoted in messages. */
export async function readTextPrefix(r: Request | Response, max: number): Promise<string> {
  return new TextDecoder().decode((await readUpTo(r, max)).bytes);
}
