// Chunked multi-row inserts under the DO SQLite limits: at most 100 bound
// parameters per statement and at most 100 KB of statement text.

/** DO SQLite limits: bound parameters per statement and statement size. */
export const MAX_BOUND_PARAMS = 100;
export const MAX_STATEMENT_BYTES = 100_000;

export interface InsertManyOptions {
  /** "INSERT", "INSERT OR REPLACE" or "INSERT OR IGNORE". Default "INSERT". */
  verb?: "INSERT" | "INSERT OR REPLACE" | "INSERT OR IGNORE";
  /** Text after VALUES, for example "ON CONFLICT(id) DO UPDATE SET data = excluded.data". */
  suffix?: string;
}

const IDENT_RE = /^[A-Za-z_][A-Za-z0-9_]*$/;

/**
 * Inserts rows in as few statements as the limits allow. Values are bound,
 * so only the placeholders count toward the statement size. Returns the
 * number of statements run. Call it inside transactionSync for atomicity.
 */
export function insertMany(
  sql: SqlStorage,
  table: string,
  cols: readonly string[],
  rows: readonly (readonly unknown[])[],
  opts: InsertManyOptions = {},
): number {
  if (!IDENT_RE.test(table) || cols.length === 0 || !cols.every((c) => IDENT_RE.test(c))) {
    throw new Error(`insertMany: bad table or column names for ${table}`);
  }
  if (cols.length > MAX_BOUND_PARAMS) throw new Error(`insertMany: ${cols.length} columns exceed ${MAX_BOUND_PARAMS}`);
  const head = `${opts.verb ?? "INSERT"} INTO ${table} (${cols.join(", ")}) VALUES `;
  const tail = opts.suffix ? " " + opts.suffix : "";
  const one = `(${cols.map(() => "?").join(", ")})`;
  const byParams = Math.floor(MAX_BOUND_PARAMS / cols.length);
  const bySize = Math.floor((MAX_STATEMENT_BYTES - head.length - tail.length) / (one.length + 2));
  const per = Math.max(1, Math.min(byParams, bySize));
  let statements = 0;
  for (let i = 0; i < rows.length; i += per) {
    const part = rows.slice(i, i + per);
    for (const r of part) {
      if (r.length !== cols.length) throw new Error(`insertMany: row has ${r.length} values, want ${cols.length}`);
    }
    sql.exec(head + part.map(() => one).join(", ") + tail, ...part.flat());
    statements++;
  }
  return statements;
}

/** Splits a list for `WHERE x IN (...)` queries under the parameter limit. */
export function chunks<T>(list: readonly T[], size = MAX_BOUND_PARAMS - 4): T[][] {
  const out: T[][] = [];
  for (let i = 0; i < list.length; i += size) out.push(list.slice(i, i + size));
  return out;
}
