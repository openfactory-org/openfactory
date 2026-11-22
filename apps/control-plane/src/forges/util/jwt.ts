// RS256 signing with an RSA private key in PEM (the GitHub App JWT). Port of
// internal/forge/forgeutil/jwt.go. WebCrypto imports only PKCS#8 private
// keys, so a PEM PKCS#1 RSA key ("BEGIN RSA PRIVATE KEY", the format of
// GitHub App keys) is wrapped in a PKCS#8 PrivateKeyInfo before the import.
// Verification of tokens is in integrations/jwt.ts.
import { SignJWT } from "jose";
import { fromB64 } from "../../lib/bytes";

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

/** A compact JWT signed with RS256 (the GitHub App JWT). Header keys alg and typ are set. */
export async function signRS256(
  key: CryptoKey,
  claims: Record<string, unknown>,
  header: Record<string, unknown> = {},
): Promise<string> {
  return new SignJWT(claims).setProtectedHeader({ ...header, alg: "RS256", typ: "JWT" }).sign(key);
}
