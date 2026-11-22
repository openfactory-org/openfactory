// Base tables of every BaseDO (scope "lib"): meta, the outbox, the inbox dedupe
// and the timers.
import type { Migration } from "../sql/migrate";

export const LIB_MIGRATIONS: readonly Migration[] = [
  {
    version: 1,
    name: "base",
    sql: `
CREATE TABLE meta (k TEXT PRIMARY KEY, v TEXT NOT NULL);
CREATE TABLE outbox (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  msg_id TEXT NOT NULL UNIQUE,
  dest TEXT NOT NULL,
  lane TEXT NOT NULL DEFAULT '',
  kind TEXT NOT NULL,
  payload TEXT NOT NULL,
  state TEXT NOT NULL DEFAULT 'pending',
  attempts INTEGER NOT NULL DEFAULT 0,
  next_ms INTEGER NOT NULL,
  last_error TEXT NOT NULL DEFAULT '',
  created_ms INTEGER NOT NULL
);
CREATE INDEX outbox_due ON outbox (state, next_ms);
CREATE INDEX outbox_lane ON outbox (lane, id) WHERE lane <> '';
CREATE TABLE inbox_dedupe (msg_id TEXT PRIMARY KEY, received_ms INTEGER NOT NULL);
CREATE INDEX inbox_dedupe_received ON inbox_dedupe (received_ms);
CREATE TABLE timers (
  name TEXT PRIMARY KEY,
  kind TEXT NOT NULL,
  due_ms INTEGER NOT NULL,
  payload TEXT NOT NULL DEFAULT 'null',
  attempts INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX timers_due ON timers (due_ms);
`,
  },
  {
    version: 2,
    name: "timer_seq",
    // Row version of a timer: take() and fail() match on it, not on due_ms.
    sql: `ALTER TABLE timers ADD COLUMN seq INTEGER NOT NULL DEFAULT 0`,
  },
];
