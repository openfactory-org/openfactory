// Byte helpers on WebCrypto: UTF-8, hex, base64 variants (Go encoding/base64
// Std, RawStd, URL, RawURL), SHA-256, HMAC-SHA256 and constant-time compare.
// They run in Workers, browsers and Node.js (global crypto).

const enc = new TextEncoder();
const dec = new TextDecoder();

type Bytes = Uint8Array<ArrayBuffer>;

/** UTF-8 bytes of s. */
export const utf8 = (s: string): Bytes => enc.encode(s) as Bytes;

/** UTF-8 text of b. Invalid sequences become U+FFFD. */
export const fromUtf8 = (b: Uint8Array): string => dec.decode(b);

const asBytes = (data: Uint8Array | string): Bytes => (typeof data === "string" ? utf8(data) : (data as Bytes));

export function concat(...parts: Uint8Array[]): Bytes {
  const out = new Uint8Array(parts.reduce((n, p) => n + p.length, 0));
  let o = 0;
  for (const p of parts) {
    out.set(p, o);
    o += p.length;
  }
  return out;
}

const HEX = "0123456789abcdef";

/** Lower-case hex of bytes. */
export function toHex(b: ArrayBuffer | Uint8Array): string {
  const u = b instanceof Uint8Array ? b : new Uint8Array(b);
  let s = "";
  for (const x of u) s += HEX[x >> 4]! + HEX[x & 15]!;
  return s;
}

/** Decodes hex (either case); throws on bad input. */
export function fromHex(s: string): Bytes {
  if (s.length % 2 !== 0 || !/^[0-9a-fA-F]*$/.test(s)) throw new Error("invalid hex");
  const out = new Uint8Array(s.length / 2);
  for (let i = 0; i < out.length; i++) out[i] = parseInt(s.slice(i * 2, i * 2 + 2), 16);
  return out;
}

function bin(b: Uint8Array): string {
  let s = "";
  for (let i = 0; i < b.length; i += 0x8000) s += String.fromCharCode(...b.subarray(i, i + 0x8000));
  return s;
}

/** Standard base64 with padding (StdEncoding). */
export function b64(b: Uint8Array): string {
  return btoa(bin(b));
}

/** base64url without padding (RawURLEncoding). */
export function b64url(b: Uint8Array): string {
  return b64(b).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

// Like Go's default (non-Strict) decoders, non-zero trailing bits are accepted.
function decode(s: string, alphabet: RegExp, padded: boolean, url: boolean): Bytes | null {
  if (!alphabet.test(s)) return null;
  if (padded && s.length % 4 !== 0) return null;
  if (!padded && (s.includes("=") || s.length % 4 === 1)) return null;
  let t = url ? s.replace(/-/g, "+").replace(/_/g, "/") : s;
  if (!padded) t += "=".repeat((4 - (t.length % 4)) % 4);
  try {
    const raw = atob(t);
    const out = new Uint8Array(raw.length);
    for (let i = 0; i < raw.length; i++) out[i] = raw.charCodeAt(i);
    return out;
  } catch {
    return null;
  }
}

const STD = /^[A-Za-z0-9+/]*={0,2}$/;
const RAW_STD = /^[A-Za-z0-9+/]*$/;
const URL_PADDED = /^[A-Za-z0-9_-]*={0,2}$/;
const RAW_URL = /^[A-Za-z0-9_-]*$/;

/** Decodes base64url without padding; null on bad input. */
export function tryFromB64url(s: string): Bytes | null {
  return decode(s, RAW_URL, false, true);
}

/** Decodes base64url without padding; throws on bad input. */
export function fromB64url(s: string): Bytes {
  const out = tryFromB64url(s);
  if (!out) throw new Error("invalid base64url");
  return out;
}

/** Decodes standard base64 with padding; throws on bad input. */
export function fromB64(s: string): Bytes {
  const out = decode(s, STD, true, false);
  if (!out) throw new Error("invalid base64");
  return out;
}

/** Tries Std, RawStd, URL and RawURL in that order, like secrets.LoadKey; null when none decodes. */
export function fromAnyB64(s: string): Bytes | null {
  return (
    decode(s, STD, true, false) ??
    decode(s, RAW_STD, false, false) ??
    decode(s, URL_PADDED, true, true) ??
    decode(s, RAW_URL, false, true)
  );
}

/** SHA-256 of a string (UTF-8) or bytes. */
export async function sha256(data: Uint8Array | string): Promise<Bytes> {
  return new Uint8Array(await crypto.subtle.digest("SHA-256", asBytes(data)));
}

/** SHA-256 of a string (UTF-8) or bytes, as lower-case hex. */
export async function sha256Hex(data: Uint8Array | string): Promise<string> {
  return toHex(await sha256(data));
}

/** HMAC-SHA256 of data. A string key or data is UTF-8. */
export async function hmacSha256(key: Uint8Array | string, data: Uint8Array | string): Promise<Bytes> {
  const k = await crypto.subtle.importKey("raw", asBytes(key), { name: "HMAC", hash: "SHA-256" }, false, ["sign"]);
  return new Uint8Array(await crypto.subtle.sign("HMAC", k, asBytes(data)));
}

/** Lower-case hex of HMAC-SHA256(key, data). */
export async function hmacSha256Hex(key: Uint8Array | string, data: Uint8Array | string): Promise<string> {
  return toHex(await hmacSha256(key, data));
}

/**
 * Constant-time equality of two byte strings. Different lengths are not
 * equal, and the length check returns early: use constantTimeEqual for
 * secrets whose length must not leak.
 */
export function timingSafeEqual(a: Uint8Array | string, b: Uint8Array | string): boolean {
  const x = asBytes(a);
  const y = asBytes(b);
  if (x.length !== y.length) return false;
  const subtle = crypto.subtle as SubtleCrypto & {
    timingSafeEqual?: (a: ArrayBufferView, b: ArrayBufferView) => boolean;
  };
  if (typeof subtle.timingSafeEqual === "function") return subtle.timingSafeEqual(x, y);
  let r = 0;
  for (let i = 0; i < x.length; i++) r |= x[i]! ^ y[i]!;
  return r === 0;
}

/**
 * Constant-time compare of two secrets. Empty values never match. Both values
 * are padded to the same length before the compare, so the time does not
 * depend on where they differ or on whether their lengths differ.
 */
export function constantTimeEqual(a: Uint8Array | string, b: Uint8Array | string): boolean {
  const x = asBytes(a);
  const y = asBytes(b);
  if (x.length === 0 || y.length === 0) return false;
  const n = Math.max(x.length, y.length);
  const px = new Uint8Array(n);
  const py = new Uint8Array(n);
  px.set(x);
  py.set(y);
  const same = timingSafeEqual(px, py);
  return same && x.length === y.length;
}

/**
 * Checks a hex HMAC-SHA256 signature of body (either case; an optional
 * "sha256=" prefix is stripped). An empty secret never verifies.
 */
export async function verifyHmacSha256Hex(
  secret: Uint8Array | string,
  body: Uint8Array | string,
  sig: string,
): Promise<boolean> {
  if (secret.length === 0) return false;
  let got: Uint8Array;
  try {
    got = fromHex(sig.trim().replace(/^sha256=/, ""));
  } catch {
    return false;
  }
  return timingSafeEqual(await hmacSha256(secret, body), got);
}

export function randomBytes(n: number): Bytes {
  const b = new Uint8Array(n);
  crypto.getRandomValues(b);
  return b;
}

/** base64url of n random bytes, like auth.randomToken. */
export function randomToken(n: number): string {
  return b64url(randomBytes(n));
}
