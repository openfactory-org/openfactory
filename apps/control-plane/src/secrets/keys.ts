// Key provider: wraps data keys with the master key. Port of internal/keys
// (Local): AES-256-GCM, AAD "openfactory-dek-wrap-v2", version = hex of the
// first 4 bytes of SHA-256(key). KeyProvider stays an interface so that the
// enterprise build can plug in a KMS.
import { OfError } from "@openfactory/core";
import { concat, randomBytes, utf8 } from "../lib/bytes";
import { keyVersion } from "../config";

export const KEY_SIZE = 32;
const NONCE = 12;
const WRAP_AAD = utf8("openfactory-dek-wrap-v2");

export interface KeyProvider {
  currentVersion(): string;
  versions(): string[];
  wrap(dataKey: Uint8Array): Promise<{ wrapped: Uint8Array; version: string }>;
  /** version "" tries every key. Throws OfError resource_not_found for an unknown version. */
  unwrap(wrapped: Uint8Array, version: string): Promise<Uint8Array>;
}

export async function gcmSeal(key: Uint8Array, plain: Uint8Array, aad: Uint8Array): Promise<Uint8Array<ArrayBuffer>> {
  const k = await crypto.subtle.importKey("raw", key as Uint8Array<ArrayBuffer>, "AES-GCM", false, ["encrypt"]);
  const nonce = randomBytes(NONCE);
  const ct = new Uint8Array(
    await crypto.subtle.encrypt(
      { name: "AES-GCM", iv: nonce, additionalData: aad as Uint8Array<ArrayBuffer> },
      k,
      plain as Uint8Array<ArrayBuffer>,
    ),
  );
  return concat(nonce, ct);
}

export async function gcmOpen(key: Uint8Array, sealed: Uint8Array, aad: Uint8Array): Promise<Uint8Array<ArrayBuffer>> {
  if (sealed.length < NONCE) throw new Error("ciphertext too short");
  const k = await crypto.subtle.importKey("raw", key as Uint8Array<ArrayBuffer>, "AES-GCM", false, ["decrypt"]);
  return new Uint8Array(
    await crypto.subtle.decrypt(
      {
        name: "AES-GCM",
        iv: sealed.subarray(0, NONCE) as Uint8Array<ArrayBuffer>,
        additionalData: aad as Uint8Array<ArrayBuffer>,
      },
      k,
      sealed.subarray(NONCE) as Uint8Array<ArrayBuffer>,
    ),
  );
}

export class LocalKeys implements KeyProvider {
  private constructor(
    private readonly order: string[],
    private readonly keys: Map<string, Uint8Array>,
  ) {}

  static async create(current: Uint8Array, previous: Uint8Array[] = []): Promise<LocalKeys> {
    const keys = new Map<string, Uint8Array>();
    const order: string[] = [];
    for (const [i, k] of [current, ...previous].entries()) {
      if (k.length !== KEY_SIZE) throw new Error(`keys: key ${i} must have ${KEY_SIZE} bytes, got ${k.length}`);
      const v = await keyVersion(k);
      if (keys.has(v)) continue;
      keys.set(v, k);
      order.push(v);
    }
    return new LocalKeys(order, keys);
  }

  currentVersion(): string {
    return this.order[0]!;
  }

  versions(): string[] {
    return [...this.order];
  }

  async wrap(dataKey: Uint8Array): Promise<{ wrapped: Uint8Array; version: string }> {
    const v = this.currentVersion();
    return { wrapped: await gcmSeal(this.keys.get(v)!, dataKey, WRAP_AAD), version: v };
  }

  async unwrap(wrapped: Uint8Array, version: string): Promise<Uint8Array> {
    if (version === "") {
      for (const v of this.order) {
        try {
          return await gcmOpen(this.keys.get(v)!, wrapped, WRAP_AAD);
        } catch {
          /* next */
        }
      }
      throw new Error("keys: no key can unwrap the data key");
    }
    const key = this.keys.get(version);
    if (!key) throw new OfError("resource_not_found", `key version "${version}"`);
    try {
      return await gcmOpen(key, wrapped, WRAP_AAD);
    } catch {
      throw new Error("keys: cannot unwrap the data key");
    }
  }
}
