// The projections of the IndexDO read model: one registry entry per entity
// that index.project accepts, with its upsert, its delete and its SSE topic.
import { msOf } from "../../lib/page";

export type J = Record<string, unknown>;
export const s = (v: unknown): string => (typeof v === "string" ? v : "");
export const n = (v: unknown): number => (typeof v === "number" && Number.isFinite(v) ? v : 0);
export const obj = (v: unknown): J => (typeof v === "object" && v !== null ? (v as J) : {});

export interface Projection {
  /** SSE topic of the team stream that an upsert feeds; "" for none. */
  topic: string;
  upsert(sql: SqlStorage, id: string, version: number, summary: J): void;
  remove(sql: SqlStorage, id: string): void;
}

function upsertRun(sql: SqlStorage, id: string, version: number, r: J): void {
  const cfg = obj(r["config"]);
  const usage = obj(r["usage"]);
  const creator = obj(r["creator"]);
  sql.exec(
    `INSERT INTO run_summaries (id, factory_id, work_item_id, parent_run_id, root_run_id, agent_name, name, state, creator_id,
       source, model, created_ms, finished_ms, cost_usd, version, data)
     VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
     ON CONFLICT(id) DO UPDATE SET factory_id = excluded.factory_id, work_item_id = excluded.work_item_id,
       parent_run_id = excluded.parent_run_id, root_run_id = excluded.root_run_id, agent_name = excluded.agent_name,
       name = excluded.name, state = excluded.state, creator_id = excluded.creator_id, source = excluded.source,
       model = excluded.model, finished_ms = excluded.finished_ms, cost_usd = excluded.cost_usd,
       version = excluded.version, data = excluded.data`,
    id,
    s(r["factory_id"]),
    s(r["work_item_id"]),
    s(r["parent_run_id"]),
    s(r["root_run_id"]),
    s(r["agent_name"]),
    s(r["name"]),
    s(r["state"]),
    s(creator["id"]) || s(r["creator_id"]),
    s(r["source"]),
    s(cfg["model"]) || s(r["model"]),
    msOf(r["created_at"]) || Date.now(),
    msOf(r["finished_at"]) || null,
    n(usage["cost_usd"]) || n(r["cost_usd"]),
    version,
    JSON.stringify(r),
  );
}

function upsertWorkItem(sql: SqlStorage, id: string, version: number, w: J): void {
  sql.exec(
    `INSERT INTO work_item_summaries (id, factory_id, stage, creator_id, title, created_ms, updated_ms, completed_ms, version, data)
     VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
     ON CONFLICT(id) DO UPDATE SET factory_id = excluded.factory_id, stage = excluded.stage, creator_id = excluded.creator_id,
       title = excluded.title, updated_ms = excluded.updated_ms, completed_ms = excluded.completed_ms,
       version = excluded.version, data = excluded.data`,
    id,
    s(w["factory_id"]),
    s(w["stage"]),
    s(w["creator_id"]),
    s(w["title"]),
    msOf(w["created_at"]) || Date.now(),
    msOf(w["updated_at"]) || Date.now(),
    msOf(w["completed_at"]) || null,
    version,
    JSON.stringify(w),
  );
}

/** Upserts an inbox item and replaces its recipients. owner: "work" (a WorkDO projection) or "index". */
export function upsertInbox(sql: SqlStorage, id: string, version: number, it: J, owner: string): void {
  sql.exec(
    `INSERT INTO inbox (id, owner, factory_id, work_item_id, run_id, kind, resolved, assignee_id, created_ms, resolved_ms, version, data)
     VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
     ON CONFLICT(id) DO UPDATE SET owner = excluded.owner, factory_id = excluded.factory_id, work_item_id = excluded.work_item_id,
       run_id = excluded.run_id, kind = excluded.kind, resolved = excluded.resolved, assignee_id = excluded.assignee_id,
       resolved_ms = excluded.resolved_ms, version = excluded.version, data = excluded.data`,
    id,
    owner,
    s(it["factory_id"]),
    s(it["work_item_id"]),
    s(it["run_id"]),
    s(it["kind"]),
    it["resolved"] === true ? 1 : 0,
    s(it["assignee_id"]),
    msOf(it["created_at"]) || Date.now(),
    msOf(it["resolved_at"]) || null,
    version,
    JSON.stringify(it),
  );
  sql.exec("DELETE FROM inbox_recipients WHERE inbox_id = ?", id);
  const recipients = Array.isArray(it["recipients"]) ? (it["recipients"] as unknown[]).map(s).filter(Boolean) : [];
  for (const u of new Set(recipients)) {
    sql.exec("INSERT OR IGNORE INTO inbox_recipients (inbox_id, user_id) VALUES (?, ?)", id, u);
  }
}

function upsertScore(sql: SqlStorage, id: string, version: number, sc: J): void {
  sql.exec(
    `INSERT INTO scores (id, factory_id, scorer_id, run_id, state, judge_run_id, created_ms, version, data)
     VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
     ON CONFLICT(id) DO UPDATE SET factory_id = excluded.factory_id, scorer_id = excluded.scorer_id, run_id = excluded.run_id,
       state = excluded.state, judge_run_id = excluded.judge_run_id, version = excluded.version, data = excluded.data`,
    id,
    s(sc["factory_id"]),
    s(sc["scorer_id"]),
    s(sc["run_id"]),
    s(sc["state"]) || "done",
    s(sc["judge_run_id"]),
    msOf(sc["created_at"]) || Date.now(),
    version,
    JSON.stringify(sc),
  );
}

function upsertBenchmarkRun(sql: SqlStorage, id: string, version: number, b: J): void {
  sql.exec(
    `INSERT INTO benchmark_summaries (id, factory_id, suite, state, created_ms, version, data) VALUES (?, ?, ?, ?, ?, ?, ?)
     ON CONFLICT(id) DO UPDATE SET factory_id = excluded.factory_id, suite = excluded.suite, state = excluded.state,
     version = excluded.version, data = excluded.data`,
    id,
    s(b["factory_id"]),
    s(b["suite"]),
    s(b["state"]),
    msOf(b["created_at"]) || Date.now(),
    version,
    JSON.stringify(b),
  );
}

const deleteFrom =
  (table: string) =>
  (sql: SqlStorage, id: string): void => {
    sql.exec(`DELETE FROM ${table} WHERE id = ?`, id);
  };

/** Entities accepted by index.project. */
export const PROJECTIONS: Readonly<Record<string, Projection>> = {
  run: { topic: "run", upsert: upsertRun, remove: deleteFrom("run_summaries") },
  work_item: { topic: "workitem", upsert: upsertWorkItem, remove: deleteFrom("work_item_summaries") },
  inbox: {
    topic: "inbox",
    upsert: (sql, id, version, it) => upsertInbox(sql, id, version, it, s(it["owner"]) || "work"),
    remove: (sql, id) => {
      sql.exec("DELETE FROM inbox WHERE id = ?", id);
      sql.exec("DELETE FROM inbox_recipients WHERE inbox_id = ?", id);
    },
  },
  score: { topic: "", upsert: upsertScore, remove: deleteFrom("scores") },
  benchmark_run: { topic: "", upsert: upsertBenchmarkRun, remove: deleteFrom("benchmark_summaries") },
};
