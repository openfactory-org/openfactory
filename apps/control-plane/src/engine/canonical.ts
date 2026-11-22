// Canonical JSON and the tool idempotency key. JavaScript key
// order differs from Go's json.Marshal, so the key hashes a canonical form:
// object keys sorted by UTF-16 code units, no whitespace, undefined dropped.
import { sha256Hex } from "@openfactory/core";

export function canonicalJson(v: unknown): string {
  if (v === null || v === undefined) return "null";
  if (typeof v === "number") return Number.isFinite(v) ? JSON.stringify(v) : "null";
  if (typeof v === "string" || typeof v === "boolean") return JSON.stringify(v);
  if (Array.isArray(v)) return "[" + v.map((x) => (x === undefined ? "null" : canonicalJson(x))).join(",") + "]";
  if (typeof v === "object") {
    const o = v as Record<string, unknown>;
    const keys = Object.keys(o)
      .filter((k) => o[k] !== undefined)
      .sort();
    return "{" + keys.map((k) => JSON.stringify(k) + ":" + canonicalJson(o[k])).join(",") + "}";
  }
  return "null";
}

/** orch:idem:<run>:<turn>:<tool>:<sha256(canonical input)> */
export async function toolIdempotencyKey(runId: string, turnId: string, tool: string, input: unknown): Promise<string> {
  return `orch:idem:${runId}:${turnId}:${tool}:${await sha256Hex(canonicalJson(input))}`;
}
