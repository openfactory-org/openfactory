// Go formatting helpers. Validation messages of the Go compiler use fmt verbs
// (%q, %v); these helpers produce the same text so that messages port exactly.

const printRe = /^[\p{L}\p{M}\p{N}\p{P}\p{S}]$/u;

function hex(n: number, width: number): string {
  return n.toString(16).padStart(width, "0");
}

/** goQuote formats s like Go's strconv.Quote (the %q verb for strings). */
export function goQuote(s: string): string {
  let out = '"';
  for (const ch of s) {
    const cp = ch.codePointAt(0) ?? 0;
    if (ch === '"') {
      out += '\\"';
    } else if (ch === "\\") {
      out += "\\\\";
    } else if (cp === 0x20 || (cp > 0x20 && cp !== 0x7f && printRe.test(ch))) {
      out += ch;
    } else {
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
          if (cp < 0x80) out += "\\x" + hex(cp, 2);
          else if (cp >= 0xd800 && cp <= 0xdfff) out += "\\ufffd";
          else if (cp < 0x10000) out += "\\u" + hex(cp, 4);
          else out += "\\U" + hex(cp, 8);
      }
    }
  }
  return out + '"';
}

/** goFloat formats a float64 like Go's %v verb (strconv 'g', shortest). */
export function goFloat(f: number): string {
  if (Number.isNaN(f)) return "NaN";
  if (f === Infinity) return "+Inf";
  if (f === -Infinity) return "-Inf";
  if (f === 0) return Object.is(f, -0) ? "-0" : "0";
  const neg = f < 0;
  const [mant = "0", expStr = "0"] = Math.abs(f).toExponential().split("e");
  const digits = mant.replace(".", "");
  const exp = Number(expStr);
  let s: string;
  if (exp < -4 || exp >= 6) {
    const m = digits.length > 1 ? digits[0] + "." + digits.slice(1) : digits;
    const e = Math.abs(exp);
    s = m + "e" + (exp < 0 ? "-" : "+") + (e < 10 ? "0" + e : String(e));
  } else if (exp < 0) {
    s = "0." + "0".repeat(-exp - 1) + digits;
  } else if (digits.length <= exp + 1) {
    s = digits + "0".repeat(exp + 1 - digits.length);
  } else {
    s = digits.slice(0, exp + 1) + "." + digits.slice(exp + 1);
  }
  return neg ? "-" + s : s;
}

/** goTimeQuote formats s like the quote helper of Go's time package. */
export function goTimeQuote(s: string): string {
  let out = '"';
  const bytes = new TextEncoder().encode(s);
  let i = 0;
  for (const ch of s) {
    const cp = ch.codePointAt(0) ?? 0;
    const width = new TextEncoder().encode(ch).length;
    if (cp >= 0x80 || cp < 0x20) {
      for (let j = 0; j < width; j++) out += "\\x" + hex(bytes[i + j] ?? 0, 2);
    } else {
      if (ch === '"' || ch === "\\") out += "\\";
      out += ch;
    }
    i += width;
  }
  return out + '"';
}

const space = "\\t\\n\\v\\f\\r \\u0085\\u00a0\\u1680\\u2000-\\u200a\\u2028\\u2029\\u202f\\u205f\\u3000";
const fieldsRe = new RegExp(`[${space}]+`);
const leadRe = new RegExp(`^[${space}]+`);
const trailRe = new RegExp(`[${space}]+$`);

/** goFields splits like strings.Fields (Unicode white space). */
export function goFields(s: string): string[] {
  return s.split(fieldsRe).filter((x) => x !== "");
}

/** goTrimSpace trims like strings.TrimSpace. */
export function goTrimSpace(s: string): string {
  return s.replace(leadRe, "").replace(trailRe, "");
}
