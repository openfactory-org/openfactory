// Small typed helpers over SqlStorage. They read synchronously; call them
// inside transactionSync when the read decides a write.

export type SqlValue = ArrayBuffer | string | number | null;
export type Row = Record<string, SqlStorageValue>;

/** All rows. */
export function all<T extends Row = Row>(sql: SqlStorage, query: string, ...args: unknown[]): T[] {
  return sql.exec<T>(query, ...args).toArray();
}

/** The first row, or undefined. */
export function first<T extends Row = Row>(sql: SqlStorage, query: string, ...args: unknown[]): T | undefined {
  const cur = sql.exec<T>(query, ...args);
  const r = cur.next();
  return r.done ? undefined : r.value;
}

/** Runs a write and returns the number of rows written. */
export function run(sql: SqlStorage, query: string, ...args: unknown[]): number {
  return sql.exec(query, ...args).rowsWritten;
}

/** Parses the JSON `data` column of a row. */
export function parseData<T>(row: { data: SqlStorageValue } | undefined): T | undefined {
  if (!row) return undefined;
  return JSON.parse(String(row.data)) as T;
}

/** Bool to SQLite integer. */
export const b = (v: boolean | undefined): number => (v ? 1 : 0);
