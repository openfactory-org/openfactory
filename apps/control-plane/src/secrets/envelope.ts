// Secret envelope. Byte-exact port of internal/secrets/secrets.go:
//
//   0x02 | len(ver) (1) | ver | len(wrapped) (2, BE) | wrapped | body
//   wrapped = nonce12 | GCM(dek)   (KeyProvider)
//   body    = nonce12 | GCM(value) with AAD
//             "openfactory-secret-v1\0" team "\0" factory "\0" name "\0" type
//
// Each value has its own random 32-byte data key. Rewrap wraps the data key
// again with the current key; the sealed value does not change.
import { OfError } from "@openfactory/core";
import type { Env } from "../contracts/env";
import { encryptionKeys } from "../config";
import { concat, randomBytes, utf8 } from "../lib/bytes";
import { gcmOpen, gcmSeal, LocalKeys, type KeyProvider } from "./keys";

const ENVELOPE_VERSION = 2;

export interface SecretIdentity {
  teamId: string;
  factoryId: string;
  name: string;
  type: string;
}

export function secretAad(id: SecretIdentity): Uint8Array<ArrayBuffer> {
  return utf8(`openfactory-secret-v1\0${id.teamId}\0${id.factoryId}\0${id.name}\0${id.type}`);
}

export function buildEnvelope(version: string, wrapped: Uint8Array, body: Uint8Array): Uint8Array<ArrayBuffer> {
  const v = utf8(version);
  if (v.length === 0 || v.length > 255 || wrapped.length > 0xffff) {
    throw new Error("secrets: key provider returned a version or a wrapped key that is too long");
  }
  const len = new Uint8Array([wrapped.length >> 8, wrapped.length & 0xff]);
  return concat(new Uint8Array([ENVELOPE_VERSION, v.length]), v, len, wrapped, body);
}

export function parseEnvelope(env: Uint8Array): { version: string; wrapped: Uint8Array; body: Uint8Array } {
  if (env.length < 2) throw new Error("secrets: malformed ciphertext");
  if (env[0] !== ENVELOPE_VERSION) throw new Error(`secrets: unsupported envelope version ${env[0]}`);
  const n = env[1]!;
  let rest = env.subarray(2);
  if (n === 0 || rest.length < n + 2) throw new Error("secrets: malformed ciphertext");
  const version = new TextDecoder().decode(rest.subarray(0, n));
  rest = rest.subarray(n);
  const w = (rest[0]! << 8) | rest[1]!;
  rest = rest.subarray(2);
  if (rest.length < w) throw new Error("secrets: malformed ciphertext");
  return { version, wrapped: rest.subarray(0, w), body: rest.subarray(w) };
}

export class Envelopes {
  constructor(readonly provider: KeyProvider) {}

  currentVersion(): string {
    return this.provider.currentVersion();
  }

  async encrypt(id: SecretIdentity, value: Uint8Array): Promise<Uint8Array<ArrayBuffer>> {
    const dek = randomBytes(32);
    const { wrapped, version } = await this.provider.wrap(dek);
    const body = await gcmSeal(dek, value, secretAad(id));
    return buildEnvelope(version, wrapped, body);
  }

  private async unwrap(p: { version: string; wrapped: Uint8Array }): Promise<Uint8Array> {
    try {
      return await this.provider.unwrap(p.wrapped, p.version);
    } catch (e) {
      if (e instanceof OfError && e.code === "resource_not_found") {
        // Not a not-found error: callers would read it as a missing secret.
        throw new Error(
          `secrets: ciphertext was encrypted with a different master key (key version "${p.version}" is not configured)`,
          { cause: e },
        );
      }
      throw new Error(`secrets: cannot unwrap data key: ${(e as Error).message}`, { cause: e });
    }
  }

  async decrypt(id: SecretIdentity, envelope: Uint8Array): Promise<Uint8Array<ArrayBuffer>> {
    const p = parseEnvelope(envelope);
    const dek = await this.unwrap(p);
    try {
      return await gcmOpen(dek, p.body, secretAad(id));
    } catch {
      throw new Error("secrets: cannot decrypt value (tampered ciphertext)");
    }
  }

  /** Rewrap: the new envelope and whether it changed. Opens the value first. */
  async rewrap(id: SecretIdentity, envelope: Uint8Array): Promise<{ out: Uint8Array; changed: boolean }> {
    const p = parseEnvelope(envelope);
    if (p.version === this.provider.currentVersion()) return { out: envelope, changed: false };
    const dek = await this.unwrap(p);
    try {
      await gcmOpen(dek, p.body, secretAad(id));
    } catch {
      throw new Error("secrets: cannot decrypt value (tampered ciphertext)");
    }
    const { wrapped, version } = await this.provider.wrap(dek);
    return { out: buildEnvelope(version, wrapped, p.body), changed: true };
  }

  /** Key version of an envelope, or "" when malformed. */
  static versionOf(envelope: Uint8Array): string {
    try {
      return parseEnvelope(envelope).version;
    } catch {
      return "";
    }
  }
}

const cache = new Map<string, Promise<Envelopes>>();

/** The envelope sealer of OF_SECRET_KEY (+ OF_SECRET_KEY_PREVIOUS), cached per isolate. */
export function envelopes(env: Pick<Env, "OF_SECRET_KEY" | "OF_SECRET_KEY_PREVIOUS">): Promise<Envelopes> {
  const id = `${env.OF_SECRET_KEY}\0${env.OF_SECRET_KEY_PREVIOUS ?? ""}`;
  let p = cache.get(id);
  if (!p) {
    const ks = encryptionKeys(env);
    p = LocalKeys.create(ks.current, ks.previous).then((k) => new Envelopes(k));
    p.catch(() => cache.delete(id));
    cache.set(id, p);
  }
  return p;
}
