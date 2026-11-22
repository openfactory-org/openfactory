// IndexDO schema: the read model, inbox recipients and per-user read state.
export default `
CREATE TABLE run_summaries (id TEXT PRIMARY KEY, factory_id TEXT NOT NULL DEFAULT '', work_item_id TEXT NOT NULL DEFAULT '',
  parent_run_id TEXT NOT NULL DEFAULT '', root_run_id TEXT NOT NULL DEFAULT '', agent_name TEXT NOT NULL DEFAULT '',
  name TEXT NOT NULL DEFAULT '', state TEXT NOT NULL DEFAULT '', creator_id TEXT NOT NULL DEFAULT '', source TEXT NOT NULL DEFAULT '',
  model TEXT NOT NULL DEFAULT '', created_ms INTEGER NOT NULL, finished_ms INTEGER, cost_usd REAL NOT NULL DEFAULT 0,
  version INTEGER NOT NULL, data TEXT NOT NULL);
CREATE INDEX rs_factory ON run_summaries (factory_id, created_ms DESC, id DESC);
CREATE INDEX rs_work_item ON run_summaries (work_item_id, created_ms DESC);
CREATE INDEX rs_parent ON run_summaries (parent_run_id, created_ms DESC);
CREATE INDEX rs_root ON run_summaries (root_run_id, created_ms DESC);
CREATE INDEX rs_created ON run_summaries (created_ms DESC, id DESC);
CREATE INDEX rs_state ON run_summaries (state, created_ms DESC);
CREATE INDEX rs_creator ON run_summaries (creator_id, created_ms);
CREATE TABLE work_item_summaries (id TEXT PRIMARY KEY, factory_id TEXT NOT NULL DEFAULT '', stage TEXT NOT NULL DEFAULT '',
  creator_id TEXT NOT NULL DEFAULT '', title TEXT NOT NULL DEFAULT '', created_ms INTEGER NOT NULL, updated_ms INTEGER NOT NULL,
  completed_ms INTEGER, version INTEGER NOT NULL, data TEXT NOT NULL);
CREATE INDEX wis_factory ON work_item_summaries (factory_id, created_ms DESC, id DESC);
CREATE INDEX wis_stage ON work_item_summaries (stage);
CREATE TABLE work_item_metric_events (id TEXT PRIMARY KEY, work_item_id TEXT NOT NULL, factory_id TEXT NOT NULL DEFAULT '',
  kind TEXT NOT NULL, at_ms INTEGER NOT NULL, data TEXT NOT NULL);
CREATE INDEX wime_wi ON work_item_metric_events (work_item_id, at_ms);
CREATE INDEX wime_factory ON work_item_metric_events (factory_id, at_ms);
CREATE TABLE inbox (id TEXT PRIMARY KEY, owner TEXT NOT NULL DEFAULT '', factory_id TEXT NOT NULL DEFAULT '',
  work_item_id TEXT NOT NULL DEFAULT '', run_id TEXT NOT NULL DEFAULT '', kind TEXT NOT NULL DEFAULT '',
  resolved INTEGER NOT NULL DEFAULT 0, assignee_id TEXT NOT NULL DEFAULT '', created_ms INTEGER NOT NULL, resolved_ms INTEGER,
  version INTEGER NOT NULL, data TEXT NOT NULL);
CREATE INDEX inbox_factory ON inbox (factory_id, resolved, created_ms DESC);
CREATE INDEX inbox_assignee ON inbox (assignee_id, resolved, created_ms DESC);
CREATE INDEX inbox_created ON inbox (created_ms DESC, id DESC);
CREATE TABLE inbox_recipients (inbox_id TEXT NOT NULL, user_id TEXT NOT NULL, PRIMARY KEY (inbox_id, user_id));
CREATE INDEX inbox_recipients_user ON inbox_recipients (user_id);
CREATE TABLE inbox_reads (inbox_id TEXT NOT NULL, user_id TEXT NOT NULL, read_ms INTEGER NOT NULL, PRIMARY KEY (inbox_id, user_id));
CREATE TABLE pull_request_facts (id TEXT PRIMARY KEY, pr_key TEXT NOT NULL DEFAULT '', repo TEXT NOT NULL DEFAULT '',
  head_branch TEXT NOT NULL DEFAULT '', is_open INTEGER NOT NULL DEFAULT 1, received_ms INTEGER NOT NULL DEFAULT 0, data TEXT NOT NULL);
CREATE INDEX prf_key ON pull_request_facts (pr_key);
CREATE INDEX prf_branch ON pull_request_facts (repo, head_branch);
CREATE TABLE scores (id TEXT PRIMARY KEY, factory_id TEXT NOT NULL DEFAULT '', scorer_id TEXT NOT NULL DEFAULT '',
  run_id TEXT NOT NULL DEFAULT '', state TEXT NOT NULL DEFAULT 'done', judge_run_id TEXT NOT NULL DEFAULT '',
  created_ms INTEGER NOT NULL, version INTEGER NOT NULL DEFAULT 0, data TEXT NOT NULL);
CREATE UNIQUE INDEX scores_scorer_run ON scores (scorer_id, run_id);
CREATE INDEX scores_factory ON scores (factory_id, created_ms DESC);
CREATE TABLE benchmark_summaries (id TEXT PRIMARY KEY, factory_id TEXT NOT NULL DEFAULT '', suite TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL DEFAULT '', created_ms INTEGER NOT NULL, version INTEGER NOT NULL, data TEXT NOT NULL);
CREATE INDEX bs_factory ON benchmark_summaries (factory_id, created_ms DESC);
CREATE TABLE dead_letters (id TEXT PRIMARY KEY, origin_do TEXT NOT NULL, origin_msg_id TEXT NOT NULL, kind TEXT NOT NULL,
  lane TEXT NOT NULL DEFAULT '', attempts INTEGER NOT NULL, last_error TEXT NOT NULL, created_ms INTEGER NOT NULL,
  redriven INTEGER NOT NULL DEFAULT 0, data TEXT NOT NULL);
CREATE UNIQUE INDEX dead_letters_origin ON dead_letters (origin_do, origin_msg_id);
CREATE INDEX dead_letters_created ON dead_letters (created_ms DESC, id DESC);
CREATE TABLE metrics_rollups (factory_id TEXT NOT NULL, day TEXT NOT NULL, data TEXT NOT NULL, PRIMARY KEY (factory_id, day));
CREATE TABLE entity_versions (entity TEXT NOT NULL, id TEXT NOT NULL, version INTEGER NOT NULL, PRIMARY KEY (entity, id));
CREATE TABLE stream_seq (k INTEGER PRIMARY KEY CHECK (k = 1), seq INTEGER NOT NULL);
`;
