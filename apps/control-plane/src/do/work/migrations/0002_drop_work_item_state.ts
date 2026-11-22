// Drops work_item_state: no code reads or writes it. Per-run values are in
// run_state and DO-wide values in meta.
import type { Migration } from "../../../lib/sql/migrate";

export const m0002: Migration = {
  version: 2,
  name: "drop_work_item_state",
  sql: `DROP TABLE work_item_state;`,
};
