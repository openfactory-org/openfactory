// Canonical JSON, SHA-256 hashing (WebCrypto) and lossless JSON numbers.
//
// Canonical JSON is the key of idempotency rows (plan rule 3): JavaScript key
// order differs from Go json.Marshal, so keys are sorted at every level (Go
// sort order: UTF-8 bytes) and strings use Go escaping. Payload filters parse
// JSON losslessly (plan rule 12): Go compares numbers above 2^53 exactly.

import {
  isLosslessNumber,
  LosslessNumber,
  parse as losslessParse,
  stringify as losslessStringify,
} from "lossless-json";
import { compareBytes, goJSONString } from "./gostd";

export { isLosslessNumber, LosslessNumber };

/**
 * The canonical JSON text of v: object keys sorted by UTF-8 bytes, no
 * whitespace, Go string escaping, undefined members left out (undefined in an
 * array is null). LosslessNumber and bigint values keep their exact digits.
 * It equals Go json.Marshal for a value decoded into map[string]any.
 */
export function canonicalJSON(v: unknown): string {
  if (v === null || v === undefined) return "null";
  switch (typeof v) {
    case "string":
      return goJSONString(v);
    case "number":
      return Number.isFinite(v) ? JSON.stringify(v) : "null";
    case "bigint":
      return v.toString();
    case "boolean":
      return v ? "true" : "false";
    case "object":
      break;
    default:
      return "null";
  }
  if (isLosslessNumber(v)) return v.toString();
  if (Array.isArray(v)) return "[" + v.map(canonicalJSON).join(",") + "]";
  if (v instanceof Date) return goJSONString(v.toISOString());
  const o = v as Record<string, unknown>;
  if (typeof (o as { toJSON?: unknown }).toJSON === "function") {
    return canonicalJSON((o as { toJSON: () => unknown }).toJSON());
  }
  const keys = Object.keys(o)
    .filter((k) => o[k] !== undefined && typeof o[k] !== "function")
    .sort(compareBytes);
  return "{" + keys.map((k) => goJSONString(k) + ":" + canonicalJSON(o[k])).join(",") + "}";
}

const HEX = "0123456789abcdef";

/** Lower-case hex of bytes. */
export function toHex(bytes: Uint8Array): string {
  let s = "";
  for (const b of bytes) s += HEX[b >> 4]! + HEX[b & 15]!;
  return s;
}

/** SHA-256 of a string (UTF-8) or bytes, as lower-case hex. */
export async function sha256Hex(data: string | Uint8Array): Promise<string> {
  const bytes = typeof data === "string" ? new TextEncoder().encode(data) : data;
  const digest = await crypto.subtle.digest("SHA-256", bytes);
  return toHex(new Uint8Array(digest));
}

/** SHA-256 hex of the canonical JSON of v. */
export function hashCanonical(v: unknown): Promise<string> {
  return sha256Hex(canonicalJSON(v));
}

/**
 * The idempotency key of one tool call: "orch:idem:<run>:<turn>:<tool>:<sha256
 * of the canonical JSON of the input>" (internal/orchestration/idempotency.go,
 * with canonical JSON in place of json.Marshal).
 */
export async function toolIdempotencyKey(runId: string, turnId: string, tool: string, input: unknown): Promise<string> {
  return `orch:idem:${runId}:${turnId}:${tool}:${await hashCanonical(input)}`;
}

// ---------- lossless JSON ----------

/** A JSON value in which every number is a LosslessNumber. */
export type LosslessValue = null | boolean | string | LosslessNumber | LosslessValue[] | { [k: string]: LosslessValue };

/** Parses JSON and keeps every number as a LosslessNumber (exact digits). Throws on invalid JSON. */
export function parseLossless(text: string): LosslessValue {
  return losslessParse(text) as LosslessValue;
}

/** Stringifies a value that may hold LosslessNumber or bigint values. */
export function stringifyLossless(v: unknown): string {
  return losslessStringify(v) ?? "null";
}

interface Decimal {
  neg: boolean;
  digits: string; // no leading or trailing zeros; "" for zero
  exp: number; // value = 0.digits * 10^exp
}

const NUMBER_RE = /^(-)?(\d+)(?:\.(\d+))?(?:[eE]([+-]?\d+))?$/;

function toDecimal(v: string | number | bigint | LosslessNumber): Decimal | null {
  const s = typeof v === "string" ? v.trim() : v.toString();
  const m = NUMBER_RE.exec(s);
  if (!m) return null;
  const intPart = m[2]!;
  const frac = m[3] ?? "";
  const e = m[4] === undefined ? 0 : Number(m[4]);
  let digits = intPart + frac;
  let exp = intPart.length + e;
  const lead = digits.length - digits.replace(/^0+/, "").length;
  digits = digits.slice(lead);
  exp -= lead;
  digits = digits.replace(/0+$/, "");
  if (digits === "") return { neg: false, digits: "", exp: 0 };
  return { neg: m[1] === "-", digits, exp };
}

function compareAbs(a: Decimal, b: Decimal): number {
  if (a.digits === "" || b.digits === "") return (a.digits === "" ? 0 : 1) - (b.digits === "" ? 0 : 1);
  if (a.exp !== b.exp) return a.exp < b.exp ? -1 : 1;
  const n = Math.max(a.digits.length, b.digits.length);
  const x = a.digits.padEnd(n, "0");
  const y = b.digits.padEnd(n, "0");
  return x === y ? 0 : x < y ? -1 : 1;
}

/**
 * Compares two JSON numbers exactly (Go big.Float/big.Rat semantics for
 * decimal literals). It returns -1, 0 or 1, or null when an operand is not a
 * number literal.
 */
export function compareJSONNumbers(
  a: string | number | bigint | LosslessNumber,
  b: string | number | bigint | LosslessNumber,
): -1 | 0 | 1 | null {
  const x = toDecimal(a);
  const y = toDecimal(b);
  if (x === null || y === null) return null;
  const sx = x.digits === "" ? 0 : x.neg ? -1 : 1;
  const sy = y.digits === "" ? 0 : y.neg ? -1 : 1;
  if (sx !== sy) return sx < sy ? -1 : 1;
  if (sx === 0) return 0;
  const c = compareAbs(x, y);
  const r = sx < 0 ? -c : c;
  return r < 0 ? -1 : r > 0 ? 1 : 0;
}

/** Reports whether a string is a JSON number literal. */
export function isJSONNumber(s: string): boolean {
  return /^-?(0|[1-9]\d*)(\.\d+)?([eE][+-]?\d+)?$/.test(s);
}
