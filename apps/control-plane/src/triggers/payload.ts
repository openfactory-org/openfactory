// The webhook payload pattern matcher. Port of internal/triggers/payload.go.
// Numbers are compared exactly (plan rule 12): a raw JSON body is parsed with
// lossless-json, and numbers compare as decimals (compareJSONNumbers), like Go
// math/big. A body that is already a JS value is compared as decoded.
import { compareJSONNumbers, isLosslessNumber, parseLossless } from "@openfactory/core";

export const MAX_PATTERN_DEPTH = 10;
export const MAX_PATTERN_LEAVES = 64;
export const MAX_PATTERN_VALUES = 32;

export class InvalidPatternError extends Error {
  override readonly name = "InvalidPatternError";
  constructor(path: string[], msg: string) {
    super("invalid payload pattern: " + (path.length > 0 ? path.join(".") + ": " + msg : msg));
  }
}

/** A decoded JSON value with numbers as decimal strings. */
export type JNum = { $num: string };
export type JVal = null | boolean | string | JNum | JVal[] | { [k: string]: JVal };

export function isNum(v: unknown): v is JNum {
  return typeof v === "object" && v !== null && typeof (v as JNum).$num === "string";
}
export function isObj(v: JVal | undefined): v is { [k: string]: JVal } {
  return typeof v === "object" && v !== null && !Array.isArray(v) && !isNum(v);
}

/** Converts a lossless or plain decoded value into JVal. */
export function toJVal(v: unknown): JVal {
  if (v === null || v === undefined) return null;
  if (typeof v === "boolean" || typeof v === "string") return v;
  if (typeof v === "number") return { $num: String(v) };
  if (typeof v === "bigint") return { $num: v.toString() };
  if (isLosslessNumber(v)) return { $num: v.toString() };
  if (Array.isArray(v)) return v.map(toJVal);
  if (typeof v === "object") {
    const o: { [k: string]: JVal } = {};
    for (const [k, x] of Object.entries(v as Record<string, unknown>)) o[k] = toJVal(x);
    return o;
  }
  return null;
}

/** Decodes JSON text losslessly; throws on invalid JSON. */
export function decodeJSON(text: string): JVal {
  return toJVal(parseLossless(text));
}

export function kindOf(v: JVal): string {
  if (v === null) return "null";
  if (typeof v === "boolean") return "a boolean";
  if (typeof v === "string") return "a string";
  if (isNum(v)) return "a number";
  if (Array.isArray(v)) return "a list";
  return "an object";
}

interface Leaf {
  in: JVal[];
  notIn: JVal[];
  exists?: boolean;
}
interface Field {
  key: string;
  nested?: Node;
  leaf?: Leaf;
}
interface Node {
  fields: Field[];
}

const OPERATOR_KEYS = new Set(["in", "not_in", "exists"]);

/** A pattern as raw JSON text, a decoded value, or null/undefined (matches everything). */
export type Pattern = string | unknown;

function patternValue(p: Pattern): JVal | undefined {
  if (p === undefined || p === null) return undefined;
  if (typeof p === "string") {
    const t = p.trim();
    if (t === "" || t === "null") return undefined;
    try {
      return decodeJSON(t);
    } catch {
      throw new InvalidPatternError([], "not valid JSON");
    }
  }
  return toJVal(p);
}

function parsePattern(p: Pattern): Node | null {
  const v = patternValue(p);
  if (v === undefined || v === null) return null;
  if (!isObj(v)) throw new InvalidPatternError([], "pattern must be a JSON object");
  return buildNode(v, [], 1, { n: 0 });
}

function buildNode(obj: { [k: string]: JVal }, path: string[], depth: number, leaves: { n: number }): Node {
  if (depth > MAX_PATTERN_DEPTH) throw new InvalidPatternError(path, `pattern nests deeper than ${MAX_PATTERN_DEPTH} levels`);
  const keys = Object.keys(obj).sort();
  const n: Node = { fields: [] };
  for (const k of keys) {
    const p = [...path, k];
    const v = obj[k]!;
    const f: Field = { key: k };
    if (Array.isArray(v)) {
      f.leaf = buildLeaf({ in: v }, p);
    } else if (isObj(v)) {
      const ks = Object.keys(v);
      if (ks.length > 0 && ks.every((x) => OPERATOR_KEYS.has(x))) {
        f.leaf = buildLeaf(v, p);
      } else {
        if (ks.some((x) => OPERATOR_KEYS.has(x))) throw new InvalidPatternError(p, "object mixes operators (in, not_in, exists) with field names");
        f.nested = buildNode(v, p, depth + 1, leaves);
        if (ks.length === 0) leaves.n++;
      }
    } else {
      throw new InvalidPatternError(p, `expected an array of allowed values, an operator object or a nested object, got ${kindOf(v)}`);
    }
    if (f.leaf) leaves.n++;
    if (leaves.n > MAX_PATTERN_LEAVES) throw new InvalidPatternError(p, `pattern holds more than ${MAX_PATTERN_LEAVES} leaf fields`);
    n.fields.push(f);
  }
  return n;
}

function buildLeaf(ops: { [k: string]: JVal }, path: string[]): Leaf {
  const lm: Leaf = { in: [], notIn: [] };
  if ("in" in ops) lm.in = valueSet(ops["in"]!, path, "in");
  if ("not_in" in ops) lm.notIn = valueSet(ops["not_in"]!, path, "not_in");
  if ("exists" in ops && ops["exists"] !== null) {
    if (typeof ops["exists"] !== "boolean") throw new InvalidPatternError(path, "exists must be true or false");
    lm.exists = ops["exists"];
  }
  for (const a of lm.in) for (const b of lm.notIn) if (scalarEqual(a, b)) throw new InvalidPatternError(path, `value ${scalarText(a)} appears in both in and not_in`);
  if (lm.exists === false && lm.in.length > 0) throw new InvalidPatternError(path, "exists: false cannot be combined with a non-empty in");
  return lm;
}

function valueSet(v: JVal, path: string[], op: string): JVal[] {
  if (v === null) return [];
  if (!Array.isArray(v)) throw new InvalidPatternError(path, `${op} must be an array of values`);
  if (v.length > MAX_PATTERN_VALUES) throw new InvalidPatternError(path, `${op} lists more than ${MAX_PATTERN_VALUES} values`);
  const out: JVal[] = [];
  for (const e of v) {
    if (!(e === null || typeof e === "string" || typeof e === "boolean" || isNum(e))) {
      throw new InvalidPatternError(path, `${op} values must be strings, numbers, booleans or null, got ${kindOf(e)}`);
    }
    if (!out.some((x) => scalarEqual(x, e))) out.push(e);
  }
  return out;
}

function scalarEqual(a: JVal, b: JVal): boolean {
  if (a === null) return b === null;
  if (typeof a === "string" || typeof a === "boolean") return a === b;
  if (isNum(a)) return isNum(b) && (a.$num === b.$num || compareJSONNumbers(a.$num, b.$num) === 0);
  return false;
}

function scalarText(v: JVal): string {
  if (isNum(v)) return v.$num;
  return JSON.stringify(v);
}

function overlaps(v: JVal, set: JVal[]): boolean {
  if (Array.isArray(v)) return v.some((e) => overlaps(e, set));
  return set.some((s) => scalarEqual(v, s));
}

function leafMatch(l: Leaf, v: JVal | undefined, present: boolean): boolean {
  if (l.exists !== undefined && l.exists !== present) return false;
  if (l.in.length > 0 && (!present || !overlaps(v!, l.in))) return false;
  if (l.notIn.length > 0 && present && overlaps(v!, l.notIn)) return false;
  return true;
}

function matchNested(n: Node, v: JVal | undefined, present: boolean): boolean {
  if (!present || v === undefined) return false;
  if (isObj(v)) return matchObject(n, v);
  if (Array.isArray(v)) return v.some((e) => isObj(e) && matchObject(n, e));
  return false;
}

function matchObject(n: Node, obj: { [k: string]: JVal }): boolean {
  for (const f of n.fields) {
    const present = Object.prototype.hasOwnProperty.call(obj, f.key);
    const v = present ? obj[f.key] : undefined;
    if (f.nested) {
      if (!matchNested(f.nested, v, present)) return false;
    } else if (!leafMatch(f.leaf!, v, present)) {
      return false;
    }
  }
  return true;
}

/** Checks a payload pattern without evaluating it. Throws InvalidPatternError. */
export function validatePayloadPattern(pattern: Pattern): void {
  parsePattern(pattern);
}

/**
 * Evaluates a pattern against a body: raw JSON text or bytes (parsed
 * losslessly) or a decoded value. Throws for an invalid pattern or a body that
 * is not JSON; callers fail closed.
 */
export function matchPayload(pattern: Pattern, body: string | Uint8Array | unknown): boolean {
  const node = parsePattern(pattern);
  if (!node || node.fields.length === 0) return true;
  let doc: JVal;
  if (typeof body === "string" || body instanceof Uint8Array) {
    const text = typeof body === "string" ? body : new TextDecoder().decode(body);
    try {
      doc = decodeJSON(text);
    } catch (e) {
      throw new Error(`payload is not valid JSON: ${(e as Error).message}`);
    }
  } else {
    if (body === undefined) throw new Error("payload is not valid JSON: empty");
    doc = toJVal(body);
  }
  return matchNested(node, doc, true);
}
