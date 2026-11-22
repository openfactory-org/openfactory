// API keys. Port of internal/api/auth.go: of_<prefix>_<secret> for users,
// ofa_<prefix>_<secret> for service accounts; prefix 8 lower-case
// alphanumerics, secret 40 alphanumerics. Only SHA-256(full key) is stored.
import { sha256Hex } from "../lib/bytes";

export const KEY_PREFIX = "of_";
export const SERVICE_ACCOUNT_KEY_PREFIX = "ofa_";
const PREFIX_LEN = 8;
const SECRET_LEN = 40;
const LOWER_ALNUM = "abcdefghijklmnopqrstuvwxyz0123456789";
const ALNUM = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789";

/** Uniform random string over an alphabet (rejection sampling). */
export function randomString(alphabet: string, n: number): string {
  const out: string[] = [];
  const limit = 256 - (256 % alphabet.length);
  const buf = new Uint8Array(n * 2);
  while (out.length < n) {
    crypto.getRandomValues(buf);
    for (const b of buf) {
      if (b < limit) out.push(alphabet[b % alphabet.length]!);
      if (out.length === n) break;
    }
  }
  return out.join("");
}

export interface GeneratedKey {
  key: string;
  prefix: string;
  hash: string;
}

export async function generateKey(
  kind: typeof KEY_PREFIX | typeof SERVICE_ACCOUNT_KEY_PREFIX = KEY_PREFIX,
): Promise<GeneratedKey> {
  const prefix = randomString(LOWER_ALNUM, PREFIX_LEN);
  const key = `${kind}${prefix}_${randomString(ALNUM, SECRET_LEN)}`;
  return { key, prefix, hash: await hashKey(key) };
}

/** Hex SHA-256 of a key. */
export function hashKey(key: string): Promise<string> {
  return sha256Hex(key);
}

const allIn = (s: string, alphabet: string) => [...s].every((c) => alphabet.includes(c));

/** ParseKey: the lookup prefix, or null for a malformed key. */
export function parseKey(key: string): { prefix: string; serviceAccount: boolean } | null {
  let rest: string;
  let serviceAccount = false;
  if (key.startsWith(KEY_PREFIX)) rest = key.slice(KEY_PREFIX.length);
  else if (key.startsWith(SERVICE_ACCOUNT_KEY_PREFIX)) {
    rest = key.slice(SERVICE_ACCOUNT_KEY_PREFIX.length);
    serviceAccount = true;
  } else return null;
  const i = rest.indexOf("_");
  if (i < 0) return null;
  const prefix = rest.slice(0, i);
  const secret = rest.slice(i + 1);
  if (prefix.length !== PREFIX_LEN || secret.length !== SECRET_LEN) return null;
  if (!allIn(prefix, LOWER_ALNUM) || !allIn(secret, ALNUM)) return null;
  return { prefix, serviceAccount };
}

export function isApiKey(tok: string): boolean {
  return tok.startsWith(KEY_PREFIX) || tok.startsWith(SERVICE_ACCOUNT_KEY_PREFIX);
}

/** Token of "Authorization: Bearer <token>" (case-insensitive scheme), or "". */
export function bearerToken(req: Request): string {
  const h = req.headers.get("authorization") ?? "";
  const i = h.indexOf(" ");
  if (i < 0 || h.slice(0, i).toLowerCase() !== "bearer") return "";
  return h.slice(i + 1).trim();
}
