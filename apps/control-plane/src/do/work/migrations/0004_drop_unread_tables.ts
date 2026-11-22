// Drops work_item_refs and run_uploads: no code reads them. The references
// of a work item live in its body (WorkItem.references); the uploads of a run
// stay at their runner keys in R2.
import type { Migration } from "../../../lib/sql/migrate";

export const m0004: Migration = {
  version: 4,
  name: "drop_unread_tables",
  sql: `DROP TABLE work_item_refs;
DROP TABLE run_uploads;`,
};
