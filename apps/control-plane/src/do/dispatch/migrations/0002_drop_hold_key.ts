// Drops waiting.hold_key: no code reads or writes it (`hold` keeps the reason).
import type { Migration } from "../../../lib/sql/migrate";

export const m0002: Migration = {
  version: 2,
  name: "drop_hold_key",
  sql: `ALTER TABLE waiting DROP COLUMN hold_key;`,
};
