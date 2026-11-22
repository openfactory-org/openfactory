// Drops events.payload_r2 and action_targets: no code reads or writes them.
// Payloads stay inline in events.payload, and the (action, target) dispatch
// ledger is in WorkDO (dispatches).
import type { Migration } from "../../../lib/sql/migrate";

export const m0003: Migration = {
  version: 3,
  name: "drop_unused",
  sql: `ALTER TABLE events DROP COLUMN payload_r2;
DROP TABLE action_targets;`,
};
