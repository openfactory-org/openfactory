// Text helpers of internal/scoring. Go counts bytes; so do these (UTF-8), and
// they never cut a character in half. clipBytes, oneLine and byteLength are in
// @openfactory/core.
import { cutBytes } from "@openfactory/core";

const enc = new TextEncoder();
const dec = new TextDecoder();

/** Shortens to at most n bytes (at a character boundary) and notes how many bytes were cut. */
export function truncate(s: string, n: number): string {
  const b = enc.encode(s);
  if (b.length <= n) return s;
  const kept = cutBytes(b, n);
  return dec.decode(kept) + `… [truncated ${b.length - kept.length} bytes]`;
}

/** Keeps the head and the tail of s within max bytes. */
export function capMiddle(s: string, max: number): string {
  if (max <= 0) return s;
  const b = enc.encode(s);
  if (b.length <= max) return s;
  const marker = `\n\n… [transcript truncated: ${b.length - max} bytes omitted] …\n\n`;
  const keep = max - enc.encode(marker).length;
  if (keep < 2) return dec.decode(b.subarray(0, max));
  const head = cutBytes(b, Math.floor(keep / 2));
  const tail = keep - Math.floor(keep / 2);
  let start = b.length - tail;
  while (start < b.length && (b[start]! & 0xc0) === 0x80) start++;
  return dec.decode(head) + marker + dec.decode(b.subarray(start));
}

/** Go's %.2f. */
export const f2 = (n: number): string => n.toFixed(2);
