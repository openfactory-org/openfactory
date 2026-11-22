// JWT verification for integration webhooks (port of forgeutil/jwt.go), with jose.
import { decodeJwt, decodeProtectedHeader, errors, importJWK, jwtVerify } from "jose";

export type JwtFields = Record<string, unknown>;

/** A string member of a JWT header or claims set; "" when it is absent or not a string. */
export function claimString(fields: JwtFields, k: string): string {
  const v = fields[k];
  return typeof v === "string" ? v : "";
}

/** The header and claims of a compact JWT, without verification. Throws "jwt: ..." for a malformed token. */
export function decodeUnverified(tok: string): { header: JwtFields; claims: JwtFields } {
  try {
    return { header: decodeProtectedHeader(tok), claims: decodeJwt(tok) };
  } catch (e) {
    throw new Error(`jwt: ${(e as Error).message}`, { cause: e });
  }
}

export interface VerifyJwtOptions {
  /** The only accepted signature algorithm. */
  alg: "HS256" | "RS256";
  nowMs: number;
  /** Clock skew allowed for exp and nbf. */
  skewMs: number;
  /** The required iss claim. */
  issuer?: string;
  /** The aud claim (a string or an array) must contain one of these. */
  audience?: string[];
}

/**
 * Verifies a compact JWT: the signature with key and only the algorithm
 * alg, a required exp and an optional nbf (with the clock skew), and the
 * issuer and audience when given. Returns the claims. Errors start with "jwt: ".
 */
export async function verifyJwt(
  tok: string,
  key: CryptoKey | Uint8Array | string,
  o: VerifyJwtOptions,
): Promise<JwtFields> {
  const alg = claimString(decodeUnverified(tok).header, "alg");
  if (alg !== o.alg) throw new Error(`jwt: unexpected alg "${alg}"`);
  try {
    const { payload } = await jwtVerify(tok, typeof key === "string" ? new TextEncoder().encode(key) : key, {
      algorithms: [o.alg],
      requiredClaims: ["exp"],
      clockTolerance: o.skewMs / 1000,
      currentDate: new Date(o.nowMs),
      ...(o.issuer === undefined ? {} : { issuer: o.issuer }),
      ...(o.audience === undefined ? {} : { audience: o.audience }),
    });
    return payload;
  } catch (e) {
    throw jwtError(e);
  }
}

/** Maps a jose error to the stable messages of the Go port. */
function jwtError(e: unknown): Error {
  let msg = `jwt: ${(e as Error).message}`;
  if (e instanceof errors.JWTExpired) {
    msg = "jwt: token expired";
  } else if (e instanceof errors.JWSSignatureVerificationFailed) {
    msg = "jwt: invalid signature";
  } else if (e instanceof errors.JWTClaimValidationFailed) {
    if (e.claim === "exp") msg = "jwt: missing exp";
    else if (e.claim === "nbf" && e.reason === "check_failed") msg = "jwt: token not yet valid";
    else if (e.claim === "iss") msg = `jwt: unexpected issuer "${claimString(e.payload, "iss")}"`;
    else if (e.claim === "aud") msg = "jwt: token audience does not match";
  }
  return new Error(msg, { cause: e });
}

/** One RSA key of a JWKS document. */
export interface Jwk {
  kty?: string;
  kid: string;
  use?: string;
  n: string;
  e: string;
  endorsements?: string[];
}

/** Imports an RSA JWK as an RS256 verification key. */
export async function importRsaJwk(k: Jwk): Promise<CryptoKey> {
  if (k.kty && k.kty !== "RSA") throw new Error(`jwk ${k.kid}: unsupported kty ${k.kty}`);
  const key = await importJWK({ kty: "RSA", n: k.n.replace(/=+$/, ""), e: k.e.replace(/=+$/, "") }, "RS256");
  if (key instanceof Uint8Array) throw new Error(`jwk ${k.kid}: not an RSA key`);
  return key;
}
