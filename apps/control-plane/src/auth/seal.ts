// Sealed cookie values. Port of internal/auth/seal.go: AES-256-GCM with a
// subkey HMAC-SHA256(master, "openfactory/auth/" + purpose) per purpose, the
// cookie name as additional data, and an expiry envelope {"exp","d"}.
// Output: base64url(nonce12 || ciphertext || tag16), the layout of Go Seal.
import { b64url, fromB64url, fromUtf8, hmacSha256, randomBytes, utf8 } from "../lib/bytes";

export const SESSION_KEY_SIZE = 32;
const NONCE = 12;
const TAG = 16;

async function aesKey(master: Uint8Array, purpose: string): Promise<CryptoKey> {
  if (master.length !== SESSION_KEY_SIZE) {
    throw new Error(`auth: session key must be ${SESSION_KEY_SIZE} bytes, got ${master.length}`);
  }
  const sub = await hmacSha256(master, utf8("openfactory/auth/" + purpose));
  return crypto.subtle.importKey("raw", sub, "AES-GCM", false, ["encrypt", "decrypt"]);
}

export class Sealer {
  private constructor(
    private readonly key: CryptoKey,
    private readonly old: CryptoKey[],
  ) {}

  static async create(master: Uint8Array, purpose: string, previous: Uint8Array[] = []): Promise<Sealer> {
    return new Sealer(await aesKey(master, purpose), await Promise.all(previous.map((p) => aesKey(p, purpose))));
  }

  /** Seals v with expiry `expMs` (unix ms; stored as unix seconds). */
  async seal(v: unknown, aad: string, expMs: number): Promise<string> {
    // Go marshals {"exp":<int>,"d":<raw json>}.
    const plain = `{"exp":${Math.floor(expMs / 1000)},"d":${JSON.stringify(v)}}`;
    const nonce = randomBytes(NONCE);
    const ct = new Uint8Array(
      await crypto.subtle.encrypt({ name: "AES-GCM", iv: nonce, additionalData: utf8(aad) }, this.key, utf8(plain)),
    );
    const out = new Uint8Array(NONCE + ct.length);
    out.set(nonce);
    out.set(ct, NONCE);
    return b64url(out);
  }

  /** Opens a sealed value; null when malformed, tampered, sealed for another aad, or expired. */
  async open<T>(tok: string, aad: string, nowMs: number = Date.now()): Promise<T | null> {
    let raw: Uint8Array<ArrayBuffer>;
    try {
      raw = fromB64url(tok);
    } catch {
      return null;
    }
    if (raw.length < NONCE + TAG) return null;
    const iv = raw.subarray(0, NONCE);
    const body = raw.subarray(NONCE);
    let plain: ArrayBuffer | null = null;
    for (const k of [this.key, ...this.old]) {
      try {
        plain = await crypto.subtle.decrypt({ name: "AES-GCM", iv, additionalData: utf8(aad) }, k, body);
        break;
      } catch {
        /* next key */
      }
    }
    if (!plain) return null;
    try {
      const env = JSON.parse(fromUtf8(new Uint8Array(plain))) as { exp?: unknown; d?: unknown };
      if (typeof env.exp !== "number" || Math.floor(nowMs / 1000) >= env.exp) return null;
      return (env.d ?? null) as T;
    } catch {
      return null;
    }
  }
}
