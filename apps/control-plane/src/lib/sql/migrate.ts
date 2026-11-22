// DO SQLite migrations. Each DO class keeps an ordered list of migrations
// (src/do/<name>/migrations/NNNN_<name>.ts exporting the SQL as a string).
// applyMigrations runs in the DO constructor under blockConcurrencyWhile.
//
// Applied migrations are recorded in schema_migrations with the sha256 of
// their SQL, like storetest/upgrade.go records released files. A changed
// released migration is reported, not re-applied.
import { sha256Hex } from "../bytes";

export interface Migration {
  /** NNNN from the file name. Unique within a scope. */
  readonly version: number;
  /** <name> from the file name, for diagnostics. */
  readonly name: string;
  /** One or more statements joined with ";". No bindings. */
  readonly sql: string;
}

/** Applied migration row. */
export interface AppliedMigration {
  scope: string;
  version: number;
  name: string;
  sha256: string;
  applied_ms: number;
}

/** Checks a migration list: positive, strictly increasing versions. Throws on a bad list. */
export function checkMigrations(list: readonly Migration[]): void {
  let last = 0;
  for (const m of list) {
    if (!Number.isInteger(m.version) || m.version <= 0)
      throw new Error(`migration ${m.name}: bad version ${m.version}`);
    if (m.version <= last) throw new Error(`migration ${m.version}_${m.name}: versions must increase (after ${last})`);
    last = m.version;
  }
}

/**
 * Applies the migrations of `scope` that are not applied yet, each in its own
 * transactionSync. Returns the versions applied by this call.
 * Scopes keep independent version sequences ("lib" for the base tables of
 * src/lib, "do" for the DO's own tables).
 */
export async function applyMigrations(
  storage: DurableObjectStorage,
  scope: string,
  list: readonly Migration[],
): Promise<number[]> {
  checkMigrations(list);
  const sql = storage.sql;
  sql.exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
    scope TEXT NOT NULL, version INTEGER NOT NULL, name TEXT NOT NULL, sha256 TEXT NOT NULL,
    applied_ms INTEGER NOT NULL, PRIMARY KEY (scope, version))`);
  const applied = new Map<number, string>();
  for (const r of sql.exec<{ version: number; sha256: string }>(
    "SELECT version, sha256 FROM schema_migrations WHERE scope = ?",
    scope,
  )) {
    applied.set(r.version, r.sha256);
  }
  const hashes = await Promise.all(list.map((m) => sha256Hex(m.sql)));
  const done: number[] = [];
  list.forEach((m, i) => {
    const hash = hashes[i]!;
    const prev = applied.get(m.version);
    if (prev !== undefined) {
      if (prev !== hash) console.warn(`migration ${scope}/${m.version}_${m.name} changed after it was applied`);
      return;
    }
    storage.transactionSync(() => {
      sql.exec(m.sql);
      sql.exec(
        "INSERT INTO schema_migrations (scope, version, name, sha256, applied_ms) VALUES (?, ?, ?, ?, ?)",
        scope,
        m.version,
        m.name,
        hash,
        Date.now(),
      );
    });
    done.push(m.version);
  });
  return done;
}

/** The schema as text (sorted CREATE statements), for snapshot tests. */
export function schemaSnapshot(sql: SqlStorage): string {
  return sql
    .exec<{ sql: string }>(
      "SELECT sql FROM sqlite_master WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%' AND name NOT LIKE '_cf_%' ORDER BY type, name",
    )
    .toArray()
    .map((r) => r.sql)
    .join(";\n");
}
