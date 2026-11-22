// Output helpers: aligned tables (as Go text/tabwriter with padding 2),
// relative times, truncation and indented JSON.

import { CliError, type Writer } from "./framework.ts";

/** Prints v as JSON indented with two spaces, with a newline. */
export function emit(w: Writer, v: unknown): void {
  w.write(JSON.stringify(v, null, 2) + "\n");
}

const width = (s: string): number => [...s].length;

/**
 * Prints rows with aligned columns. A column is as wide as its widest cell
 * plus two spaces. The last cell of a row is not padded.
 */
export function table(w: Writer, header: string[], rows: string[][]): void {
  const all = [header, ...rows];
  const cols = header.length;
  const widths: number[] = [];
  for (let c = 0; c < cols - 1; c++) widths.push(Math.max(...all.map((r) => width(r[c] ?? ""))) + 2);
  let out = "";
  for (const r of all) {
    out +=
      r.map((cell, c) => (c < cols - 1 ? cell + " ".repeat((widths[c] ?? 0) - width(cell)) : cell)).join("") + "\n";
  }
  w.write(out);
}

const RFC3339_ZERO = /^0001-01-01/;

/** A relative time like Go's ago(): "12s ago", "5m ago", "3h ago", or the local date after two days. */
export function ago(value: string | undefined | null, now: Date = new Date()): string {
  if (!value || RFC3339_ZERO.test(value)) return "-";
  const t = new Date(value);
  if (Number.isNaN(t.getTime())) return "-";
  const secs = Math.round((now.getTime() - t.getTime()) / 1000);
  if (secs < 60) return `${secs}s ago`;
  if (secs < 3600) return `${Math.floor(secs / 60)}m ago`;
  if (secs < 48 * 3600) return `${Math.floor(secs / 3600)}h ago`;
  return localDate(t);
}

const p2 = (n: number): string => String(n).padStart(2, "0");

/** The local date as 2006-01-02. */
export function localDate(t: Date): string {
  return `${t.getFullYear()}-${p2(t.getMonth() + 1)}-${p2(t.getDate())}`;
}

/** The local time as 15:04:05. */
export function localClock(t: Date): string {
  return `${p2(t.getHours())}:${p2(t.getMinutes())}:${p2(t.getSeconds())}`;
}

/** The local time as RFC 3339 ("Z" for UTC). */
export function rfc3339Local(t: Date): string {
  const off = -t.getTimezoneOffset();
  const zone =
    off === 0 ? "Z" : `${off < 0 ? "-" : "+"}${p2(Math.floor(Math.abs(off) / 60))}:${p2(Math.abs(off) % 60)}`;
  return `${localDate(t)}T${localClock(t)}${zone}`;
}

/** Collapses white space and cuts s to n characters, ending with "…". */
export function trunc(s: string | undefined, n: number): string {
  const one = (s ?? "").split(/\s+/).filter(Boolean).join(" ");
  const chars = [...one];
  if (chars.length <= n) return one;
  return chars.slice(0, n - 1).join("") + "…";
}

/**
 * Parses key=value flags. The key is trimmed and must not be empty; the value
 * is everything after the first "=". want names the form in the error, for
 * example "NAME=VALUE". Returns undefined for no flags.
 */
export function parseKV(list: string[], flag: string, want = "key=value"): Record<string, string> | undefined {
  if (list.length === 0) return undefined;
  const out: Record<string, string> = {};
  for (const kv of list) {
    const i = kv.indexOf("=");
    const key = i < 0 ? "" : kv.slice(0, i).trim();
    if (key === "") throw new CliError(`invalid ${flag} ${JSON.stringify(kv)} (want ${want})`);
    out[key] = kv.slice(i + 1);
  }
  return out;
}
