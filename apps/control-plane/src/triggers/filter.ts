// Trigger filter parsing and evaluation. Port of internal/triggers/filter.go.
import { compareBytes } from "@openfactory/core";
import {
  CATALOG,
  KEY_PAYLOAD,
  KEY_SKIP_BOTS,
  SKIP_BOTS_ALL,
  SKIP_BOTS_NONE,
  SKIP_BOTS_SELF,
  type CmpKind,
  type FieldSpec,
} from "./catalog";
import { matchGlob, validateGlob } from "./glob";
import { decodeJSON, isNum, isObj, kindOf, toJVal, validatePayloadPattern, type JVal } from "./payload";
import { EVENT_PROVIDER } from "@openfactory/core";

export class InvalidFilterError extends Error {
  override readonly name = "InvalidFilterError";
  constructor(msg: string) {
    super("invalid trigger filter: " + msg);
  }
}

const filterErr = (msg: string) => new InvalidFilterError(msg);

export type ParseMode = "match" | "author";

export interface Condition {
  key: string;
  spec: FieldSpec;
  in: string[];
  notIn: string[];
}

export interface ParsedFilter {
  conds: Condition[];
  /** github only; defaults to "all". */
  skipBots: string;
  /** The payload pattern (decoded), or undefined. */
  payload?: JVal;
}

const goQ = (s: string) => JSON.stringify(s);

/** A filter as raw JSON text, a decoded value, or empty. */
export function filterValue(raw: unknown): JVal | undefined {
  if (raw === undefined || raw === null) return undefined;
  if (typeof raw === "string") {
    const t = raw.trim();
    if (t === "" || t === "null") return undefined;
    if (t[0] !== "{") throw filterErr("filter must be an object");
    try {
      return decodeJSON(t);
    } catch (e) {
      throw filterErr(`filter is not valid JSON: ${(e as Error).message}`);
    }
  }
  if (raw instanceof Uint8Array) return filterValue(new TextDecoder().decode(raw));
  return toJVal(raw);
}

const isNull = (v: JVal | undefined): boolean => v === undefined || v === null;

/** Validates a filter against the provider and event catalog. */
export function parseFilter(provider: string, event: string, raw: unknown, mode: ParseMode): ParsedFilter {
  const ps = CATALOG[provider];
  if (!ps) throw filterErr(`unknown provider ${goQ(provider)}`);
  const es = Object.prototype.hasOwnProperty.call(ps.events, event) ? ps.events[event] : undefined;
  if (!es) throw filterErr(`unknown event ${goQ(event)} for provider ${goQ(provider)}`);
  if (mode === "author" && es.notAuthorable)
    throw filterErr(`event ${provider}.${event} is not available for authoring automations`);
  const pf: ParsedFilter = { conds: [], skipBots: provider === EVENT_PROVIDER.GitHub ? SKIP_BOTS_ALL : "" };
  const v = filterValue(raw);
  let obj: { [k: string]: JVal } = {};
  if (!isNull(v)) {
    if (!isObj(v)) throw filterErr("filter must be an object");
    obj = v;
  }
  if (provider === EVENT_PROVIDER.Schedule && mode === "author" && Object.keys(obj).length > 0)
    throw filterErr("schedule triggers accept no filter");
  const names = Object.keys(obj).sort(compareBytes);
  const seenNative = new Map<string, string>();
  for (const key of names) {
    const val = obj[key]!;
    if (key === KEY_SKIP_BOTS) {
      if (!es.keys.includes(KEY_SKIP_BOTS))
        throw filterErr(`key ${goQ(key)} is not supported for ${provider}.${event}`);
      const sb = parseSkipBots(val);
      if (sb !== "") pf.skipBots = sb;
      continue;
    }
    if (key === KEY_PAYLOAD) {
      if (!es.keys.includes(KEY_PAYLOAD)) throw filterErr(`key ${goQ(key)} is not supported for ${provider}.${event}`);
      validatePayloadPattern(val);
      if (!isNull(val)) pf.payload = val;
      continue;
    }
    let spec = ps.fields[key];
    let canonical = key;
    if (!spec) {
      const as = ps.aliases[key];
      if (!as) throw filterErr(`unknown key ${goQ(key)} for ${provider}.${event}`);
      spec = as;
      canonical = as.native!;
    }
    if (!es.keys.includes(canonical)) throw filterErr(`key ${goQ(key)} is not supported for ${provider}.${event}`);
    if (spec.serverManaged && mode === "author") throw filterErr(`key ${goQ(key)} is server-managed and cannot be set`);
    const prev = seenNative.get(canonical);
    if (prev !== undefined) throw filterErr(`keys ${goQ(prev)} and ${goQ(key)} select the same field; use only one`);
    seenNative.set(canonical, key);
    const cond = parseCondition(key, spec, val);
    if (cond.in.length === 0 && cond.notIn.length === 0) continue;
    pf.conds.push(cond);
  }
  for (const k of es.keys) {
    const spec = ps.fields[k];
    if (!spec?.required) continue;
    const found = pf.conds.some(
      (c) => ((c.spec.native === undefined && c.key === k) || c.spec.native === k) && c.in.length > 0,
    );
    if (!found) throw filterErr(`key ${goQ(k)} is required for ${provider}.${event}`);
  }
  return pf;
}

function parseSkipBots(raw: JVal): string {
  if (raw === null) return "";
  if (typeof raw !== "string") throw filterErr("skip_bots must be one of all, self, none");
  if (raw === SKIP_BOTS_ALL || raw === SKIP_BOTS_SELF || raw === SKIP_BOTS_NONE) return raw;
  throw filterErr(`skip_bots must be one of all, self, none (got ${goQ(raw)})`);
}

function parseCondition(key: string, spec: FieldSpec, raw: JVal): Condition {
  const c: Condition = { key, spec, in: [], notIn: [] };
  if (raw === null) return c;
  let inRaw: JVal | undefined;
  let notInRaw: JVal | undefined;
  if (Array.isArray(raw)) {
    inRaw = raw;
  } else if (isObj(raw)) {
    if (spec.plainListOnly) throw filterErr(`key ${goQ(key)} accepts a plain list only`);
    for (const [op, v] of Object.entries(raw)) {
      if (op === "in") inRaw = v;
      else if (op === "not_in") notInRaw = v;
      else throw filterErr(`key ${goQ(key)}: unknown operator ${goQ(op)}`);
    }
  } else {
    throw filterErr(`key ${goQ(key)} must be a list or a matcher object`);
  }
  c.in = parseValues(key, spec, inRaw);
  c.notIn = parseValues(key, spec, notInRaw);
  if (spec.inOnly && c.notIn.length > 0) throw filterErr(`key ${goQ(key)} supports only in`);
  if (spec.exactlyOne && c.in.length !== 1 && (c.in.length > 0 || spec.required))
    throw filterErr(`key ${goQ(key)} requires exactly one value`);
  for (const v of c.in)
    if (c.notIn.includes(v)) throw filterErr(`key ${goQ(key)} lists ${goQ(v)} in both in and not_in`);
  return c;
}

function parseValues(key: string, spec: FieldSpec, raw: JVal | undefined): string[] {
  if (isNull(raw)) return [];
  if (!Array.isArray(raw)) throw filterErr(`key ${goQ(key)}: values must be a list`);
  const out: string[] = [];
  for (const it of raw) {
    const v = parseValue(key, spec, it);
    if (spec.enumValues && !spec.enumValues.includes(v)) {
      throw filterErr(`key ${goQ(key)}: unsupported value ${goQ(v)} (allowed: ${spec.enumValues.join(", ")})`);
    }
    if (!out.includes(v)) out.push(v);
  }
  return out;
}

const INT64_MAX = (1n << 63n) - 1n;

function parseValue(key: string, spec: FieldSpec, v: JVal): string {
  if (spec.cmp === "int") {
    if (!isNum(v)) throw filterErr(`key ${goQ(key)} takes integers, not ${kindOf(v)}`);
    const n = v.$num;
    if (!/^[+-]?\d+$/.test(n) || BigInt(n) <= 0n || BigInt(n) > INT64_MAX)
      throw filterErr(`key ${goQ(key)} takes positive integers (got ${n})`);
    return BigInt(n).toString();
  }
  let s: string;
  if (typeof v === "string") s = v;
  else if (isNum(v)) s = v.$num;
  else throw filterErr(`key ${goQ(key)} takes strings, not ${kindOf(v)}`);
  s = normalize(spec.cmp, s);
  if (s === "" && spec.cmp !== "noop") throw filterErr(`key ${goQ(key)} contains an empty value`);
  if (spec.cmp === "glob") {
    const err = validateGlob(s);
    if (err) throw filterErr(`key ${goQ(key)}: ${err}`);
  }
  return s;
}

/** Canonicalizes a filter or attribute value for comparison. */
export function normalize(k: CmpKind, s: string): string {
  switch (k) {
    case "fold":
    case "keyword":
      return s.trim().toLowerCase();
    case "emoji":
      return normalizeEmoji(s);
    case "branch": {
      const t = s.trim();
      return t.startsWith("refs/heads/") ? t.slice("refs/heads/".length) : t;
    }
    case "channelName": {
      const t = s.trim();
      return (t.startsWith("#") ? t.slice(1) : t).trim();
    }
    case "channelNameFold": {
      const t = s.trim();
      return (t.startsWith("#") ? t.slice(1) : t).trim().toLowerCase();
    }
    case "glob":
      return s.trim().replace(/^\//, "");
    case "noop":
      return s;
    default:
      return s.trim();
  }
}

/** ":thumbsup::skin-tone-3:" -> "thumbsup". */
export function normalizeEmoji(s: string): string {
  let t = s.trim().toLowerCase();
  let i = t.indexOf("::skin-tone-");
  if (i >= 0) t = t.slice(0, i);
  t = t.replace(/^:+|:+$/g, "");
  i = t.indexOf(":skin-tone-");
  if (i >= 0) t = t.slice(0, i);
  return t;
}

/** Whether attrs and text satisfy c. Throws for an attribute of an unsupported type. */
export function evalCondition(attrs: Record<string, unknown>, text: string, c: Condition): boolean {
  if (c.spec.cmp === "noop") return true;
  let values: string[];
  if (c.spec.cmp === "keyword") values = text !== "" ? [text.toLowerCase()] : [];
  else values = collect(attrs, c.spec.attrs, c.spec.cmp).values;
  if (c.in.length > 0) {
    let inValues = values;
    if (c.spec.addedAttr) {
      const added = collect(attrs, [c.spec.addedAttr], c.spec.cmp);
      if (added.present) inValues = added.values;
    }
    if (!anyMatch(c.spec.cmp, c.in, inValues)) return false;
  }
  if (c.notIn.length > 0 && anyMatch(c.spec.cmp, c.notIn, values)) return false;
  return true;
}

function anyMatch(k: CmpKind, patterns: string[], values: string[]): boolean {
  for (const v of values) {
    for (const p of patterns) {
      if (k === "glob" ? matchGlob(p, v) : k === "keyword" ? v.includes(p) : p === v) return true;
    }
  }
  return false;
}

/** The normalized values of the named attributes (union); present = at least one key exists. */
export function collect(
  attrs: Record<string, unknown>,
  names: string[],
  k: CmpKind,
): { values: string[]; present: boolean } {
  const values: string[] = [];
  let present = false;
  for (const n of names) {
    const raw = attrs[n];
    if (raw === undefined || raw === null) continue;
    present = true;
    let vs: string[];
    try {
      vs = toStrings(raw);
    } catch (e) {
      throw new Error(`attribute ${goQ(n)}: ${(e as Error).message}`, { cause: e });
    }
    for (let v of vs) {
      v = k === "int" ? canonicalInt(v) : normalize(k, v);
      if (v !== "") values.push(v);
    }
  }
  return { values, present };
}

function canonicalInt(s: string): string {
  if (/^[+-]?\d+$/.test(s)) {
    try {
      const b = BigInt(s);
      if (b <= INT64_MAX && b >= -(1n << 63n)) return b.toString();
    } catch {
      /* fall through */
    }
  }
  const f = Number(s);
  if (s.trim() !== "" && Number.isFinite(f) && Math.trunc(f) === f) return BigInt(f).toString();
  return s;
}

function toStrings(v: unknown): string[] {
  if (Array.isArray(v)) return v.map(scalarString);
  return [scalarString(v)];
}

function formatFloat(n: number): string {
  if (Number.isInteger(n) && Math.abs(n) < 1e21) return BigInt(n).toString();
  return String(n);
}

function scalarString(v: unknown): string {
  switch (typeof v) {
    case "string":
      return v;
    case "number":
      return formatFloat(v);
    case "bigint":
      return v.toString();
    case "boolean":
      return v ? "true" : "false";
  }
  if (isNum(v)) return v.$num;
  if (
    typeof v === "object" &&
    v !== null &&
    typeof (v as { isLosslessNumber?: unknown }).isLosslessNumber === "boolean"
  )
    return String(v);
  throw new Error(
    `unsupported attribute value type ${v === null ? "nil" : Array.isArray(v) ? "[]interface {}" : "map[string]interface {}"}`,
  );
}

/** Go attrBool: true for a bool true or a string strconv.ParseBool accepts as true. */
export function attrBool(attrs: Record<string, unknown>, name: string): boolean {
  const x = attrs[name];
  if (typeof x === "boolean") return x;
  if (typeof x === "string") return ["1", "t", "T", "TRUE", "true", "True"].includes(x);
  return false;
}
