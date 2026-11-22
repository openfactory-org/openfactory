// JWT and key helpers. Port of internal/forge/forgeutil/jwt.go with WebCrypto.
// WebCrypto imports only PKCS#8 private keys, so a PEM PKCS#1 RSA key
// ("BEGIN RSA PRIVATE KEY", the format of GitHub App keys) is wrapped in a
// PKCS#8 PrivateKeyInfo before the import.
import { b64url, fromB64, fromB64url, hmacSha256, timingSafeEqual, utf8 } from "../../lib/bytes";

const RS256 = { name: "RSASSA-PKCS1-v1_5", hash: "SHA-256" } as const;

/** DER length bytes. */
function derLength(n: number): Uint8Array {
  if (n < 0x80) return Uint8Array.of(n);
  const bytes: number[] = [];
  while (n > 0) {
    bytes.unshift(n & 0xff);
    n >>>= 8;
  }
  return Uint8Array.of(0x80 | bytes.length, ...bytes);
}

function der(tag: number, content: Uint8Array): Uint8Array {
  const len = derLength(content.length);
  const out = new Uint8Array(1 + len.length + content.length);
  out[0] = tag;
  out.set(len, 1);
  out.set(content, 1 + len.length);
  return out;
}

// version INTEGER 0, AlgorithmIdentifier { rsaEncryption, NULL }
const PKCS8_PREFIX = Uint8Array.of(
  0x02,
  0x01,
  0x00,
  0x30,
  0x0d,
  0x06,
  0x09,
  0x2a,
  0x86,
  0x48,
  0x86,
  0xf7,
  0x0d,
  0x01,
  0x01,
  0x01,
  0x05,
  0x00,
);

/** Wraps a PKCS#1 RSAPrivateKey DER in a PKCS#8 PrivateKeyInfo DER. */
export function pkcs1ToPkcs8(pkcs1: Uint8Array): Uint8Array<ArrayBuffer> {
  const octet = der(0x04, pkcs1);
  const body = new Uint8Array(PKCS8_PREFIX.length + octet.length);
  body.set(PKCS8_PREFIX);
  body.set(octet, PKCS8_PREFIX.length);
  return der(0x30, body) as Uint8Array<ArrayBuffer>;
}

/** The first PEM block of text: its type and DER bytes. */
export function decodePEM(text: string): { type: string; der: Uint8Array<ArrayBuffer> } | null {
  const m = /-----BEGIN ([A-Z0-9 ]+)-----([\s\S]*?)-----END \1-----/.exec(text);
  if (!m) return null;
  try {
    return { type: m[1]!, der: fromB64(m[2]!.replace(/\s+/g, "")) };
  } catch {
    return null;
  }
}

/** Imports a PEM PKCS#1 or PKCS#8 RSA private key for RS256 signing. */
export async function parseRSAPrivateKey(pem: string): Promise<CryptoKey> {
  const block = decodePEM(pem);
  if (!block) throw new Error("private key: no PEM block found");
  const tryImport = (d: Uint8Array<ArrayBuffer>) => crypto.subtle.importKey("pkcs8", d, RS256, false, ["sign"]);
  if (block.type === "RSA PRIVATE KEY") {
    try {
      return await tryImport(pkcs1ToPkcs8(block.der));
    } catch (e) {
      throw new Error(`private key: ${(e as Error).message}`, { cause: e });
    }
  }
  try {
    return await tryImport(block.der);
  } catch {
    // A PKCS#1 key in a block with another label.
    try {
      return await tryImport(pkcs1ToPkcs8(block.der));
    } catch (e) {
      throw new Error(`private key: ${(e as Error).message}`, { cause: e });
    }
  }
}

const jsonB64 = (v: unknown): string => b64url(utf8(JSON.stringify(v)));

/** A compact JWS signed with RS256. Header keys alg and typ are set. */
export async function signRS256(
  key: CryptoKey,
  claims: unknown,
  header: Record<string, unknown> = {},
): Promise<string> {
  const h = { ...header, alg: "RS256", typ: "JWT" };
  const signing = jsonB64(h) + "." + jsonB64(claims);
  const sig = new Uint8Array(await crypto.subtle.sign("RSASSA-PKCS1-v1_5", key, utf8(signing)));
  return signing + "." + b64url(sig);
}

/** A compact JWT signed with HMAC-SHA256. */
export async function signHS256(secret: Uint8Array, claims: unknown): Promise<string> {
  const signing = jsonB64({ alg: "HS256", typ: "JWT" }) + "." + jsonB64(claims);
  return signing + "." + b64url(await hmacSha256(secret, signing));
}

/** A parsed, not yet verified, compact JWT. */
export class JWT {
  private constructor(
    readonly header: Record<string, unknown>,
    readonly claims: Record<string, unknown>,
    private readonly signing: string,
    private readonly signature: Uint8Array<ArrayBuffer>,
  ) {}

  static parse(tok: string): JWT {
    const parts = tok.split(".");
    if (parts.length !== 3) throw new Error("jwt: malformed token");
    const dec = (s: string, what: string): Uint8Array<ArrayBuffer> => {
      try {
        return fromB64url(s);
      } catch (e) {
        throw new Error(`jwt ${what}: ${(e as Error).message}`, { cause: e });
      }
    };
    const json = (b: Uint8Array, what: string): Record<string, unknown> => {
      try {
        const v = JSON.parse(new TextDecoder().decode(b)) as unknown;
        if (typeof v !== "object" || v === null || Array.isArray(v)) throw new Error("not an object");
        return v as Record<string, unknown>;
      } catch (e) {
        throw new Error(`jwt ${what}: ${(e as Error).message}`, { cause: e });
      }
    };
    const h = json(dec(parts[0]!, "header"), "header");
    const c = json(dec(parts[1]!, "claims"), "claims");
    const sig = dec(parts[2]!, "signature");
    return new JWT(h, c, parts[0] + "." + parts[1], sig);
  }

  headerString(k: string): string {
    const v = this.header[k];
    return typeof v === "string" ? v : "";
  }

  claimString(k: string): string {
    const v = this.claims[k];
    return typeof v === "string" ? v : "";
  }

  async verifyRS256(pub: CryptoKey): Promise<void> {
    if (this.headerString("alg") !== "RS256") throw new Error(`jwt: unexpected alg "${this.headerString("alg")}"`);
    const ok = await crypto.subtle.verify("RSASSA-PKCS1-v1_5", pub, this.signature, utf8(this.signing));
    if (!ok) throw new Error("jwt: invalid signature");
  }

  async verifyHS256(secret: Uint8Array): Promise<void> {
    if (this.headerString("alg") !== "HS256") throw new Error(`jwt: unexpected alg "${this.headerString("alg")}"`);
    if (!timingSafeEqual(await hmacSha256(secret, this.signing), this.signature))
      throw new Error("jwt: invalid signature");
  }

  /** Checks exp and nbf with a clock skew (ms). */
  verifyTime(nowMs: number, skewMs: number): void {
    const num = (k: string): number | null => {
      const v = this.claims[k];
      return typeof v === "number" ? Math.trunc(v) : null;
    };
    const exp = num("exp");
    if (exp === null) throw new Error("jwt: missing exp");
    if (nowMs > exp * 1000 + skewMs) throw new Error("jwt: token expired");
    const nbf = num("nbf");
    if (nbf !== null && nowMs + skewMs < nbf * 1000) throw new Error("jwt: token not yet valid");
  }

  hasAudience(aud: string): boolean {
    const v = this.claims["aud"];
    if (typeof v === "string") return v === aud;
    return Array.isArray(v) && v.some((a) => a === aud);
  }
}

/** One RSA key of a JWKS document. */
export interface JWK {
  kty: string;
  kid: string;
  use?: string;
  n: string;
  e: string;
}

/** Imports a JWK as an RS256 verification key. */
export async function rsaPublicKey(k: JWK): Promise<CryptoKey> {
  if (k.kty && k.kty !== "RSA") throw new Error(`jwk ${k.kid}: unsupported kty ${k.kty}`);
  return crypto.subtle.importKey(
    "jwk",
    { kty: "RSA", n: k.n.replace(/=+$/, ""), e: k.e.replace(/=+$/, ""), alg: "RS256", ext: true },
    RS256,
    false,
    ["verify"],
  );
}
