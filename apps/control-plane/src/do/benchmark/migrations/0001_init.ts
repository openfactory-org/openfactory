// BenchmarkDO schema, with the columns the DO queries by.
export default `
CREATE TABLE bench (k INTEGER PRIMARY KEY CHECK (k = 1), id TEXT NOT NULL, factory_id TEXT NOT NULL, suite TEXT NOT NULL,
  state TEXT NOT NULL, version INTEGER NOT NULL DEFAULT 1, created_ms INTEGER NOT NULL, finished_ms INTEGER, data TEXT NOT NULL);
CREATE TABLE trials (id TEXT PRIMARY KEY, ord INTEGER NOT NULL, state TEXT NOT NULL, run_id TEXT NOT NULL DEFAULT '',
  work_item_id TEXT NOT NULL DEFAULT '', data TEXT NOT NULL);
CREATE INDEX trials_run ON trials (run_id) WHERE run_id <> '';
CREATE INDEX trials_state ON trials (state, ord);
CREATE TABLE cursor (k INTEGER PRIMARY KEY CHECK (k = 1), next_trial INTEGER NOT NULL);
CREATE TABLE cancel_queue (run_id TEXT PRIMARY KEY, actor TEXT NOT NULL);
`;
