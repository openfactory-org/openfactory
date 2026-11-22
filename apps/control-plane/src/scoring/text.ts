// Text helpers of internal/scoring. Go counts bytes; so do these (UTF-8), and
// they never cut a character in half.
const enc = new TextEncoder();
const dec = new TextDecoder();

export const byteLength = (s: string): number => enc.encode(s).length;

const isRuneStart = (b: number): boolean => (b & 0xc0) !== 0x80;

/** Shortens to at most n bytes (at a character boundary) and notes how many bytes were cut. */
export function truncate(s: string, n: number): string {
  const b = enc.encode(s);
  if (b.length <= n) return s;
  let cut = n;
  while (cut > 0 && !isRuneStart(b[cut]!)) cut--;
  return dec.decode(b.subarray(0, cut)) + `… [truncated ${b.length - cut} bytes]`;
}

/** The first n bytes, at a character boundary, without a note (clip in Go). */
export function clip(s: string, n: number): string {
  const b = enc.encode(s);
  if (b.length <= n) return s;
  let cut = n;
  while (cut > 0 && !isRuneStart(b[cut]!)) cut--;
  return dec.decode(b.subarray(0, cut));
}

/** Keeps the head and the tail of s within max bytes. */
export function capMiddle(s: string, max: number): string {
  if (max <= 0) return s;
  const b = enc.encode(s);
  if (b.length <= max) return s;
  const marker = `\n\n… [transcript truncated: ${b.length - max} bytes omitted] …\n\n`;
  const keep = max - enc.encode(marker).length;
  if (keep < 2) return dec.decode(b.subarray(0, max));
  let head = Math.floor(keep / 2);
  const tail = keep - head;
  while (head > 0 && !isRuneStart(b[head]!)) head--;
  let start = b.length - tail;
  while (start < b.length && !isRuneStart(b[start]!)) start++;
  return dec.decode(b.subarray(0, head)) + marker + dec.decode(b.subarray(start));
}

/** Collapses all white space to single spaces. */
export function oneLine(s: string): string {
  return s.split(/\s+/).filter(Boolean).join(" ");
}

/** Go's %.2f. */
export const f2 = (n: number): string => n.toFixed(2);
