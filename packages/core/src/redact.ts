// Secret redaction. Port of internal/redact. The runner (Go) and the control
// plane (TS) give the same result: both test against
// schemas/testdata/redact-vectors.json.
//
// For each secret value the redactor matches the value, its JSON-escaped form
// (Go json.Marshal escaping), its standard and URL-safe base64 forms (with and
// without padding) and its URL-encoded forms, and replaces each match with
// REDACT_MARK. Redaction is a second barrier: a secret that an agent changes in
// another way (hex, split) passes.

import { canonicalJSON } from "./canonical";
import { base64Std, base64URL, byteLength, goJSONString, pathEscape, queryEscape, utf8 } from "./gostd";

/** Replaces each secret value. */
export const REDACT_MARK = "***";
/**
 * The length (UTF-8 bytes) below which a value is ignored by default, and
 * below which the encoded forms of a value are not made.
 */
export const REDACT_MIN_LEN = 8;

/**
 * The strings to remove for values, longest (in bytes) first, so that a
 * secret inside a longer secret leaves no rest. A value shorter than minLen is
 * ignored. A value of REDACT_MIN_LEN or more also gives its encoded forms.
 */
export function redactTerms(values: readonly string[], minLen: number): string[] {
  const seen = new Set<string>();
  const list: string[] = [];
  const add = (s: string): void => {
    if (s !== "" && !seen.has(s)) {
      seen.add(s);
      list.push(s);
    }
  };
  for (const v of values) {
    const n = byteLength(v);
    if (v === "" || n < minLen) continue;
    add(v);
    const esc = goJSONString(v);
    if (esc.length > 2) add(esc.slice(1, -1));
    if (n < REDACT_MIN_LEN) continue;
    const b = utf8(v);
    add(base64Std(b, true));
    add(base64Std(b, false));
    add(base64URL(b, true));
    add(base64URL(b, false));
    add(queryEscape(v));
    add(pathEscape(v));
  }
  // Array.prototype.sort is stable, like sort.SliceStable.
  return list
    .map((s) => ({ s, n: byteLength(s) }))
    .sort((a, b) => b.n - a.n)
    .map((x) => x.s);
}

/** Replaces each term of the list (from redactTerms) with REDACT_MARK. */
export function redactString(s: string, terms: readonly string[]): string {
  for (const t of terms) s = s.split(t).join(REDACT_MARK);
  return s;
}

const SAFE_JSON = '{"redacted":true}';

function validJSON(s: string): boolean {
  try {
    JSON.parse(s);
    return true;
  } catch {
    return false;
  }
}

/** Replaces each term in a JSON text. If the result is not valid JSON, it returns {"redacted":true}. */
export function redactJSON(raw: string, terms: readonly string[]): string {
  if (terms.length === 0) return raw;
  const s = redactString(raw, terms);
  return validJSON(s) ? s : SAFE_JSON;
}

/** A growing set of secret values. */
export class Redactor {
  readonly #min: number;
  readonly #values = new Set<string>();
  #terms: string[] = [];

  /** A redactor that ignores values shorter than minLen bytes (default REDACT_MIN_LEN). */
  constructor(values: readonly string[] = [], minLen = REDACT_MIN_LEN) {
    this.#min = minLen;
    this.add(...values);
  }

  /** Adds secret values. */
  add(...values: string[]): void {
    let changed = false;
    for (const v of values) {
      if (v !== "" && byteLength(v) >= this.#min && !this.#values.has(v)) {
        this.#values.add(v);
        changed = true;
      }
    }
    if (changed) this.#terms = redactTerms([...this.#values], this.#min);
  }

  /** The number of secret values. */
  get size(): number {
    return this.#values.size;
  }

  /** The current term list. */
  get terms(): readonly string[] {
    return this.#terms;
  }

  /** Removes the secret values from s. */
  string(s: string): string {
    return s === "" ? s : redactString(s, this.#terms);
  }

  /** Removes the secret values from a JSON document. */
  json(raw: string): string {
    return redactJSON(raw, this.#terms);
  }

  /**
   * Removes the secret values from the strings of m, also in nested values.
   * It returns m itself if the redactor has no value. A key that holds a
   * secret changes too.
   */
  data<T extends Record<string, unknown>>(m: T): T | { redacted: true } {
    if (this.#terms.length === 0 || Object.keys(m).length === 0) return m;
    const out = redactJSON(canonicalJSON(m), this.#terms);
    try {
      const parsed: unknown = JSON.parse(out);
      if (parsed !== null && typeof parsed === "object" && !Array.isArray(parsed)) return parsed as T;
    } catch {
      // fall through
    }
    return { redacted: true };
  }
}
