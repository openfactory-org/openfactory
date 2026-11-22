// WorkDO schema. Entity bodies keep the Go JSON in `data`; the
// columns that decisions read are promoted. Times are *_ms.
import type { Migration } from "../../../lib/sql/migrate";

export const m0001: Migration = {
  version: 1,
  name: "init",
  sql: `
CREATE TABLE work_item (id TEXT PRIMARY KEY, factory_id TEXT NOT NULL, stage TEXT NOT NULL, source_key TEXT NOT NULL DEFAULT '',
  creator_id TEXT NOT NULL DEFAULT '', root_run_id TEXT NOT NULL DEFAULT '', created_ms INTEGER NOT NULL,
  updated_ms INTEGER NOT NULL, completed_ms INTEGER, version INTEGER NOT NULL DEFAULT 1, data TEXT NOT NULL);
CREATE TABLE work_item_refs (key TEXT PRIMARY KEY, kind TEXT NOT NULL, added_ms INTEGER NOT NULL, data TEXT NOT NULL);
CREATE TABLE work_item_events (seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT NOT NULL UNIQUE, kind TEXT NOT NULL,
  run_id TEXT NOT NULL DEFAULT '', actor TEXT NOT NULL DEFAULT '', at_ms INTEGER NOT NULL, data TEXT NOT NULL DEFAULT '{}');
CREATE TABLE work_item_state (k TEXT PRIMARY KEY, v TEXT NOT NULL);
CREATE TABLE bot_actions (action_key TEXT PRIMARY KEY, at_ms INTEGER NOT NULL);
CREATE TABLE runs (id TEXT PRIMARY KEY, factory_id TEXT NOT NULL, work_item_id TEXT NOT NULL DEFAULT '',
  parent_run_id TEXT NOT NULL DEFAULT '', agent_name TEXT NOT NULL DEFAULT '', name TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL, worker_host TEXT NOT NULL DEFAULT 'server', triggered_by TEXT NOT NULL DEFAULT '',
  created_ms INTEGER NOT NULL, msg_seq INTEGER NOT NULL DEFAULT 0, input_seq INTEGER NOT NULL DEFAULT 0,
  version INTEGER NOT NULL DEFAULT 1, data TEXT NOT NULL);
CREATE INDEX runs_parent ON runs (parent_run_id, created_ms);
CREATE INDEX runs_state ON runs (state);
CREATE TABLE run_state (run_id TEXT NOT NULL, k TEXT NOT NULL, v TEXT NOT NULL, PRIMARY KEY (run_id, k));
CREATE TABLE run_messages (run_id TEXT NOT NULL, seq INTEGER NOT NULL, id TEXT NOT NULL UNIQUE, role TEXT NOT NULL,
  created_ms INTEGER NOT NULL, data TEXT NOT NULL, PRIMARY KEY (run_id, seq));
CREATE TABLE run_inputs (id TEXT NOT NULL UNIQUE, run_id TEXT NOT NULL, seq INTEGER NOT NULL, kind TEXT NOT NULL,
  state TEXT NOT NULL, session INTEGER NOT NULL DEFAULT 0, turn_id TEXT NOT NULL DEFAULT '', accepted INTEGER NOT NULL DEFAULT 0,
  dedupe_key TEXT NOT NULL DEFAULT '', created_ms INTEGER NOT NULL, data TEXT NOT NULL, PRIMARY KEY (run_id, seq));
CREATE UNIQUE INDEX run_inputs_dedupe ON run_inputs (run_id, dedupe_key) WHERE dedupe_key <> '';
CREATE INDEX run_inputs_state ON run_inputs (run_id, state);
CREATE INDEX run_inputs_turn ON run_inputs (run_id, turn_id) WHERE turn_id <> '';
CREATE TABLE run_sessions (run_id TEXT NOT NULL, session INTEGER NOT NULL, task_id TEXT NOT NULL UNIQUE, state TEXT NOT NULL,
  bootstrap_hash TEXT NOT NULL DEFAULT '', active_turn_id TEXT NOT NULL DEFAULT '', last_seen_ms INTEGER NOT NULL,
  stop_sent_ms INTEGER, provider TEXT NOT NULL DEFAULT '', worker_id TEXT NOT NULL DEFAULT '',
  version INTEGER NOT NULL DEFAULT 1, data TEXT NOT NULL, PRIMARY KEY (run_id, session));
CREATE UNIQUE INDEX run_sessions_bootstrap ON run_sessions (bootstrap_hash) WHERE bootstrap_hash <> '';
CREATE TABLE runner_events (run_id TEXT NOT NULL, session INTEGER NOT NULL, seq INTEGER NOT NULL, type TEXT NOT NULL,
  turn_id TEXT NOT NULL DEFAULT '', at_ms INTEGER NOT NULL, data TEXT, PRIMARY KEY (run_id, session, seq));
CREATE INDEX runner_events_diag ON runner_events (run_id, type) WHERE type = 'diagnostic';
CREATE TABLE runner_commands (run_id TEXT NOT NULL, session INTEGER NOT NULL, seq INTEGER NOT NULL, type TEXT NOT NULL,
  created_ms INTEGER NOT NULL, data TEXT, PRIMARY KEY (run_id, session, seq));
CREATE TABLE snapshot_parts (run_id TEXT NOT NULL, session INTEGER NOT NULL, part TEXT NOT NULL, upload_ids TEXT NOT NULL,
  bytes INTEGER NOT NULL DEFAULT 0, at_ms INTEGER NOT NULL, PRIMARY KEY (run_id, part, session));
CREATE TABLE run_uploads (run_id TEXT NOT NULL, session INTEGER NOT NULL, kind TEXT NOT NULL, name TEXT NOT NULL,
  upload_id TEXT NOT NULL, at_ms INTEGER NOT NULL, PRIMARY KEY (run_id, kind, name));
CREATE TABLE run_timeline (run_id TEXT NOT NULL, event_id TEXT NOT NULL, session INTEGER NOT NULL DEFAULT 0, type TEXT NOT NULL,
  at_ms INTEGER NOT NULL, data TEXT, PRIMARY KEY (run_id, event_id));
CREATE INDEX run_timeline_at ON run_timeline (run_id, at_ms);
CREATE TABLE inbox_items (id TEXT PRIMARY KEY, run_id TEXT NOT NULL DEFAULT '', kind TEXT NOT NULL, resolved INTEGER NOT NULL DEFAULT 0,
  created_ms INTEGER NOT NULL, version INTEGER NOT NULL DEFAULT 1, data TEXT NOT NULL);
CREATE INDEX inbox_items_open ON inbox_items (resolved, run_id);
CREATE TABLE dispatches (action_key TEXT NOT NULL, target TEXT NOT NULL, ref TEXT NOT NULL DEFAULT '', created_ms INTEGER NOT NULL,
  PRIMARY KEY (action_key, target));
CREATE INDEX dispatches_created ON dispatches (created_ms);
CREATE TABLE event_context (message_id TEXT PRIMARY KEY, run_id TEXT NOT NULL, r2_key TEXT NOT NULL, bytes INTEGER NOT NULL, created_ms INTEGER NOT NULL);
CREATE INDEX event_context_run ON event_context (run_id);
CREATE TABLE tool_calls (key TEXT PRIMARY KEY, run_id TEXT NOT NULL, turn_id TEXT NOT NULL, tool TEXT NOT NULL,
  result TEXT NOT NULL, created_ms INTEGER NOT NULL);
CREATE TABLE grants (run_id TEXT NOT NULL, session INTEGER NOT NULL, target TEXT NOT NULL, provider TEXT NOT NULL,
  worker_id TEXT NOT NULL DEFAULT '', lease_id TEXT NOT NULL, state TEXT NOT NULL DEFAULT 'pending', created_ms INTEGER NOT NULL,
  PRIMARY KEY (run_id, session));
`,
};
