// Run metadata: validation and the OF_METADATA_* environment mapping. Port of
// internal/worker/metadata.go.

import { OfError } from "./errors";
import { byteLength, goQuote, isWellFormed, sortStrings } from "./gostd";

export const MAX_METADATA_KEYS = 20;
export const MAX_METADATA_KEY_LEN = 64;
export const MAX_METADATA_VALUE_LEN = 256;
/** Prefixes metadata environment variables. */
export const METADATA_ENV_PREFIX = "OF_METADATA_";

const KEY_RE = /^[A-Za-z0-9._-]+$/;

/** OF_METADATA_ + key uppercased with "." and "-" changed to "_". */
export function metadataEnvName(key: string): string {
  return METADATA_ENV_PREFIX + key.replace(/[.-]/g, "_").toUpperCase();
}

/**
 * The problems of a metadata map: at most 20 keys; keys of 1-64 bytes using
 * A-Z, a-z, 0-9, ".", "_" and "-"; values of at most 256 bytes of UTF-8 without
 * NUL; no two keys mapping to the same environment variable.
 */
export function metadataProblems(meta: Readonly<Record<string, string>> | null | undefined): string[] {
  const m = meta ?? {};
  const problems: string[] = [];
  const keys = sortStrings(Object.keys(m));
  if (keys.length > MAX_METADATA_KEYS) {
    problems.push(`a run can have at most ${MAX_METADATA_KEYS} metadata keys, got ${keys.length}`);
  }
  const envs = new Map<string, string>();
  for (const k of keys) {
    const v = m[k]!;
    const kl = byteLength(k);
    if (kl === 0 || kl > MAX_METADATA_KEY_LEN) {
      problems.push(`metadata key ${goQuote(k)} must be 1-${MAX_METADATA_KEY_LEN} bytes`);
    } else if (!KEY_RE.test(k)) {
      problems.push(`metadata key ${goQuote(k)} may contain only A-Z, a-z, 0-9, '.', '_' and '-'`);
    }
    if (byteLength(v) > MAX_METADATA_VALUE_LEN) {
      problems.push(`metadata value for ${goQuote(k)} exceeds ${MAX_METADATA_VALUE_LEN} bytes`);
    }
    if (!isWellFormed(v)) problems.push(`metadata value for ${goQuote(k)} is not valid UTF-8`);
    if (v.includes("\u0000")) problems.push(`metadata value for ${goQuote(k)} contains a NUL character`);
    if (k !== "") {
      const env = metadataEnvName(k);
      const other = envs.get(env);
      if (other !== undefined) problems.push(`metadata keys ${goQuote(other)} and ${goQuote(k)} both map to ${env}`);
      else envs.set(env, k);
    }
  }
  return problems;
}

/** Validates metadata. Throws invalid_request with all problems joined by "; ". */
export function validateMetadata(meta: Readonly<Record<string, string>> | null | undefined): void {
  const problems = metadataProblems(meta);
  if (problems.length > 0) throw new OfError("invalid_request", problems.join("; "));
}

/** Validates meta and returns its OF_METADATA_* variables. */
export function metadataEnv(meta: Readonly<Record<string, string>> | null | undefined): Record<string, string> {
  validateMetadata(meta);
  const out: Record<string, string> = {};
  for (const [k, v] of Object.entries(meta ?? {})) out[metadataEnvName(k)] = v;
  return out;
}

/**
 * Merges run metadata for a child run: the child value wins, then the batch
 * (run_agents) value, then the inherited parent value. The result is validated
 * (the key limit includes inherited keys).
 */
export function mergeMetadata(
  parent?: Readonly<Record<string, string>> | null,
  batch?: Readonly<Record<string, string>> | null,
  child?: Readonly<Record<string, string>> | null,
): Record<string, string> {
  const out: Record<string, string> = { ...parent, ...batch, ...child };
  validateMetadata(out);
  return out;
}

/** Rejects environments that set variables with the reserved OF_METADATA_ prefix. */
export function checkNoMetadataEnv(env: Readonly<Record<string, string>> | null | undefined): void {
  const bad = sortStrings(Object.keys(env ?? {}).filter((k) => k.toUpperCase().startsWith(METADATA_ENV_PREFIX)));
  if (bad.length > 0) {
    throw new OfError("invalid_request", `the ${METADATA_ENV_PREFIX} prefix is reserved for run metadata: ${bad.join(", ")}`);
  }
}
