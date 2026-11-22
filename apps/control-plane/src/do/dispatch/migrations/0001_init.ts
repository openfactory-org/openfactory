// DispatchDO schema: admission only.
import type { Migration } from "../../../lib/sql/migrate";

export const m0001: Migration = {
  version: 1,
  name: "init",
  sql: `
CREATE TABLE waiting (run_id TEXT NOT NULL, session INTEGER NOT NULL, factory_id TEXT NOT NULL, target TEXT NOT NULL,
  target_key TEXT NOT NULL, work_do TEXT NOT NULL, restricted INTEGER NOT NULL DEFAULT 0, run_created_ms INTEGER NOT NULL,
  enqueued_ms INTEGER NOT NULL, seq INTEGER NOT NULL, hold TEXT NOT NULL DEFAULT '', hold_key TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (run_id, session));
CREATE INDEX waiting_order ON waiting (enqueued_ms, seq);
CREATE TABLE leases (run_id TEXT NOT NULL, session INTEGER NOT NULL, task_id TEXT NOT NULL UNIQUE, factory_id TEXT NOT NULL,
  target_key TEXT NOT NULL, provider TEXT NOT NULL, worker_id TEXT NOT NULL DEFAULT '', work_do TEXT NOT NULL,
  granted_ms INTEGER NOT NULL, started_ms INTEGER, PRIMARY KEY (run_id, session));
CREATE INDEX leases_factory ON leases (factory_id);
CREATE INDEX leases_provider ON leases (provider);
CREATE TABLE factory_limits (factory_id TEXT PRIMARY KEY, max_concurrent INTEGER NOT NULL DEFAULT 0,
  daily_usd REAL NOT NULL DEFAULT 0, monthly_usd REAL NOT NULL DEFAULT 0, version_id TEXT NOT NULL);
CREATE TABLE provider_buckets (provider TEXT NOT NULL, window_s INTEGER NOT NULL, tokens REAL NOT NULL, cap REAL NOT NULL,
  updated_ms INTEGER NOT NULL, PRIMARY KEY (provider, window_s));
CREATE TABLE workers (worker_id TEXT PRIMARY KEY, kind TEXT NOT NULL DEFAULT '', online INTEGER NOT NULL, draining INTEGER NOT NULL DEFAULT 0,
  slots INTEGER NOT NULL, used INTEGER NOT NULL, labels TEXT NOT NULL DEFAULT '{}', factories TEXT NOT NULL DEFAULT '[]',
  generation INTEGER NOT NULL, seen_ms INTEGER NOT NULL);
CREATE TABLE spend (run_id TEXT NOT NULL, session INTEGER NOT NULL, factory_id TEXT NOT NULL, day TEXT NOT NULL, month TEXT NOT NULL,
  cost_usd REAL NOT NULL, seq INTEGER NOT NULL, root_run_id TEXT NOT NULL, work_do TEXT NOT NULL, PRIMARY KEY (run_id, session));
CREATE TABLE spend_totals (scope TEXT NOT NULL, id TEXT NOT NULL, period TEXT NOT NULL, start TEXT NOT NULL, cost_usd REAL NOT NULL,
  PRIMARY KEY (scope, id, period, start));
CREATE TABLE spend_warned (key TEXT PRIMARY KEY, at_ms INTEGER NOT NULL);
`,
};
