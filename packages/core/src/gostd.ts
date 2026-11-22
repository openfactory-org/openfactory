// Small ports of Go standard library behavior that results must match byte for
// byte: encoding/json string escaping, net/url escaping, strconv.Quote (%q),
// and UTF-8 byte lengths. Everything runs in Workers (no node: imports).

const encoder = new TextEncoder();

/** The UTF-8 byte length of s (Go len(s)). Lone surrogates count as U+FFFD. */
export function byteLength(s: string): number {
  return encoder.encode(s).length;
}

/** The UTF-8 bytes of s. */
export function utf8(s: string): Uint8Array {
  return encoder.encode(s);
}

/** Reports whether s has no lone surrogate (Go utf8.ValidString of the encoded form). */
export function isWellFormed(s: string): boolean {
  for (let i = 0; i < s.length; i++) {
    const c = s.charCodeAt(i);
    if (c >= 0xd800 && c <= 0xdbff) {
      const n = s.charCodeAt(i + 1);
      if (!(n >= 0xdc00 && n <= 0xdfff)) return false;
      i++;
    } else if (c >= 0xdc00 && c <= 0xdfff) {
      return false;
    }
  }
  return true;
}

const HEX = "0123456789abcdef";

/**
 * The JSON form of a string as Go json.Marshal writes it, with the quotes:
 * `"`, `\`, control characters, `<`, `>`, `&`, U+2028 and U+2029 are escaped;
 * \b, \f, \n, \r and \t use their short forms; a lone surrogate becomes
 * �.
 */
export function goJSONString(s: string): string {
  let out = '"';
  for (let i = 0; i < s.length; i++) {
    const c = s.charCodeAt(i);
    switch (c) {
      case 0x22:
        out += '\\"';
        continue;
      case 0x5c:
        out += "\\\\";
        continue;
      case 0x08:
        out += "\\b";
        continue;
      case 0x0c:
        out += "\\f";
        continue;
      case 0x0a:
        out += "\\n";
        continue;
      case 0x0d:
        out += "\\r";
        continue;
      case 0x09:
        out += "\\t";
        continue;
    }
    if (c < 0x20 || c === 0x3c || c === 0x3e || c === 0x26) {
      out += "\\u00" + HEX[c >> 4] + HEX[c & 15];
      continue;
    }
    if (c === 0x2028 || c === 0x2029) {
      out += c === 0x2028 ? "\\u2028" : "\\u2029";
      continue;
    }
    if (c >= 0xd800 && c <= 0xdbff) {
      const n = s.charCodeAt(i + 1);
      if (n >= 0xdc00 && n <= 0xdfff) {
        out += s[i]! + s[i + 1]!;
        i++;
        continue;
      }
      out += "\\ufffd";
      continue;
    }
    if (c >= 0xdc00 && c <= 0xdfff) {
      out += "\\ufffd";
      continue;
    }
    out += s[i]!;
  }
  return out + '"';
}

type EscapeMode = "query" | "path";

function shouldEscape(c: number, mode: EscapeMode): boolean {
  if ((c >= 0x61 && c <= 0x7a) || (c >= 0x41 && c <= 0x5a) || (c >= 0x30 && c <= 0x39)) return false;
  switch (c) {
    case 0x2d: // -
    case 0x5f: // _
    case 0x2e: // .
    case 0x7e: // ~
      return false;
    case 0x24: // $
    case 0x26: // &
    case 0x2b: // +
    case 0x2c: // ,
    case 0x2f: // /
    case 0x3a: // :
    case 0x3b: // ;
    case 0x3d: // =
    case 0x3f: // ?
    case 0x40: // @
      if (mode === "path") return c === 0x2f || c === 0x3b || c === 0x2c || c === 0x3f;
      return true;
  }
  return true;
}

const UPPER_HEX = "0123456789ABCDEF";

function escape(s: string, mode: EscapeMode): string {
  let out = "";
  for (const b of encoder.encode(s)) {
    if (b === 0x20 && mode === "query") out += "+";
    else if (shouldEscape(b, mode)) out += "%" + UPPER_HEX[b >> 4] + UPPER_HEX[b & 15];
    else out += String.fromCharCode(b);
  }
  return out;
}

/** Go url.QueryEscape. */
export function queryEscape(s: string): string {
  return escape(s, "query");
}

/** Go url.PathEscape (one path segment). */
export function pathEscape(s: string): string {
  return escape(s, "path");
}

/** Go url.Values.Encode: keys sorted, each key and value query-escaped. */
export function encodeQuery(values: Record<string, string | readonly string[]>): string {
  const parts: string[] = [];
  for (const k of Object.keys(values).sort()) {
    const v = values[k]!;
    for (const one of typeof v === "string" ? [v] : v) parts.push(queryEscape(k) + "=" + queryEscape(one));
  }
  return parts.join("&");
}

const printable = /^[\p{L}\p{M}\p{N}\p{P}\p{S}]$/u;

/** Go strconv.Quote, the %q verb of a string. */
export function goQuote(s: string): string {
  let out = '"';
  for (const ch of s) {
    const cp = ch.codePointAt(0)!;
    if (ch === '"') out += '\\"';
    else if (ch === "\\") out += "\\\\";
    else if (cp >= 0xd800 && cp <= 0xdfff) out += "\\ufffd";
    else if (cp === 0x20 || printable.test(ch)) out += ch;
    else {
      switch (cp) {
        case 7:
          out += "\\a";
          break;
        case 8:
          out += "\\b";
          break;
        case 12:
          out += "\\f";
          break;
        case 10:
          out += "\\n";
          break;
        case 13:
          out += "\\r";
          break;
        case 9:
          out += "\\t";
          break;
        case 11:
          out += "\\v";
          break;
        default:
          if (cp < 0x20 || cp === 0x7f) out += "\\x" + cp.toString(16).padStart(2, "0");
          else if (cp < 0x10000) out += "\\u" + cp.toString(16).padStart(4, "0");
          else out += "\\U" + cp.toString(16).padStart(8, "0");
      }
    }
  }
  return out + '"';
}

/** Go strings.Fields joined by one space. */
export function oneLine(s: string): string {
  return s
    .split(/\s+/u)
    .filter((x) => x !== "")
    .join(" ");
}

/** Go strings.Trim(s, cutset). */
export function trimChars(s: string, cutset: string): string {
  let a = 0;
  let b = s.length;
  while (a < b && cutset.includes(s[a]!)) a++;
  while (b > a && cutset.includes(s[b - 1]!)) b--;
  return s.slice(a, b);
}

/** Go strings.Cut. */
export function cut(s: string, sep: string): [before: string, after: string, found: boolean] {
  const i = s.indexOf(sep);
  if (i < 0) return [s, "", false];
  return [s.slice(0, i), s.slice(i + sep.length), true];
}

/** Go strings.EqualFold for the ASCII and simple Unicode cases. */
export function equalFold(a: string, b: string): boolean {
  return a.toLowerCase() === b.toLowerCase() || a.toUpperCase() === b.toUpperCase();
}

/** Go sort.Strings order: byte order of the UTF-8 forms. */
export function compareBytes(a: string, b: string): number {
  if (a === b) return 0;
  const x = encoder.encode(a);
  const y = encoder.encode(b);
  const n = Math.min(x.length, y.length);
  for (let i = 0; i < n; i++) if (x[i] !== y[i]) return x[i]! - y[i]!;
  return x.length - y.length;
}

/** Sorted copy in Go sort.Strings order. */
export function sortStrings(list: Iterable<string>): string[] {
  return [...list].sort(compareBytes);
}

/** Standard base64 of bytes, with padding. */
export function base64Std(bytes: Uint8Array, pad = true): string {
  let bin = "";
  for (const b of bytes) bin += String.fromCharCode(b);
  const s = btoa(bin);
  return pad ? s : s.replace(/=+$/, "");
}

/** URL-safe base64 of bytes. */
export function base64URL(bytes: Uint8Array, pad = true): string {
  return base64Std(bytes, pad).replace(/\+/g, "-").replace(/\//g, "_");
}
