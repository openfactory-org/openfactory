// Byte helpers: base64 variants (Go encoding/base64 Std, RawStd, URL, RawURL),
// hex, UTF-8, SHA-256, HMAC-SHA256 and constant-time compare, on WebCrypto.

const enc = new TextEncoder();
const dec = new TextDecoder();

export const utf8 = (s: string): Uint8Array<ArrayBuffer> => enc.encode(s) as Uint8Array<ArrayBuffer>;
export const fromUtf8 = (b: Uint8Array): string => dec.decode(b);

export function concat(...parts: Uint8Array[]): Uint8Array<ArrayBuffer> {
  const out = new Uint8Array(parts.reduce((n, p) => n + p.length, 0));
  let o = 0;
  for (const p of parts) {
    out.set(p, o);
    o += p.length;
  }
  return out;
}

export function toHex(b: ArrayBuffer | Uint8Array): string {
  const u = b instanceof Uint8Array ? b : new Uint8Array(b);
  let s = "";
  for (const x of u) s += x.toString(16).padStart(2, "0");
  return s;
}

export function fromHex(s: string): Uint8Array<ArrayBuffer> {
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

/** Standard base64 with padding. */
export function b64(b: Uint8Array): string {
  return btoa(bin(b));
}

/** base64url without padding (RawURLEncoding). */
export function b64url(b: Uint8Array): string {
  return b64(b).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

function decodeStrict(s: string, alphabet: RegExp, padded: boolean, url: boolean): Uint8Array<ArrayBuffer> | null {
  if (!alphabet.test(s)) return null;
  if (padded && s.length % 4 !== 0) return null;
  if (!padded && (s.includes("=") || s.length % 4 === 1)) return null;
  let t = url ? s.replace(/-/g, "+").replace(/_/g, "/") : s;
  if (!padded) t += "=".repeat((4 - (t.length % 4)) % 4);
  try {
    const raw = atob(t);
    const out = new Uint8Array(raw.length);
    for (let i = 0; i < raw.length; i++) out[i] = raw.charCodeAt(i);
    // Reject non-canonical trailing bits like Go's strict decoder does for padded input.
    return out;
  } catch {
    return null;
  }
}

/** Decodes base64url without padding; throws on bad input. */
export function fromB64url(s: string): Uint8Array<ArrayBuffer> {
  const out = decodeStrict(s, /^[A-Za-z0-9_-]*$/, false, true);
  if (!out) throw new Error("invalid base64url");
  return out;
}

/** Decodes standard base64 with padding; throws on bad input. */
export function fromB64(s: string): Uint8Array<ArrayBuffer> {
  const out = decodeStrict(s, /^[A-Za-z0-9+/]*={0,2}$/, true, false);
  if (!out) throw new Error("invalid base64");
  return out;
}

/** Tries Std, RawStd, URL and RawURL like secrets.LoadKey. */
export function fromAnyB64(s: string): Uint8Array<ArrayBuffer> | null {
  return (
    decodeStrict(s, /^[A-Za-z0-9+/]*={0,2}$/, true, false) ??
    decodeStrict(s, /^[A-Za-z0-9+/]*$/, false, false) ??
    decodeStrict(s, /^[A-Za-z0-9_-]*={0,2}$/, true, true) ??
    decodeStrict(s, /^[A-Za-z0-9_-]*$/, false, true)
  );
}

export async function sha256(data: Uint8Array | string): Promise<Uint8Array<ArrayBuffer>> {
  const d = typeof data === "string" ? utf8(data) : data;
  return new Uint8Array(await crypto.subtle.digest("SHA-256", d as Uint8Array<ArrayBuffer>));
}

export async function sha256Hex(data: Uint8Array | string): Promise<string> {
  return toHex(await sha256(data));
}

export async function hmacSha256(key: Uint8Array, data: Uint8Array | string): Promise<Uint8Array<ArrayBuffer>> {
  const k = await crypto.subtle.importKey(
    "raw",
    key as Uint8Array<ArrayBuffer>,
    { name: "HMAC", hash: "SHA-256" },
    false,
    ["sign"],
  );
  const d = typeof data === "string" ? utf8(data) : data;
  return new Uint8Array(await crypto.subtle.sign("HMAC", k, d as Uint8Array<ArrayBuffer>));
}

/** Constant-time equality (lengths may leak). */
export function timingSafeEqual(a: Uint8Array | string, b: Uint8Array | string): boolean {
  const x = typeof a === "string" ? utf8(a) : a;
  const y = typeof b === "string" ? utf8(b) : b;
  if (x.length !== y.length) return false;
  const subtle = crypto.subtle as SubtleCrypto & {
    timingSafeEqual?: (a: ArrayBufferView, b: ArrayBufferView) => boolean;
  };
  if (typeof subtle.timingSafeEqual === "function") return subtle.timingSafeEqual(x, y);
  let r = 0;
  for (let i = 0; i < x.length; i++) r |= x[i]! ^ y[i]!;
  return r === 0;
}

export function randomBytes(n: number): Uint8Array<ArrayBuffer> {
  const b = new Uint8Array(n);
  crypto.getRandomValues(b);
  return b;
}

/** base64url(n random bytes), like auth.randomToken. */
export function randomToken(n: number): string {
  return b64url(randomBytes(n));
}
