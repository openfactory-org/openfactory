// Envelope encryption of MCP OAuth tokens and authorization states. Same
// format as internal/secrets (Store.Encrypt with the keys.Local provider):
//
//   version(1)=2 | len(keyVersion)(1) | keyVersion | len(wrapped)(2, BE) | wrapped | nonce(12) || AES-GCM(dek, value, aad)
//
// wrapped = nonce(12) || AES-GCM(kek, dek, "openfactory-dek-wrap-v2"), and
// aad = "openfactory-secret-v1\0" team "\0" factory "\0" name "\0" type.
// The KEK is OF_SECRET_KEY (previous keys in OF_SECRET_KEY_PREVIOUS decrypt).
// Owner of the general secret store is WS4; this module is the MCP subset,
// kept byte-compatible so that a rewrap of all envelopes can include ours.
import type { Env } from "../contracts/env";
import { encryptionKeys, keyVersion } from "../config";
import { concat, randomBytes, utf8 } from "../lib/bytes";

const ENVELOPE_VERSION = 2;
const NONCE = 12;
const WRAP_AAD = utf8("openfactory-dek-wrap-v2");

/** Secret types of the MCP envelopes (bound into the ciphertext). */
export const MCP_TOKEN_TYPE = "mcp_oauth_token";
export const MCP_STATE_TYPE = "mcp_oauth_state";

export function aad(teamId: string, factoryId: string, name: string, type: string): Uint8Array<ArrayBuffer> {
  return utf8(`openfactory-secret-v1\0${teamId}\0${factoryId}\0${name}\0${type}`);
}

async function aes(key: Uint8Array<ArrayBuffer>, usage: "encrypt" | "decrypt"): Promise<CryptoKey> {
  return crypto.subtle.importKey("raw", key, "AES-GCM", false, [usage]);
}

async function seal(key: Uint8Array<ArrayBuffer>, plain: Uint8Array<ArrayBuffer>, ad: Uint8Array<ArrayBuffer>): Promise<Uint8Array<ArrayBuffer>> {
  const nonce = randomBytes(NONCE);
  const ct = await crypto.subtle.encrypt({ name: "AES-GCM", iv: nonce, additionalData: ad }, await aes(key, "encrypt"), plain);
  return concat(nonce, new Uint8Array(ct));
}

async function open(key: Uint8Array<ArrayBuffer>, sealed: Uint8Array, ad: Uint8Array<ArrayBuffer>): Promise<Uint8Array<ArrayBuffer>> {
  if (sealed.length < NONCE) throw new Error("ciphertext too short");
  const pt = await crypto.subtle.decrypt(
    { name: "AES-GCM", iv: sealed.slice(0, NONCE), additionalData: ad },
    await aes(key, "decrypt"),
    sealed.slice(NONCE),
  );
  return new Uint8Array(pt);
}

/** Encrypts and decrypts envelopes with the Worker's KEKs. */
export interface Encrypter {
  encrypt(teamId: string, factoryId: string, name: string, type: string, value: Uint8Array<ArrayBuffer>): Promise<Uint8Array<ArrayBuffer>>;
  decrypt(teamId: string, factoryId: string, name: string, type: string, envelope: Uint8Array): Promise<Uint8Array<ArrayBuffer>>;
}

export async function localEncrypter(env: Pick<Env, "OF_SECRET_KEY" | "OF_SECRET_KEY_PREVIOUS">): Promise<Encrypter> {
  const ks = encryptionKeys(env);
  const keys = new Map<string, Uint8Array<ArrayBuffer>>();
  const order: string[] = [];
  for (const k of [ks.current, ...ks.previous]) {
    const v = await keyVersion(k);
    if (keys.has(v)) continue;
    keys.set(v, k);
    order.push(v);
  }
  const current = order[0]!;
  return {
    async encrypt(teamId, factoryId, name, type, value) {
      const dek = randomBytes(32);
      const wrapped = await seal(keys.get(current)!, dek, WRAP_AAD);
      const body = await seal(dek, value, aad(teamId, factoryId, name, type));
      const ver = utf8(current);
      const head = new Uint8Array(2 + ver.length + 2);
      head[0] = ENVELOPE_VERSION;
      head[1] = ver.length;
      head.set(ver, 2);
      new DataView(head.buffer).setUint16(2 + ver.length, wrapped.length);
      return concat(head, wrapped, body);
    },
    async decrypt(teamId, factoryId, name, type, env) {
      if (env.length < 2 || env[0] !== ENVELOPE_VERSION) throw new Error("secrets: malformed ciphertext");
      const n = env[1]!;
      if (n === 0 || env.length < 2 + n + 2) throw new Error("secrets: malformed ciphertext");
      const version = new TextDecoder().decode(env.subarray(2, 2 + n));
      const w = new DataView(env.buffer, env.byteOffset + 2 + n, 2).getUint16(0);
      const rest = env.subarray(4 + n);
      if (rest.length < w) throw new Error("secrets: malformed ciphertext");
      const kek = keys.get(version);
      if (!kek) throw new Error(`secrets: ciphertext was encrypted with a different master key (key version "${version}" is not configured)`);
      let dek: Uint8Array<ArrayBuffer>;
      try {
        dek = await open(kek, rest.subarray(0, w), WRAP_AAD);
      } catch {
        throw new Error("keys: cannot unwrap the data key");
      }
      try {
        return await open(dek, rest.subarray(w), aad(teamId, factoryId, name, type));
      } catch {
        throw new Error("secrets: cannot decrypt value (tampered ciphertext)");
      }
    },
  };
}
