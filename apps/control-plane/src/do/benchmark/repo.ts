// The tables of BenchmarkDO (bench, trials, cursor, cancel_queue) and the
// projections of a run: the API view and the summary row for IndexDO.
// Writes run inside the caller's transaction.
import type { RepoSubstitution } from "@openfactory/core";
import { all, first } from "../../lib/sql/query";
import { CORRECTNESS_SCORER, aggregate, type BenchmarkResults, type Trial } from "../../scoring/benchmark";

/** The part of BenchmarkResults that is fixed at start. */
export type BenchmarkPlan = Pick<
  BenchmarkResults,
  "suite" | "configurations" | "repetitions" | "correctness_model" | "correctness_judge" | "scorers"
> & { agents: string[]; cancelled_by?: string };

export interface StartBenchmarkInput {
  /** `bench_<token>...`, chosen by the caller (routing). */
  id: string;
  factoryId: string;
  plan: BenchmarkPlan;
  repoSubstitutions?: RepoSubstitution[];
}

/** The BenchmarkRun of the API (core.BenchmarkRun). */
export interface BenchmarkRunJson {
  id: string;
  factory_id: string;
  suite: string;
  state: string;
  results: BenchmarkResults;
  created_at: string;
  finished_at?: string;
}

export interface BenchRow {
  id: string;
  factory_id: string;
  suite: string;
  state: string;
  version: number;
  created_ms: number;
  finished_ms: number | null;
  data: string;
}

export const iso = (ms: number): string => new Date(ms).toISOString();

export class BenchmarkRepo {
  constructor(private readonly sql: SqlStorage) {}

  // ---------- bench ----------

  row(): BenchRow | undefined {
    return first<BenchRow & Record<string, never>>(
      this.sql,
      "SELECT id, factory_id, suite, state, version, created_ms, finished_ms, data FROM bench WHERE k = 1",
    ) as BenchRow | undefined;
  }

  insertRun(
    input: Pick<StartBenchmarkInput, "id" | "factoryId" | "plan">,
    state: string,
    createdMs: number,
    trials: readonly Trial[],
  ): BenchRow {
    this.sql.exec(
      "INSERT INTO bench (k, id, factory_id, suite, state, version, created_ms, data) VALUES (1, ?, ?, ?, ?, 1, ?, ?)",
      input.id,
      input.factoryId,
      input.plan.suite.slug,
      state,
      createdMs,
      JSON.stringify(input.plan),
    );
    trials.forEach((t, i) =>
      this.sql.exec(
        "INSERT INTO trials (id, ord, state, data) VALUES (?, ?, ?, ?)",
        t.id,
        i,
        t.status,
        JSON.stringify(t),
      ),
    );
    this.sql.exec("INSERT INTO cursor (k, next_trial) VALUES (1, 0)");
    return this.row()!;
  }

  /** Writes the state of the run; `plan` replaces the stored plan when given. */
  updateRun(state: string, version: number, finishedMs: number | null, plan?: BenchmarkPlan): void {
    if (plan) {
      this.sql.exec(
        "UPDATE bench SET state = ?, version = ?, finished_ms = ?, data = ? WHERE k = 1",
        state,
        version,
        finishedMs,
        JSON.stringify(plan),
      );
    } else {
      this.sql.exec("UPDATE bench SET state = ?, version = ?, finished_ms = ? WHERE k = 1", state, version, finishedMs);
    }
  }

  // ---------- trials ----------

  trials(): Trial[] {
    return all<{ data: string }>(this.sql, "SELECT data FROM trials ORDER BY ord").map(
      (r) => JSON.parse(r.data) as Trial,
    );
  }

  trial(id: string): Trial | undefined {
    const r = first<{ data: string }>(this.sql, "SELECT data FROM trials WHERE id = ?", id);
    return r ? (JSON.parse(r.data) as Trial) : undefined;
  }

  pendingTrials(limit: number): Trial[] {
    return all<{ data: string }>(
      this.sql,
      "SELECT data FROM trials WHERE state = 'pending' ORDER BY ord LIMIT ?",
      limit,
    ).map((r) => JSON.parse(r.data) as Trial);
  }

  hasPendingTrial(): boolean {
    return first(this.sql, "SELECT 1 FROM trials WHERE state = 'pending' LIMIT 1") !== undefined;
  }

  /** Reports whether any trial has a run. */
  hasStartedTrial(): boolean {
    return first(this.sql, "SELECT 1 FROM trials WHERE run_id <> '' LIMIT 1") !== undefined;
  }

  writeTrial(t: Trial): void {
    this.sql.exec(
      "UPDATE trials SET state = ?, run_id = ?, work_item_id = ?, data = ? WHERE id = ?",
      t.status,
      t.run_id ?? "",
      t.work_item_id ?? "",
      JSON.stringify(t),
      t.id,
    );
  }

  // ---------- cancel queue ----------

  queueCancel(runId: string, actor: string): void {
    this.sql.exec("INSERT OR IGNORE INTO cancel_queue (run_id, actor) VALUES (?, ?)", runId, actor);
  }

  cancelBatch(limit: number): { run_id: string; actor: string }[] {
    return all<{ run_id: string; actor: string }>(this.sql, "SELECT run_id, actor FROM cancel_queue LIMIT ?", limit);
  }

  dequeueCancel(runId: string): void {
    this.sql.exec("DELETE FROM cancel_queue WHERE run_id = ?", runId);
  }

  hasQueuedCancel(): boolean {
    return first(this.sql, "SELECT 1 FROM cancel_queue LIMIT 1") !== undefined;
  }
}

export const parsePlan = (row: BenchRow): BenchmarkPlan => JSON.parse(row.data) as BenchmarkPlan;

/** The API view of a run: the plan, the trials and the aggregates. */
export function benchmarkView(row: BenchRow, plan: BenchmarkPlan, trials: Trial[]): BenchmarkRunJson {
  const scorerKeys = [CORRECTNESS_SCORER, ...(plan.scorers ?? []).map((s) => s.key)];
  const agg = aggregate(plan.configurations, trials, scorerKeys);
  const results: BenchmarkResults = {
    suite: plan.suite,
    configurations: plan.configurations,
    repetitions: plan.repetitions,
    correctness_model: plan.correctness_model,
    correctness_judge: plan.correctness_judge,
    ...(plan.scorers && plan.scorers.length > 0 ? { scorers: plan.scorers } : {}),
    trials,
    summary: agg.summary,
    recommendations: agg.recommendations,
    total_cost_usd: agg.total,
    judge_cost_usd: agg.summary.reduce((n, r) => n + r.judge_cost_usd, 0),
    ...(plan.cancelled_by ? { cancelled_by: plan.cancelled_by } : {}),
  };
  const out: BenchmarkRunJson = {
    id: row.id,
    factory_id: row.factory_id,
    suite: row.suite,
    state: row.state,
    results,
    created_at: iso(row.created_ms),
  };
  if (row.finished_ms) out.finished_at = iso(row.finished_ms);
  return out;
}

/** The row for IndexDO: no trials (they are large); the summary and the totals. */
export function indexSummary(v: BenchmarkRunJson): Record<string, unknown> {
  const r = v.results;
  return {
    id: v.id,
    factory_id: v.factory_id,
    suite: v.suite,
    state: v.state,
    created_at: v.created_at,
    ...(v.finished_at ? { finished_at: v.finished_at } : {}),
    results: {
      suite: { slug: r.suite.slug, name: r.suite.name, agent: r.suite.agent },
      configurations: r.configurations,
      repetitions: r.repetitions,
      summary: r.summary,
      recommendations: r.recommendations,
      total_cost_usd: r.total_cost_usd,
      judge_cost_usd: r.judge_cost_usd,
      trials_total: r.trials.length,
      ...(r.cancelled_by ? { cancelled_by: r.cancelled_by } : {}),
    },
  };
}
