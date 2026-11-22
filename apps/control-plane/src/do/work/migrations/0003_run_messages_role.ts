// An index for the send_message limit (SENT_MESSAGES_SQL): it counts the user
// messages of one run after a time, so it searches (run_id, role, created_ms)
// instead of reading every message of the run.
import type { Migration } from "../../../lib/sql/migrate";

export const m0003: Migration = {
  version: 3,
  name: "run_messages_role",
  sql: `CREATE INDEX run_messages_role ON run_messages (run_id, role, created_ms);`,
};
