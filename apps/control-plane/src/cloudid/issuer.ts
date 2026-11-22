// Run OIDC issuer (cloud identity). Port of the issuer half of
// internal/cloudid/cloudid.go: ES256 tokens for runs, the discovery document
// and the JWKS. The signing key is a P-256 scalar: OF_CLOUD_ID_KEY when set,
// else derived like Go from the signing key (HMAC-SHA256(seed, label ||
// counter_be32) until the scalar is valid, seed = DeriveKey(signing key,
// "openfactory/oidc-signing-key")). @noble/curves computes the public point,
// because WebCrypto cannot import a bare scalar.
import { newId } from "@openfactory/core";
import { p256 } from "@noble/curves/nist.js";
import type { Env } from "../contracts/env";
import { LABELS, deriveKey, signingKeys } from "../config";
import { b64url, concat, fromAnyB64, fromHex, hmacSha256, sha256, toHex, utf8 } from "../lib/bytes";

export const KEY_LABEL = "openfactory/oidc-signing-key";
export const DISCOVERY_PATH = "/.well-known/openid-configuration";
export const JWKS_PATH = "/.well-known/jwks.json";
export const DEFAULT_TTL_MS = 3600_000;
export const MAX_TTL_MS = 3600_000;

const N = BigInt("0xffffffff00000000ffffffffffffffffbce6faada7179e84f3b9cac2fc632551");

function validScalar(d: Uint8Array): boolean {
  const v = BigInt("0x" + toHex(d));
  return v > 0n && v < N;
}

/** deriveKey of cloudid.go: the first valid candidate of 64. */
export async function deriveScalar(seed: Uint8Array): Promise<Uint8Array> {
  for (let i = 0; i < 64; i++) {
    const c = new Uint8Array([(i >>> 24) & 255, (i >>> 16) & 255, (i >>> 8) & 255, i & 255]);
    const k = await hmacSha256(seed, concat(utf8(KEY_LABEL), c));
    if (validScalar(k)) return k;
  }
  throw new Error("cloudid: could not derive a signing key");
}

interface IssuerKey {
  kid: string;
  x: string;
  y: string;
  sign?: CryptoKey;
  verify: CryptoKey;
}

async function keyFromScalar(d: Uint8Array): Promise<IssuerKey> {
  if (d.length !== 32 || !validScalar(d)) throw new Error("cloudid: invalid P-256 private scalar");
  const pub = p256.getPublicKey(d, false); // 0x04 || X || Y
  const x = b64url(pub.subarray(1, 33));
  const y = b64url(pub.subarray(33, 65));
  const kid = toHex((await sha256(pub)).subarray(0, 8));
  const jwk = { kty: "EC", crv: "P-256", x, y, d: b64url(d), ext: true };
  const sign = await crypto.subtle.importKey("jwk", jwk, { name: "ECDSA", namedCurve: "P-256" }, false, ["sign"]);
  const verify = await crypto.subtle.importKey(
    "jwk",
    { kty: "EC", crv: "P-256", x, y, ext: true },
    { name: "ECDSA", namedCurve: "P-256" },
    false,
    ["verify"],
  );
  return { kid, x, y, sign, verify };
}

/** Claims of a run token (cloudid.Claims), in Go field order. */
export interface RunTokenClaims {
  iss: string;
  sub: string;
  aud: string;
  iat: number;
  nbf: number;
  exp: number;
  jti: string;
  team_id?: string;
  factory_id?: string;
  agent?: string;
  agent_type?: string;
  run_id?: string;
  work_item_id?: string;
  run_as?: string;
  creator?: string;
  worker_host?: string;
}

/** The run fields the issuer reads. */
export interface TokenRun {
  id: string;
  team_id: string;
  factory_id: string;
  agent_name?: string;
  work_item_id?: string;
  config?: { type?: string };
  run_as?: { type?: string; id?: string } | null;
}

export interface TokenOptions {
  creator?: string;
  runAs?: string;
  workerHost?: string;
}

/** Subject: team/<team>/factory/<factory>/agent/<agent>. */
export const subjectOf = (r: TokenRun) => `team/${r.team_id}/factory/${r.factory_id}/agent/${r.agent_name ?? ""}`;

export class Issuer {
  private constructor(
    readonly issuer: string,
    private readonly key: IssuerKey,
    private readonly previous: IssuerKey[],
    private now: () => number = Date.now,
  ) {}

  /** From raw scalars (current first). */
  static async fromScalars(issuerUrl: string, current: Uint8Array, previous: Uint8Array[] = []): Promise<Issuer> {
    const iss = issuerUrl.trim().replace(/\/+$/, "");
    if (!iss) throw new Error("cloudid: issuer URL (OF_PUBLIC_URL) is required");
    const key = await keyFromScalar(current);
    const prev: IssuerKey[] = [];
    const seen = new Set([key.kid]);
    for (const p of previous) {
      const k = await keyFromScalar(p);
      if (!seen.has(k.kid)) {
        seen.add(k.kid);
        delete k.sign;
        prev.push(k);
      }
    }
    return new Issuer(iss, key, prev);
  }

  /** NewIssuer: from seeds (sub-keys of the signing key), like Go. */
  static async fromSeeds(issuerUrl: string, seed: Uint8Array, previousSeeds: Uint8Array[] = []): Promise<Issuer> {
    return Issuer.fromScalars(issuerUrl, await deriveScalar(seed), await Promise.all(previousSeeds.map(deriveScalar)));
  }

  setClock(now: () => number): void {
    this.now = now;
  }

  get kid(): string {
    return this.key.kid;
  }

  /** Signs a token for run r with audience aud. ttl 0 = 1 h, capped at 1 h. */
  async token(
    r: TokenRun,
    aud: string,
    ttlMs = 0,
    o: TokenOptions = {},
  ): Promise<{ token: string; expiresAt: number }> {
    if (aud.trim() === "") throw new Error("cloudid: audience is required");
    let ttl = ttlMs <= 0 ? DEFAULT_TTL_MS : ttlMs;
    if (ttl > MAX_TTL_MS) ttl = MAX_TTL_MS;
    let runAs = o.runAs ?? "";
    if (!runAs) {
      if (r.run_as?.id) runAs = `${r.run_as.type ?? "user"}:${r.run_as.id}`;
      else if (o.creator) runAs = "user:" + o.creator;
      else runAs = "factory:" + r.factory_id;
    }
    const now = Math.floor(this.now() / 1000);
    const exp = now + Math.floor(ttl / 1000);
    const c: RunTokenClaims = {
      iss: this.issuer,
      sub: subjectOf(r),
      aud,
      iat: now,
      nbf: now - 30,
      exp,
      jti: newId("jti"),
    };
    const put = (k: keyof RunTokenClaims, v: string | undefined) => {
      if (v) (c as unknown as Record<string, unknown>)[k] = v;
    };
    put("team_id", r.team_id);
    put("factory_id", r.factory_id);
    put("agent", r.agent_name);
    put("agent_type", r.config?.type);
    put("run_id", r.id);
    put("work_item_id", r.work_item_id);
    put("run_as", runAs);
    put("creator", o.creator);
    put("worker_host", o.workerHost);
    return { token: await this.sign(c), expiresAt: exp * 1000 };
  }

  async sign(claims: unknown): Promise<string> {
    const h = b64url(utf8(JSON.stringify({ alg: "ES256", kid: this.key.kid, typ: "JWT" })));
    const signing = `${h}.${b64url(utf8(JSON.stringify(claims)))}`;
    // WebCrypto ECDSA returns r || s (the JWS format).
    const sig = new Uint8Array(
      await crypto.subtle.sign({ name: "ECDSA", hash: "SHA-256" }, this.key.sign!, utf8(signing)),
    );
    return `${signing}.${b64url(sig)}`;
  }

  discovery(): Record<string, unknown> {
    return {
      issuer: this.issuer,
      jwks_uri: this.issuer + JWKS_PATH,
      response_types_supported: ["id_token"],
      subject_types_supported: ["public"],
      id_token_signing_alg_values_supported: ["ES256"],
      scopes_supported: ["openid"],
      claims_supported: [
        "iss",
        "sub",
        "aud",
        "iat",
        "nbf",
        "exp",
        "jti",
        "team_id",
        "factory_id",
        "agent",
        "agent_type",
        "run_id",
        "work_item_id",
        "run_as",
        "creator",
        "worker_host",
      ],
    };
  }

  jwks(): { keys: Record<string, string>[] } {
    const jwk = (k: IssuerKey) => ({ kty: "EC", crv: "P-256", use: "sig", alg: "ES256", kid: k.kid, x: k.x, y: k.y });
    return { keys: [jwk(this.key), ...this.previous.map(jwk)] };
  }

  /** Verify (tests, diagnostics). */
  async verify(token: string): Promise<RunTokenClaims> {
    const parts = token.split(".");
    if (parts.length !== 3) throw new Error("cloudid: malformed token");
    let sig: Uint8Array;
    try {
      sig = fromAnyB64(parts[2]!) ?? new Uint8Array();
    } catch {
      sig = new Uint8Array();
    }
    if (sig.length !== 64) throw new Error("cloudid: malformed signature");
    const data = utf8(`${parts[0]}.${parts[1]}`);
    let ok = false;
    for (const k of [this.key, ...this.previous]) {
      ok ||= await crypto.subtle.verify(
        { name: "ECDSA", hash: "SHA-256" },
        k.verify,
        sig as Uint8Array<ArrayBuffer>,
        data,
      );
    }
    if (!ok) throw new Error("cloudid: bad signature");
    const c = JSON.parse(new TextDecoder().decode(fromAnyB64(parts[1]!)!)) as RunTokenClaims;
    if (c.iss !== this.issuer) throw new Error("cloudid: wrong issuer");
    const now = Math.floor(this.now() / 1000);
    if (now >= c.exp || now < c.nbf) throw new Error("cloudid: token expired or not yet valid");
    return c;
  }
}

/** Parses OF_CLOUD_ID_KEY: hex (64 chars) or base64 of 32 bytes. */
export function parseScalar(s: string): Uint8Array {
  const t = s.trim();
  if (/^[0-9a-fA-F]{64}$/.test(t)) return fromHex(t);
  const b = fromAnyB64(t);
  if (!b || b.length !== 32) throw new Error("OF_CLOUD_ID_KEY must be a 32-byte P-256 scalar in hex or base64");
  return b;
}

/** Issuer URL: OF_CLOUD_ID_ISSUER_URL (optional extra var), else OF_PUBLIC_URL. */
export function issuerUrl(env: Env): string {
  const e = env as unknown as Record<string, string | undefined>;
  return (e["OF_CLOUD_ID_ISSUER_URL"] || env.OF_PUBLIC_URL || "").replace(/\/+$/, "");
}

const cache = new Map<string, Promise<Issuer>>();

/** The issuer of this deployment, cached per isolate. Throws when no issuer URL is set. */
export function issuerFor(env: Env): Promise<Issuer> {
  const url = issuerUrl(env);
  const id = `${url}\0${env.OF_CLOUD_ID_KEY ?? ""}\0${env.OF_SIGNING_KEY ?? ""}\0${env.OF_SIGNING_KEY_PREVIOUS ?? ""}\0${env.OF_SECRET_KEY}`;
  let p = cache.get(id);
  if (!p) {
    p = (async () => {
      if (env.OF_CLOUD_ID_KEY) return Issuer.fromScalars(url, parseScalar(env.OF_CLOUD_ID_KEY));
      const ks = signingKeys(env);
      return Issuer.fromSeeds(
        url,
        await deriveKey(ks.current, LABELS.oidcSigning),
        await Promise.all(ks.previous.map((k) => deriveKey(k, LABELS.oidcSigning))),
      );
    })();
    p.catch(() => cache.delete(id));
    cache.set(id, p);
  }
  return p;
}
