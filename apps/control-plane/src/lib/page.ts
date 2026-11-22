// Pagination of list responses. Port of internal/api/httputil.go: limit
// (default 50, max 200), opaque cursor base64url("<unix-nanos>:<id>") of the
// last item of the previous page, sort_by=created_at, sort_order=asc|desc.
// Response: {"items": [...], "page_info": {"has_next_page", "next_cursor"}}.
import { OfError } from "@openfactory/core";

export const DEFAULT_PAGE_SIZE = 50;
export const MAX_PAGE_SIZE = 200;

export interface PageParams {
  limit: number;
  /** Decoded cursor: created time in ms and the ID of the last item. */
  cursor?: { ms: number; id: string };
  asc: boolean;
}

export interface PageInfo {
  has_next_page: boolean;
  next_cursor?: string;
}

export interface ListResponse<T> {
  items: T[];
  page_info: PageInfo;
  [extra: string]: unknown;
}

function b64urlEncode(s: string): string {
  return btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

function b64urlDecode(s: string): string {
  if (!/^[A-Za-z0-9_-]*$/.test(s)) throw new Error("bad base64url");
  const pad = s.length % 4 === 0 ? "" : "=".repeat(4 - (s.length % 4));
  return atob(s.replace(/-/g, "+").replace(/_/g, "/") + pad);
}

/** Cursor of an item created at `ms` (unix ms). Nanoseconds as in Go. */
export function encodeCursor(ms: number, id: string): string {
  return b64urlEncode(`${BigInt(Math.trunc(ms)) * 1_000_000n}:${id}`);
}

export function decodeCursor(s: string): { ms: number; id: string } {
  let raw: string;
  try {
    raw = b64urlDecode(s);
  } catch {
    throw new OfError("invalid_request", "malformed cursor");
  }
  const i = raw.indexOf(":");
  const ns = raw.slice(0, i);
  const id = raw.slice(i + 1);
  if (i < 0 || id === "" || !/^-?\d+$/.test(ns)) throw new OfError("invalid_request", "malformed cursor");
  const n = BigInt(ns);
  // Floor division toward minus infinity.
  let ms = n / 1_000_000n;
  if (n < 0n && n % 1_000_000n !== 0n) ms -= 1n;
  return { ms: Number(ms), id };
}

type Query = Record<string, string | undefined> | URLSearchParams;

function get(q: Query, k: string): string {
  return (q instanceof URLSearchParams ? q.get(k) : q[k]) ?? "";
}

/** pageFrom: reads limit, cursor, sort_by and sort_order. */
export function pageFrom(q: Query): PageParams {
  const lim = get(q, "limit");
  let limit = DEFAULT_PAGE_SIZE;
  if (lim !== "") {
    if (!/^[+-]?\d+$/.test(lim)) throw new OfError("invalid_request", `query parameter "limit" must be an integer`);
    limit = Number(lim);
  }
  if (limit <= 0) limit = DEFAULT_PAGE_SIZE;
  if (limit > MAX_PAGE_SIZE) limit = MAX_PAGE_SIZE;
  const c = get(q, "cursor");
  const by = get(q, "sort_by").toLowerCase();
  if (by !== "" && by !== "created_at") throw new OfError("invalid_request", `unknown sort_by "${by}" (expected created_at)`);
  const o = get(q, "sort_order").toLowerCase();
  if (o !== "" && o !== "desc" && o !== "asc") {
    throw new OfError("invalid_request", `unknown sort_order "${o}" (expected asc or desc)`);
  }
  const p: PageParams = { limit, asc: o === "asc" };
  if (c !== "") p.cursor = decodeCursor(c);
  return p;
}

export function listResponse<T>(items: T[], next: string, extra?: Record<string, unknown>): ListResponse<T> {
  return {
    items: items ?? [],
    page_info: next ? { has_next_page: true, next_cursor: next } : { has_next_page: false },
    ...extra,
  };
}

/** Sorts in memory and returns one page (Go paginate). */
export function paginate<T>(items: T[], key: (t: T) => { ms: number; id: string }, p: PageParams): { items: T[]; next: string } {
  const sorted = [...items].sort((a, b) => {
    const ka = key(a);
    const kb = key(b);
    if (ka.ms !== kb.ms) return (ka.ms > kb.ms) !== p.asc ? -1 : 1;
    if (ka.id === kb.id) return 0;
    return (ka.id > kb.id) !== p.asc ? -1 : 1;
  });
  let start = 0;
  if (p.cursor) {
    const c = p.cursor;
    start = sorted.length;
    for (let i = 0; i < sorted.length; i++) {
      const k = key(sorted[i]!);
      const next = p.asc ? k.ms > c.ms || (k.ms === c.ms && k.id > c.id) : k.ms < c.ms || (k.ms === c.ms && k.id < c.id);
      if (next) {
        start = i;
        break;
      }
    }
  }
  const end = start + p.limit;
  if (end >= sorted.length) return { items: sorted.slice(start), next: "" };
  const k = key(sorted[end - 1]!);
  return { items: sorted.slice(start, end), next: encodeCursor(k.ms, k.id) };
}

export interface SqlPageSpec {
  /** "SELECT ... FROM t" without WHERE/ORDER/LIMIT. */
  select: string;
  /** Extra conditions joined with AND (may be empty). */
  where: string[];
  args: unknown[];
  timeCol?: string;
  idCol?: string;
}

/** Keyset page over SQL (created_ms, id), fetched with limit + 1 rows. */
export function sqlPage<R extends Record<string, SqlStorageValue>>(
  sql: SqlStorage,
  spec: SqlPageSpec,
  p: PageParams,
): { rows: R[]; next: string } {
  const t = spec.timeCol ?? "created_ms";
  const id = spec.idCol ?? "id";
  const where = [...spec.where];
  const args = [...spec.args];
  if (p.cursor) {
    where.push(p.asc ? `(${t} > ? OR (${t} = ? AND ${id} > ?))` : `(${t} < ? OR (${t} = ? AND ${id} < ?))`);
    args.push(p.cursor.ms, p.cursor.ms, p.cursor.id);
  }
  const dir = p.asc ? "ASC" : "DESC";
  const q = `${spec.select}${where.length ? " WHERE " + where.join(" AND ") : ""} ORDER BY ${t} ${dir}, ${id} ${dir} LIMIT ?`;
  const rows = sql.exec<R>(q, ...args, p.limit + 1).toArray();
  if (rows.length <= p.limit) return { rows, next: "" };
  const last = rows[p.limit - 1]!;
  return { rows: rows.slice(0, p.limit), next: encodeCursor(Number(last[t]), String(last[id])) };
}

/** RFC 3339 (Go time.Time JSON) to unix ms; 0 for empty or invalid. */
export function msOf(t: unknown): number {
  if (typeof t !== "string" || t === "") return 0;
  const ms = Date.parse(t);
  return Number.isFinite(ms) ? ms : 0;
}

/** Unix ms to RFC 3339 in UTC, like Go's time.Time JSON (no trailing zero fraction). */
export function isoOf(ms: number): string {
  const s = new Date(ms).toISOString();
  return s.endsWith(".000Z") ? s.slice(0, -5) + "Z" : s.replace(/0+Z$/, "Z");
}
