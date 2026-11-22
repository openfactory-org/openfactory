// Signature Version 4 (SigV4) for the S3 API of R2. Port of
// internal/objectstore/sigv4.go (header signing and query presigning),
// hand-written on WebCrypto. R2 uses region "auto" and path-style URLs
// https://<account>.r2.cloudflarestorage.com/<bucket>/<key>.
import type { Env } from "../contracts/env";
import { hmacSha256, sha256Hex, toHex, utf8 } from "../lib/bytes";
import { serverConfig } from "../config";

export const SIGV4_ALGORITHM = "AWS4-HMAC-SHA256";
export const UNSIGNED_PAYLOAD = "UNSIGNED-PAYLOAD";
export const EMPTY_PAYLOAD_HASH = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855";
/** R2 presigned URLs live at most 7 days. */
export const MAX_PRESIGN_SECONDS = 604_800;

export interface Creds {
  accessKeyId: string;
  secretAccessKey: string;
  sessionToken?: string;
}

/** awsURIEncode: unreserved kept, everything else %XX of the UTF-8 bytes; "/" kept when keepSlash. */
export function awsUriEncode(s: string, keepSlash: boolean): string {
  let out = "";
  for (const c of utf8(s)) {
    if (
      (c >= 0x41 && c <= 0x5a) ||
      (c >= 0x61 && c <= 0x7a) ||
      (c >= 0x30 && c <= 0x39) ||
      c === 0x2d ||
      c === 0x5f ||
      c === 0x2e ||
      c === 0x7e ||
      (c === 0x2f && keepSlash)
    ) {
      out += String.fromCharCode(c);
    } else {
      out += "%" + c.toString(16).toUpperCase().padStart(2, "0");
    }
  }
  return out;
}

/** canonicalQuery: encoded pairs sorted by key, then value. */
export function canonicalQuery(q: URLSearchParams): string {
  const pairs: [string, string][] = [];
  for (const [k, v] of q) pairs.push([awsUriEncode(k, false), awsUriEncode(v, false)]);
  pairs.sort((a, b) => (a[0] !== b[0] ? (a[0] < b[0] ? -1 : 1) : a[1] < b[1] ? -1 : a[1] > b[1] ? 1 : 0));
  return pairs.map(([k, v]) => `${k}=${v}`).join("&");
}

/** host, content-type, content-md5, range and x-amz-* headers, sorted. */
export function signedHeaderNames(h: Headers): string[] {
  const names = new Set(["host"]);
  for (const k of h.keys()) {
    const lk = k.toLowerCase();
    if (lk === "content-type" || lk === "content-md5" || lk === "range" || lk.startsWith("x-amz-")) names.add(lk);
  }
  return [...names].sort();
}

const trimValue = (v: string) => v.trim().split(/\s+/).join(" ");

function canonicalHeaders(names: string[], host: string, h: Headers): string {
  return names
    .map((n) => {
      if (n === "host") return `host:${host}\n`;
      const v = h.get(n) ?? "";
      return `${n}:${v.split(", ").map(trimValue).join(",")}\n`;
    })
    .join("");
}

function amzDate(t: Date): string {
  return t
    .toISOString()
    .replace(/[-:]/g, "")
    .replace(/\.\d{3}/, "");
}

export class SigV4 {
  constructor(
    readonly creds: Creds,
    readonly region: string,
    readonly service: string,
  ) {}

  scope(t: Date): string {
    return `${amzDate(t).slice(0, 8)}/${this.region}/${this.service}/aws4_request`;
  }

  private async signingKey(t: Date): Promise<Uint8Array> {
    let k = await hmacSha256(utf8("AWS4" + this.creds.secretAccessKey), amzDate(t).slice(0, 8));
    k = await hmacSha256(k, this.region);
    k = await hmacSha256(k, this.service);
    return hmacSha256(k, "aws4_request");
  }

  async signature(t: Date, canonicalRequest: string): Promise<string> {
    const sts = [SIGV4_ALGORITHM, amzDate(t), this.scope(t), await sha256Hex(canonicalRequest)].join("\n");
    return toHex(await hmacSha256(await this.signingKey(t), sts));
  }

  /** The encoded path; S3 encodes once, other services twice. `path` is decoded. */
  canonicalUri(path: string): string {
    const enc = awsUriEncode(path === "" ? "/" : path, true);
    return this.service === "s3" ? enc : awsUriEncode(enc, true);
  }

  /** Sign: sets X-Amz-Date (and token, and X-Amz-Content-Sha256 for S3) and Authorization. */
  async sign(method: string, url: URL, headers: Headers, payloadHash: string, t: Date): Promise<void> {
    headers.set("x-amz-date", amzDate(t));
    if (this.creds.sessionToken) headers.set("x-amz-security-token", this.creds.sessionToken);
    if (this.service === "s3") headers.set("x-amz-content-sha256", payloadHash);
    const names = signedHeaderNames(headers);
    const signed = names.join(";");
    const creq = [
      method,
      this.canonicalUri(decodeURIComponent(url.pathname)),
      canonicalQuery(url.searchParams),
      canonicalHeaders(names, url.host, headers),
      signed,
      payloadHash,
    ].join("\n");
    headers.set(
      "authorization",
      `${SIGV4_ALGORITHM} Credential=${this.creds.accessKeyId}/${this.scope(t)}, SignedHeaders=${signed}, Signature=${await this.signature(t, creq)}`,
    );
  }

  /** Presign: query-string authentication. `extra` headers are signed and must be sent by the client. */
  async presign(method: string, url: URL, extra: Headers | undefined, t: Date, ttlSeconds: number): Promise<string> {
    const q = new URLSearchParams(url.searchParams);
    q.set("X-Amz-Algorithm", SIGV4_ALGORITHM);
    q.set("X-Amz-Credential", `${this.creds.accessKeyId}/${this.scope(t)}`);
    q.set("X-Amz-Date", amzDate(t));
    q.set("X-Amz-Expires", String(Math.trunc(ttlSeconds)));
    if (this.creds.sessionToken) q.set("X-Amz-Security-Token", this.creds.sessionToken);
    const h = extra ?? new Headers();
    const names = signedHeaderNames(h);
    const signed = names.join(";");
    q.set("X-Amz-SignedHeaders", signed);
    const creq = [
      method,
      this.canonicalUri(decodeURIComponent(url.pathname)),
      canonicalQuery(q),
      canonicalHeaders(names, url.host, h),
      signed,
      UNSIGNED_PAYLOAD,
    ].join("\n");
    q.set("X-Amz-Signature", await this.signature(t, creq));
    return `${url.protocol}//${url.host}${url.pathname}?${canonicalQuery(q)}`;
  }
}

export interface PresignInput {
  method: "GET" | "PUT" | "HEAD" | "DELETE";
  key: string;
  /** Seconds, 1..604800. Default OF_OBJECT_STORE_PRESIGN_TTL. */
  expiresSeconds?: number;
  /** Signed when set; the client must send the same Content-Type. */
  contentType?: string;
  now?: Date;
}

/** The R2 S3 host of the account (also a RequiredHost of restricted runs). */
export function r2Host(accountId: string): string {
  return `${accountId}.r2.cloudflarestorage.com`;
}

/** A presigned R2 URL for one object (path-style). */
export async function presignR2(env: Env, i: PresignInput): Promise<string> {
  const cfg = serverConfig(env);
  if (!cfg.r2AccountId) throw new Error("presign: OF_R2_ACCOUNT_ID is not set");
  const ttl = i.expiresSeconds ?? Math.floor(cfg.presignTtlMs / 1000);
  if (ttl < 1 || ttl > MAX_PRESIGN_SECONDS) throw new Error("R2 presign expiry must be 1..604800 s");
  const url = new URL(
    `https://${r2Host(cfg.r2AccountId)}/${cfg.r2Bucket}/${i.key
      .split("/")
      .map((p) => awsUriEncode(p, false))
      .join("/")}`,
  );
  const s = new SigV4(
    { accessKeyId: env.OF_R2_ACCESS_KEY_ID, secretAccessKey: env.OF_R2_SECRET_ACCESS_KEY },
    "auto",
    "s3",
  );
  const extra = new Headers();
  if (i.contentType) extra.set("content-type", i.contentType);
  return s.presign(i.method, url, extra, i.now ?? new Date(), ttl);
}
