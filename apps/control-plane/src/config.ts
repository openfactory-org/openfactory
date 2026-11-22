// Control-plane configuration from Worker vars and secrets (D31). Port of the
// key handling of internal/config (MasterKey, EncryptionKeys, SigningKeys,
// parseKeyList, DeriveKey) and of the duration settings. Settings that an
// admin changes at run time live in the TeamDO `settings` module.
import { OfError } from "@openfactory/core";
import type { Env } from "./contracts/env";
import { fromAnyB64, hmacSha256, sha256, utf8 } from "./lib/bytes";

export const KEY_SIZE = 32;

/** Labels of DeriveKey (internal/config). The object-store label is gone with the file store. */
export const LABELS = {
  webSession: "openfactory/web-session-key",
  accountLink: "openfactory/account-link-key",
  oidcSigning: "openfactory/oidc-signing-key",
  runnerToken: "openfactory/runner-token-key",
  /** New: upload IDs and other short MACs of the Worker. */
  uploadId: "openfactory/upload-id-key",
} as const;

/** secrets.LoadKey: base64 (std or URL, padded or not) of exactly 32 bytes. */
export function loadKey(encoded: string): Uint8Array<ArrayBuffer> {
  const s = encoded.trim();
  if (s === "") throw new Error("empty key");
  const k = fromAnyB64(s);
  if (!k) throw new Error("key is not valid base64");
  if (k.length !== KEY_SIZE) throw new Error(`key must be ${KEY_SIZE} bytes, got ${k.length}`);
  return k;
}

/** parseKeyList: comma-separated base64 keys; empty items are skipped. */
export function parseKeyList(name: string, list: string | undefined): Uint8Array<ArrayBuffer>[] {
  const out: Uint8Array<ArrayBuffer>[] = [];
  (list ?? "").split(",").forEach((item, i) => {
    if (item.trim() === "") return;
    try {
      out.push(loadKey(item));
    } catch (e) {
      throw new Error(`${name} item ${i + 1}: ${(e as Error).message}`);
    }
  });
  return out;
}

export interface KeySet {
  current: Uint8Array<ArrayBuffer>;
  previous: Uint8Array<ArrayBuffer>[];
}

function configError(msg: string): OfError {
  console.error("config:", msg);
  return new OfError("internal_error", "server configuration error");
}

/** OF_SECRET_KEY + OF_SECRET_KEY_PREVIOUS (EncryptionKeys). */
export function encryptionKeys(env: Pick<Env, "OF_SECRET_KEY" | "OF_SECRET_KEY_PREVIOUS">): KeySet {
  if (!env.OF_SECRET_KEY || env.OF_SECRET_KEY.trim() === "") throw configError("OF_SECRET_KEY is not set");
  try {
    return { current: loadKey(env.OF_SECRET_KEY), previous: parseKeyList("OF_SECRET_KEY_PREVIOUS", env.OF_SECRET_KEY_PREVIOUS) };
  } catch (e) {
    throw configError(`OF_SECRET_KEY: ${(e as Error).message}`);
  }
}

/**
 * SigningKeys: OF_SIGNING_KEY + OF_SIGNING_KEY_PREVIOUS, or the encryption
 * keys when OF_SIGNING_KEY is empty (one rotation moves both).
 */
export function signingKeys(
  env: Pick<Env, "OF_SECRET_KEY" | "OF_SECRET_KEY_PREVIOUS" | "OF_SIGNING_KEY" | "OF_SIGNING_KEY_PREVIOUS">,
): KeySet {
  if (!env.OF_SIGNING_KEY || env.OF_SIGNING_KEY.trim() === "") {
    if ((env.OF_SIGNING_KEY_PREVIOUS ?? "").trim() !== "") throw configError("OF_SIGNING_KEY_PREVIOUS needs OF_SIGNING_KEY");
    return encryptionKeys(env);
  }
  try {
    return { current: loadKey(env.OF_SIGNING_KEY), previous: parseKeyList("OF_SIGNING_KEY_PREVIOUS", env.OF_SIGNING_KEY_PREVIOUS) };
  } catch (e) {
    throw configError(`OF_SIGNING_KEY: ${(e as Error).message}`);
  }
}

/** DeriveKey: HMAC-SHA256(master, label). */
export function deriveKey(master: Uint8Array, label: string): Promise<Uint8Array<ArrayBuffer>> {
  return hmacSha256(master, utf8(label));
}

/** Derived keys of one label for the current and previous signing keys, cached per isolate. */
const derivedCache = new Map<string, Promise<KeySet>>();

export function derivedSigningKeys(env: Env, label: string): Promise<KeySet> {
  const id = `${label}\0${env.OF_SIGNING_KEY ?? ""}\0${env.OF_SIGNING_KEY_PREVIOUS ?? ""}\0${env.OF_SECRET_KEY}\0${env.OF_SECRET_KEY_PREVIOUS ?? ""}`;
  let p = derivedCache.get(id);
  if (!p) {
    const ks = signingKeys(env);
    p = (async () => ({
      current: await deriveKey(ks.current, label),
      previous: await Promise.all(ks.previous.map((k) => deriveKey(k, label))),
    }))();
    p.catch(() => derivedCache.delete(id));
    derivedCache.set(id, p);
  }
  return p;
}

/** Key ID of a key: hex of the first 4 bytes of SHA-256 (keys.KeyVersion, runner kid). */
export async function keyVersion(key: Uint8Array): Promise<string> {
  const d = await sha256(key);
  return [...d.subarray(0, 4)].map((b) => b.toString(16).padStart(2, "0")).join("");
}

// ---------- durations ----------

const UNIT_MS: Record<string, number> = { ns: 1e-6, us: 1e-3, "µs": 1e-3, ms: 1, s: 1000, m: 60_000, h: 3_600_000 };

/** Go time.ParseDuration subset ("90s", "1h30m", "15m", "0"). Returns ms. */
export function parseDuration(s: string): number {
  const t = s.trim();
  if (t === "0") return 0;
  const re = /(\d+(?:\.\d+)?)(ns|us|µs|ms|s|m|h)/gy;
  let total = 0;
  let pos = 0;
  let neg = false;
  let str = t;
  if (str.startsWith("-") || str.startsWith("+")) {
    neg = str.startsWith("-");
    str = str.slice(1);
  }
  re.lastIndex = 0;
  if (str === "") throw new Error(`invalid duration "${s}"`);
  while (pos < str.length) {
    re.lastIndex = pos;
    const m = re.exec(str);
    if (!m || m.index !== pos) throw new Error(`invalid duration "${s}"`);
    total += Number(m[1]) * UNIT_MS[m[2]!]!;
    pos = re.lastIndex;
  }
  return neg ? -total : total;
}

function durationVar(env: Env, name: keyof Env, def: string): number {
  const v = (env[name] as string | undefined) ?? "";
  try {
    return parseDuration(v === "" ? def : v);
  } catch {
    console.error(`config: ${String(name)}="${v}" is not a duration; using ${def}`);
    return parseDuration(def);
  }
}

export interface ServerConfig {
  publicUrl: string;
  /** scheme://host of publicUrl, "" when unset. */
  origin: string;
  secureCookies: boolean;
  presignTtlMs: number;
  queueTimeoutMs: number;
  maxSessionDurationMs: number;
  sessionStopGraceMs: number;
  sandboxStartTimeoutMs: number;
  sandboxProvider: string;
  allowUnenforcedNetworkPolicy: boolean;
  devProcessWorkers: boolean;
  runnerVersion: string;
  r2AccountId: string;
  r2Bucket: string;
  oidc: { issuer: string; clientId: string; clientSecret: string } | null;
  logLevel: string;
}

const R2_MAX_PRESIGN_MS = 7 * 24 * 3600_000;

const truthy = (v: string | undefined): boolean => /^(1|t|true|yes|on)$/i.test((v ?? "").trim());

/** Reads the Worker vars. Cheap; call per request. */
export function serverConfig(env: Env): ServerConfig {
  const publicUrl = (env.OF_PUBLIC_URL ?? "").replace(/\/+$/, "");
  let origin = "";
  let secure = false;
  if (publicUrl) {
    try {
      const u = new URL(publicUrl);
      origin = `${u.protocol}//${u.host}`;
      secure = u.protocol === "https:";
    } catch {
      console.error(`config: OF_PUBLIC_URL "${publicUrl}" is not an absolute URL`);
    }
  }
  const presign = Math.min(durationVar(env, "OF_OBJECT_STORE_PRESIGN_TTL", "15m"), R2_MAX_PRESIGN_MS);
  return {
    publicUrl,
    origin,
    secureCookies: secure,
    presignTtlMs: presign > 0 ? presign : 15 * 60_000,
    queueTimeoutMs: durationVar(env, "OF_QUEUE_TIMEOUT", "30m"),
    maxSessionDurationMs: durationVar(env, "OF_MAX_SESSION_DURATION", "4h"),
    sessionStopGraceMs: durationVar(env, "OF_SESSION_STOP_GRACE", "30s"),
    sandboxStartTimeoutMs: durationVar(env, "OF_SANDBOX_START_TIMEOUT", "10m"),
    sandboxProvider: env.OF_SANDBOX_PROVIDER || "cloudflare-sandbox",
    allowUnenforcedNetworkPolicy: truthy(env.OF_ALLOW_UNENFORCED_NETWORK_POLICY),
    devProcessWorkers: truthy(env.OF_DEV_PROCESS_WORKERS),
    runnerVersion: env.OF_RUNNER_VERSION || "0.0.0-dev",
    r2AccountId: env.OF_R2_ACCOUNT_ID ?? "",
    r2Bucket: env.OF_R2_BUCKET || "openfactory-data",
    oidc: env.OF_OIDC_ISSUER_URL
      ? { issuer: env.OF_OIDC_ISSUER_URL, clientId: env.OF_OIDC_CLIENT_ID ?? "", clientSecret: env.OF_OIDC_CLIENT_SECRET ?? "" }
      : null,
    logLevel: env.OF_LOG_LEVEL || "info",
  };
}

/** Build version, set at build time (GET /version). */
export const BUILD = {
  version: (globalThis as { __OF_VERSION__?: string }).__OF_VERSION__ ?? "0.0.0-dev",
  commit: (globalThis as { __OF_COMMIT__?: string }).__OF_COMMIT__ ?? "",
  date: (globalThis as { __OF_DATE__?: string }).__OF_DATE__ ?? "",
};
