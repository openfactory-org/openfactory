// Compact JWT parsing and verification with WebCrypto (port of forgeutil/jwt.go).
import { hmacSha256, bytesEqual } from "./util";

const enc = new TextEncoder();
const dec = new TextDecoder();

export function b64urlEncode(b: Uint8Array | string): string {
  const u = typeof b === "string" ? enc.encode(b) : b;
  let s = "";
  for (const x of u) s += String.fromCharCode(x);
  return btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

export function b64urlDecode(s: string): Uint8Array {
  const t = s.replace(/=+$/, "");
  if (!/^[A-Za-z0-9_-]*$/.test(t)) throw new Error("bad base64url");
  const pad = t.length % 4 === 0 ? "" : "=".repeat(4 - (t.length % 4));
  const bin = atob(t.replace(/-/g, "+").replace(/_/g, "/") + pad);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

export class Jwt {
  constructor(
    readonly header: Record<string, unknown>,
    readonly claims: Record<string, unknown>,
    private readonly signing: string,
    private readonly signature: Uint8Array,
  ) {}

  headerString(k: string): string {
    const v = this.header[k];
    return typeof v === "string" ? v : "";
  }

  claimString(k: string): string {
    const v = this.claims[k];
    return typeof v === "string" ? v : "";
  }

  /** Checks an HMAC-SHA256 signature. */
  async verifyHS256(secret: string | Uint8Array): Promise<void> {
    if (this.headerString("alg") !== "HS256") throw new Error(`jwt: unexpected alg "${this.headerString("alg")}"`);
    if (!bytesEqual(await hmacSha256(secret, this.signing), this.signature)) throw new Error("jwt: invalid signature");
  }

  /** Checks the signature against an RSA public key. */
  async verifyRS256(pub: CryptoKey): Promise<void> {
    if (this.headerString("alg") !== "RS256") throw new Error(`jwt: unexpected alg "${this.headerString("alg")}"`);
    const ok = await crypto.subtle.verify("RSASSA-PKCS1-v1_5", pub, this.signature, enc.encode(this.signing));
    if (!ok) throw new Error("jwt: invalid signature");
  }

  /** Checks exp and nbf with a clock skew. */
  verifyTime(nowMs: number, skewMs: number): void {
    const num = (k: string): number | undefined => {
      const v = this.claims[k];
      return typeof v === "number" ? Math.trunc(v) : undefined;
    };
    const exp = num("exp");
    if (exp === undefined) throw new Error("jwt: missing exp");
    if (nowMs > exp * 1000 + skewMs) throw new Error("jwt: token expired");
    const nbf = num("nbf");
    if (nbf !== undefined && nowMs + skewMs < nbf * 1000) throw new Error("jwt: token not yet valid");
  }

  /** Reports whether the aud claim (string or array) contains aud. */
  hasAudience(aud: string): boolean {
    const v = this.claims["aud"];
    if (typeof v === "string") return v === aud;
    if (Array.isArray(v)) return v.some((a) => a === aud);
    return false;
  }
}

export function parseJwt(tok: string): Jwt {
  const parts = tok.split(".");
  if (parts.length !== 3) throw new Error("jwt: malformed token");
  try {
    const header = JSON.parse(dec.decode(b64urlDecode(parts[0]!))) as Record<string, unknown>;
    const claims = JSON.parse(dec.decode(b64urlDecode(parts[1]!))) as Record<string, unknown>;
    return new Jwt(header, claims, parts[0] + "." + parts[1], b64urlDecode(parts[2]!));
  } catch (e) {
    throw new Error(`jwt: ${(e as Error).message}`);
  }
}

export async function signHS256(secret: string | Uint8Array, claims: unknown): Promise<string> {
  const signing = b64urlEncode(JSON.stringify({ alg: "HS256", typ: "JWT" })) + "." + b64urlEncode(JSON.stringify(claims));
  return signing + "." + b64urlEncode(await hmacSha256(secret, signing));
}

export async function signRS256(key: CryptoKey, header: Record<string, unknown>, claims: unknown): Promise<string> {
  const signing =
    b64urlEncode(JSON.stringify({ ...header, alg: "RS256", typ: "JWT" })) + "." + b64urlEncode(JSON.stringify(claims));
  const sig = new Uint8Array(await crypto.subtle.sign("RSASSA-PKCS1-v1_5", key, enc.encode(signing)));
  return signing + "." + b64urlEncode(sig);
}

export interface Jwk {
  kty?: string;
  kid: string;
  use?: string;
  n: string;
  e: string;
  endorsements?: string[];
}

/** Imports an RSA JWK as a verification key. */
export async function importRsaJwk(k: Jwk): Promise<CryptoKey> {
  if (k.kty && k.kty !== "RSA") throw new Error(`jwk ${k.kid}: unsupported kty ${k.kty}`);
  return crypto.subtle.importKey(
    "jwk",
    { kty: "RSA", n: k.n.replace(/=+$/, ""), e: k.e.replace(/=+$/, ""), alg: "RS256", ext: true },
    { name: "RSASSA-PKCS1-v1_5", hash: "SHA-256" },
    false,
    ["verify"],
  );
}
