// {{NAME}} placeholders and redaction. Port of internal/secrets/expand.go.
import { OfError } from "@openfactory/core";

const PLACEHOLDER_RE = /\{\{\s*([A-Za-z_][A-Za-z0-9_]*)\s*\}\}/g;

/** Distinct names referenced in s, in order of appearance. */
export function placeholders(s: string): string[] {
  const out: string[] = [];
  for (const m of s.matchAll(PLACEHOLDER_RE)) if (!out.includes(m[1]!)) out.push(m[1]!);
  return out;
}

/** Lookup returns undefined for a missing secret. */
export type SecretLookup = (name: string) => Promise<string | undefined> | string | undefined;

/** ExpandWith: one lookup per name; missing names reported together (resource_not_found). */
export async function expandWith(s: string, lookup: SecretLookup): Promise<string> {
  const names = placeholders(s);
  if (names.length === 0) return s;
  const values = new Map<string, string>();
  const missing: string[] = [];
  for (const n of names) {
    const v = await lookup(n);
    if (v === undefined) missing.push(n);
    else values.set(n, v);
  }
  if (missing.length) throw new OfError("resource_not_found", `unknown secret(s) referenced: ${missing.join(", ")}`);
  return s.replace(PLACEHOLDER_RE, (_m, n: string) => values.get(n)!);
}

export async function expandMap(m: Record<string, string> | undefined, lookup: SecretLookup): Promise<Record<string, string> | undefined> {
  if (!m) return undefined;
  const out: Record<string, string> = {};
  for (const [k, v] of Object.entries(m)) {
    try {
      out[k] = await expandWith(v, lookup);
    } catch (e) {
      if (e instanceof OfError) throw new OfError(e.code, `${k}: ${e.detail}`);
      throw e;
    }
  }
  return out;
}

export const REDACTED_MARKER = "[REDACTED]";
const MIN_REDACT_LEN = 4;

/** Redact: longer values first; values shorter than 4 characters are kept. */
export function redact(text: string, values: string[]): string {
  if (!text || values.length === 0) return text;
  const vs = values.filter((v) => v.length >= MIN_REDACT_LEN).sort((a, b) => b.length - a.length);
  for (const v of vs) text = text.split(v).join(REDACTED_MARKER);
  return text;
}
