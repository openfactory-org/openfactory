// BenchmarkDO (key team:<t>|bench:<token>): one benchmark run. It owns the
// run, its trials and the chunk cursor; the summary goes to IndexDO
// (`index.project`, entity benchmark_run) for the lists.
//
// Port of the benchmark part of internal/scoring (benchmark.go) with the plan's
// changes:
// - Trials are dispatched in chunks driven by one alarm timer (rule 10), each
//   dispatch idempotent on the trial ID, so a reset DO repeats no trial.
// - The in-process lockKey is a per-DO promise chain. Network calls (engine,
//   runs) run outside any transaction; state changes are one `tx` each (rule 1, 2).
// - A cancel marks the trials at once and cancels the runs in chunks.
// - A reconcile timer re-checks unfinished trials every 15 minutes (the Go sweep).
import {
  OfError,
  newRoutedId,
  toOfError,
  type BenchmarkConfiguration,
  type BenchmarkSuite,
  type RepoSubstitution,
  type Run,
} from "@openfactory/core";
import type { CallMeta, BenchmarkRpc } from "../../contracts/do-messages";
import type { Env } from "../../contracts/env";
import { requireRole } from "../../auth/roles";
import { BaseDO } from "../../lib/base";
import { dest } from "../../lib/outbox/dest";
import type { Migration } from "../../lib/sql/migrate";
import { all, first } from "../../lib/sql/query";
import {
  BENCHMARK_CANCELLED,
  BENCHMARK_FAILED,
  BENCHMARK_RUNNING,
  aggregate,
  applyRepoSubstitutions,
  benchmarkState,
  benchmarkTerminal,
  trialDispatch,
  trialDone,
  trialRunFinished,
  trialScore,
  trialScorers,
  CORRECTNESS_SCORER,
  type BenchmarkResults,
  type BenchmarkScorer,
  type Trial,
  type TrialScore,
} from "../../scoring/benchmark";
import { workEngine, workRunsPort } from "../../scoring/clients";
import { judgeAnswer, judgeRequest, treeUsage } from "../../scoring/judge";
import { deterministicRunId } from "../../scoring/service";
import { resolveLabel } from "../../scoring/sample";
import { clip } from "../../scoring/text";
import {
  JUDGE_TRIAL,
  MAX_METADATA_VALUE,
  META_JUDGE,
  META_JUDGE_BENCHMARK,
  META_JUDGE_KEY,
  META_JUDGE_SCORED_RUN,
  META_JUDGE_TRIAL,
  finishedForScoring,
  isTerminalState,
  waitsForInput,
  type EnginePort,
  type JudgeExec,
  type RunsPort,
} from "../../scoring/types";
import m0001 from "./migrations/0001_init";

const MIGRATIONS: readonly Migration[] = [{ version: 1, name: "init", sql: m0001 }];

/** Trials dispatched per alarm step. */
export const DISPATCH_CHUNK = 5;
/** Run cancels per alarm step. */
export const CANCEL_CHUNK = 10;
export const RECONCILE_EVERY_MS = 15 * 60_000;

const T_DISPATCH = "bench.dispatch";
const T_CANCEL = "bench.cancel";
const T_RECONCILE = "bench.reconcile";

export interface BenchmarkDeps {
  runs: RunsPort;
  engine: EnginePort;
}

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

interface BenchRow {
  id: string;
  factory_id: string;
  suite: string;
  state: string;
  version: number;
  created_ms: number;
  finished_ms: number | null;
  data: string;
}

const iso = (ms: number): string => new Date(ms).toISOString();

export class BenchmarkDO extends BaseDO implements BenchmarkRpc {
  /** Overridden by tests. The default talks to WorkDO and TeamDO. */
  deps: BenchmarkDeps | undefined;
  /** Overridden by tests. */
  clock: () => number = Date.now;
  private chain: Promise<unknown> = Promise.resolve();

  constructor(ctx: DurableObjectState, env: Env) {
    super(ctx, env);
    // Dispatch and cancel steps run outside the lock: a cancel must not wait for a slow dispatch.
    this.onTimerAsync(T_DISPATCH, () => this.dispatchStep());
    this.onTimerAsync(T_CANCEL, () => this.cancelStep());
    this.onTimerAsync(T_RECONCILE, () => this.serial(() => this.reconcile()));
  }

  protected migrations(): readonly Migration[] {
    return MIGRATIONS;
  }

  async ping(): Promise<string> {
    return "BenchmarkDO";
  }

  private port(): BenchmarkDeps {
    if (!this.deps) {
      const team = this.teamId();
      this.deps = { runs: workRunsPort(this.env, team), engine: workEngine(this.env, team) };
    }
    return this.deps;
  }

  /** Runs fn after every earlier call: the per-DO lock. */
  private serial<T>(fn: () => Promise<T>): Promise<T> {
    const next = this.chain.then(fn, fn);
    this.chain = next.catch(() => undefined);
    return next;
  }

  private token(): string {
    const name = this.doName();
    const m = /\|bench:([wib][a-z2-7]{15})$/.exec(name);
    if (!m) throw new OfError("internal_error", `BenchmarkDO name ${JSON.stringify(name)} has no token`);
    return m[1]!;
  }

  // ---------- storage ----------

  private benchRow(): BenchRow | undefined {
    return first<BenchRow & Record<string, never>>(
      this.sql,
      "SELECT id, factory_id, suite, state, version, created_ms, finished_ms, data FROM bench WHERE k = 1",
    ) as BenchRow | undefined;
  }

  private mustBench(): { row: BenchRow; plan: BenchmarkPlan } {
    const row = this.benchRow();
    if (!row) throw new OfError("resource_not_found", "benchmark run not found");
    return { row, plan: JSON.parse(row.data) as BenchmarkPlan };
  }

  private loadTrials(): Trial[] {
    return all<{ data: string }>(this.sql, "SELECT data FROM trials ORDER BY ord").map(
      (r) => JSON.parse(r.data) as Trial,
    );
  }

  private view(row: BenchRow, plan: BenchmarkPlan, trials: Trial[]): BenchmarkRunJson {
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
  private summary(v: BenchmarkRunJson): Record<string, unknown> {
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

  /**
   * Writes the changed trials and the state of the run in one transaction and
   * projects the run to IndexDO. `state` forces a state; else it follows the trials.
   */
  private persist(
    changed: readonly Trial[],
    opts: { state?: string; plan?: BenchmarkPlan; extra?: () => void } = {},
  ): BenchmarkRunJson {
    return this.tx(() => {
      const { row, plan: oldPlan } = this.mustBench();
      const plan = opts.plan ?? oldPlan;
      for (const t of changed) {
        this.sql.exec(
          "UPDATE trials SET state = ?, run_id = ?, work_item_id = ?, data = ? WHERE id = ?",
          t.status,
          t.run_id ?? "",
          t.work_item_id ?? "",
          JSON.stringify(t),
          t.id,
        );
      }
      opts.extra?.();
      const trials = this.loadTrials();
      let state = row.state;
      if (opts.state) state = opts.state;
      else if (!benchmarkTerminal(row.state)) state = benchmarkState(trials);
      let finished = row.finished_ms;
      if (benchmarkTerminal(state) && finished === null) finished = this.clock();
      const version = row.version + 1;
      this.sql.exec(
        "UPDATE bench SET state = ?, version = ?, finished_ms = ?, data = ? WHERE k = 1",
        state,
        version,
        finished,
        JSON.stringify(plan),
      );
      const next: BenchRow = { ...row, state, version, finished_ms: finished, data: JSON.stringify(plan) };
      const v = this.view(next, plan, trials);
      this.project(next, v);
      return v;
    });
  }

  private project(row: BenchRow, v: BenchmarkRunJson): void {
    this.enqueue({
      dest: dest.index(this.teamId()),
      kind: "index.project",
      msgId: `bench:${row.id}:${row.version}`,
      payload: { entity: "benchmark_run", id: row.id, version: row.version, summary: this.summary(v) },
    });
  }

  // ---------- RPC ----------

  /** POST /factories/{uid}/benchmarks/{suite}/runs: creates the run and its trials and starts dispatching. */
  async start(meta: CallMeta, input: unknown): Promise<BenchmarkRunJson> {
    return this.rpc(() =>
      this.serial(async () => {
        const inp = input as StartBenchmarkInput;
        requireRole(meta.principal, inp.factoryId, "editor");
        const existing = this.benchRow();
        if (existing) return this.view(existing, JSON.parse(existing.data) as BenchmarkPlan, this.loadTrials());
        const plan = inp.plan;
        if (!plan?.suite?.tasks || plan.suite.tasks.length === 0)
          throw new OfError("invalid_request", `benchmark suite ${JSON.stringify(plan?.suite?.slug)} has no tasks`);
        const token = this.token();
        const now = this.clock();
        const trials: Trial[] = [];
        for (const cfg of plan.configurations) {
          for (const task of plan.suite.tasks) {
            for (let rep = 1; rep <= plan.repetitions; rep++) {
              trials.push({
                id: newRoutedId("trial", token),
                configuration: cfg.name,
                task: task.slug,
                repetition: rep,
                status: "pending",
                cost_usd: 0,
                input_tokens: 0,
                output_tokens: 0,
                duration_ms: 0,
                scored: false,
                ...(task.startingRepoRefs && task.startingRepoRefs.length > 0
                  ? { starting_repo_refs: applyRepoSubstitutions(task.startingRepoRefs, inp.repoSubstitutions) }
                  : {}),
              });
            }
          }
        }
        return this.tx(() => {
          this.sql.exec(
            "INSERT INTO bench (k, id, factory_id, suite, state, version, created_ms, data) VALUES (1, ?, ?, ?, ?, 1, ?, ?)",
            inp.id,
            inp.factoryId,
            plan.suite.slug,
            BENCHMARK_RUNNING,
            now,
            JSON.stringify(plan),
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
          this.setTimer(T_DISPATCH, T_DISPATCH, now);
          this.setTimer(T_RECONCILE, T_RECONCILE, now + RECONCILE_EVERY_MS);
          const row = this.benchRow()!;
          const v = this.view(row, plan, trials);
          this.project(row, v);
          return v;
        });
      }),
    );
  }

  /** GET /benchmark-runs/{id}. */
  async get(meta: CallMeta): Promise<BenchmarkRunJson> {
    return this.rpc(() => {
      const { row, plan } = this.mustBench();
      requireRole(meta.principal, row.factory_id, "user");
      return this.view(row, plan, this.loadTrials());
    });
  }

  /**
   * POST /benchmark-runs/{id}/cancel. Pending trials are cancelled at once.
   * The runs of the other trials are cancelled in chunks by an alarm step.
   */
  async cancel(meta: CallMeta): Promise<BenchmarkRunJson> {
    return this.rpc(() =>
      this.serial(async () => {
        const { row, plan } = this.mustBench();
        requireRole(meta.principal, row.factory_id, "editor");
        if (row.state === BENCHMARK_CANCELLED) return this.view(row, plan, this.loadTrials());
        if (benchmarkTerminal(row.state)) throw new OfError("conflict", `benchmark run ${row.id} is ${row.state}`);
        const actor = meta.principal?.userId ? "user:" + meta.principal.userId : (meta.principal?.id ?? "api");
        const trials = this.loadTrials();
        const queue: string[] = [];
        const changed: Trial[] = [];
        for (const t of trials) {
          let ch = false;
          if (t.status === "pending") {
            t.status = "cancelled";
            ch = true;
          } else if (t.status === "running") {
            if (t.run_id) queue.push(t.run_id);
            t.status = "cancelled";
            t.run_state = "cancelled";
            ch = true;
          }
          for (const ts of t.scores ?? []) {
            if (!ts.pending) continue;
            if (ts.judge_run_id) queue.push(ts.judge_run_id);
            ts.pending = false;
            ts.error = "cancelled";
            ch = true;
          }
          if (ch) changed.push(t);
        }
        return this.persist(changed, {
          state: BENCHMARK_CANCELLED,
          plan: { ...plan, cancelled_by: actor },
          extra: () => {
            for (const id of queue)
              this.sql.exec("INSERT OR IGNORE INTO cancel_queue (run_id, actor) VALUES (?, ?)", id, actor);
            if (queue.length > 0) this.setTimer(T_CANCEL, T_CANCEL, this.clock());
            this.clearTimer(T_DISPATCH);
            this.clearTimer(T_RECONCILE);
          },
        });
      }),
    );
  }

  /** A root run of a trial finished or waits for input (queue job bench.trialUpdate / scoring.runFinished). */
  async trialRunUpdate(runId: string): Promise<void> {
    return this.rpc(() => this.serial(() => this.onTrialRun(runId)));
  }

  /** A judge run of a trial finished or waits for input. */
  async trialJudgeUpdate(judgeRunId: string): Promise<void> {
    return this.rpc(() => this.serial(() => this.onTrialJudge(judgeRunId)));
  }

  // ---------- dispatch in chunks ----------

  private async dispatchStep(): Promise<void> {
    const row = this.benchRow();
    if (!row || benchmarkTerminal(row.state)) return;
    const plan = JSON.parse(row.data) as BenchmarkPlan;
    const pending = all<{ data: string }>(
      this.sql,
      "SELECT data FROM trials WHERE state = 'pending' ORDER BY ord LIMIT ?",
      DISPATCH_CHUNK,
    ).map((r) => JSON.parse(r.data) as Trial);
    const { engine } = this.port();
    const cfgByName = new Map(plan.configurations.map((c) => [c.name, c]));
    const taskBySlug = new Map((plan.suite.tasks ?? []).map((t) => [t.slug, t]));
    const results = await Promise.all(
      pending.map(async (t) => {
        try {
          const req = trialDispatch(
            { id: row.id, factoryId: row.factory_id },
            plan.suite,
            cfgByName.get(t.configuration)!,
            taskBySlug.get(t.task)!,
            t,
            plan.repetitions,
            plan.agents,
          );
          // Idempotent on the trial ID: a reset between the dispatch and the commit repeats nothing.
          return { t, out: await engine.dispatch(req, t.id) } as const;
        } catch (e) {
          return { t, err: toOfError(e).error.detail } as const;
        }
      }),
    );
    const cancelRuns: string[] = [];
    this.tx(() => {
      const fresh = this.benchRow()!;
      const cancelled = fresh.state === BENCHMARK_CANCELLED;
      const now = this.clock();
      for (const r of results) {
        const cur = first<{ data: string }>(this.sql, "SELECT data FROM trials WHERE id = ?", r.t.id);
        const t = JSON.parse(cur!.data) as Trial;
        if (t.status !== "pending" && !(cancelled && t.status === "cancelled" && !t.run_id)) continue;
        if ("err" in r) {
          t.status = cancelled ? "cancelled" : "error";
          t.error = r.err;
        } else {
          t.run_id = r.out.runId;
          if (r.out.workItemId) t.work_item_id = r.out.workItemId;
          if (cancelled) {
            t.status = "cancelled";
            t.run_state = "cancelled";
            cancelRuns.push(r.out.runId);
          } else {
            t.status = "running";
            t.started_at = iso(now);
          }
        }
        this.sql.exec(
          "UPDATE trials SET state = ?, run_id = ?, work_item_id = ?, data = ? WHERE id = ?",
          t.status,
          t.run_id ?? "",
          t.work_item_id ?? "",
          JSON.stringify(t),
          t.id,
        );
      }
      for (const id of cancelRuns)
        this.sql.exec("INSERT OR IGNORE INTO cancel_queue (run_id, actor) VALUES (?, 'benchmark')", id);
      if (cancelRuns.length > 0) this.setTimer(T_CANCEL, T_CANCEL, now);
      if (cancelled) return;
      const left = first(this.sql, "SELECT 1 FROM trials WHERE state = 'pending' LIMIT 1");
      if (left) {
        // +1: BaseDO deletes the fired timer by (name, due), so the next step must differ.
        this.setTimer(T_DISPATCH, T_DISPATCH, now + 1);
        return;
      }
      // Every trial is dispatched. No trial started: the run failed.
      const started = first(this.sql, "SELECT 1 FROM trials WHERE run_id <> '' LIMIT 1");
      const trials = this.loadTrials();
      const plan2 = JSON.parse(fresh.data) as BenchmarkPlan;
      const state = started ? benchmarkState(trials) : BENCHMARK_FAILED;
      const version = fresh.version + 1;
      const finished = benchmarkTerminal(state) ? now : null;
      this.sql.exec("UPDATE bench SET state = ?, version = ?, finished_ms = ? WHERE k = 1", state, version, finished);
      const next: BenchRow = { ...fresh, state, version, finished_ms: finished };
      this.project(next, this.view(next, plan2, trials));
    });
  }

  // ---------- cancel in chunks ----------

  private async cancelStep(): Promise<void> {
    const rows = all<{ run_id: string; actor: string }>(
      this.sql,
      "SELECT run_id, actor FROM cancel_queue LIMIT ?",
      CANCEL_CHUNK,
    );
    if (rows.length === 0) return;
    const { engine } = this.port();
    await Promise.all(
      rows.map(async (r) => {
        try {
          await engine.cancel(r.run_id, r.actor);
        } catch (e) {
          if (toOfError(e).error.code !== "resource_not_found")
            console.warn(`BenchmarkDO: cancel ${r.run_id} failed`, e);
        }
      }),
    );
    this.tx(() => {
      for (const r of rows) this.sql.exec("DELETE FROM cancel_queue WHERE run_id = ?", r.run_id);
      if (first(this.sql, "SELECT 1 FROM cancel_queue LIMIT 1")) this.setTimer(T_CANCEL, T_CANCEL, this.clock() + 1);
    });
  }

  // ---------- trial lifecycle ----------

  private trialById(id: string): Trial | undefined {
    const r = first<{ data: string }>(this.sql, "SELECT data FROM trials WHERE id = ?", id);
    return r ? (JSON.parse(r.data) as Trial) : undefined;
  }

  private async onTrialRun(runId: string): Promise<void> {
    const row = this.benchRow();
    if (!row || benchmarkTerminal(row.state)) return;
    const { runs } = this.port();
    const run = await runs.getRun(runId);
    if (!run || !finishedForScoring(run.state)) return;
    // Descendants of a trial inherit the metadata; only the root run is a trial.
    const trialId = run.metadata?.["benchmark_trial"];
    if (!trialId || run.parent_run_id) return;
    const t = this.trialById(trialId);
    if (!t) return;
    if (!t.run_id) {
      // The run finished before the dispatch step recorded it: adopt it.
      t.run_id = run.id;
      if (run.work_item_id) t.work_item_id = run.work_item_id;
      t.status = "running";
      t.started_at = run.created_at;
    } else if (t.run_id !== run.id) {
      return;
    }
    if (!trialRunFinished(t)) {
      await this.finalizeTrial(row.factory_id, t, run);
      this.persist([t]);
    } else if (t.scored) {
      return;
    }
    await this.scoreTrial(trialId);
  }

  /** Fills the outcome, the usage (with descendants) and the duration of a trial. */
  private async finalizeTrial(factoryId: string, t: Trial, run: Run): Promise<void> {
    const { runs } = this.port();
    t.run_state = run.state;
    switch (run.state) {
      case "cancelled":
        t.status = "cancelled";
        break;
      case "error":
        t.status = "error"; // no scoring: the run did not produce evidence to judge
        break;
      case "idle":
      case "blocked": {
        t.status = "finished";
        const items = await runs.listOpenInbox(factoryId);
        if (items.some((it) => it.run_id === run.id || (t.work_item_id && it.work_item_id === t.work_item_id)))
          t.status = "blocked";
        break;
      }
      default:
        t.status = "finished";
    }
    if (run.error) t.error = run.error;
    const usage = await treeUsage(runs, run);
    t.cost_usd = usage.cost_usd;
    t.input_tokens = usage.input_tokens;
    t.output_tokens = usage.output_tokens;
    const now = this.clock();
    const end = run.finished_at ? Date.parse(run.finished_at) : now;
    t.finished_at = iso(end);
    const created = Date.parse(run.created_at);
    if (run.finished_at && !Number.isNaN(created)) t.duration_ms = end - created;
    else if ((run.usage?.duration_ms ?? 0) > 0) t.duration_ms = run.usage.duration_ms;
    else if (!Number.isNaN(created)) t.duration_ms = now - created;
  }

  /** Starts one judge run for each scorer of a finished trial that has no result and no judge run. */
  private async scoreTrial(trialId: string): Promise<void> {
    const row = this.benchRow();
    if (!row || benchmarkTerminal(row.state)) return;
    const plan = JSON.parse(row.data) as BenchmarkPlan;
    const t = this.trialById(trialId);
    if (!t || t.scored || t.status === "error" || !trialRunFinished(t) || !t.run_id) return;
    const { runs, engine } = this.port();
    const run = await runs.getRun(t.run_id);
    if (!run) throw new OfError("resource_not_found", `trial run ${t.run_id} not found`);
    let refs = (await runs.loadStartCommits(run.id)).map((s) => ({
      codeForge: s.codeForge,
      owner: s.owner,
      repo: s.repo,
      ref: s.ref,
    }));
    if (refs.length === 0) refs = t.starting_repo_refs ?? [];
    let changed = false;
    for (const sc of trialScorers(plan, t)) {
      if (trialScore(t, sc.key)) continue;
      const ts: TrialScore = { scorer: sc.key, score: 0, passed: false };
      try {
        const exec: JudgeExec = {};
        if (sc.harness) exec.harness = sc.harness;
        if (sc.machine) exec.machine = sc.machine;
        if (sc.secrets) exec.secrets = sc.secrets;
        if (sc.mcp_servers) exec.mcp_servers = sc.mcp_servers;
        const req = await judgeRequest(runs, {
          factoryId: row.factory_id,
          name: sc.key,
          rubric: sc.rubric,
          labels: sc.labels,
          exec,
          scored: run,
          includeDescendants: true,
          refs,
        });
        req.triggeredBy = "benchmark:" + row.id;
        req.metadata = {
          [META_JUDGE]: JUDGE_TRIAL,
          [META_JUDGE_BENCHMARK]: row.id,
          [META_JUDGE_TRIAL]: t.id,
          [META_JUDGE_KEY]: clip(sc.key, MAX_METADATA_VALUE),
          [META_JUDGE_SCORED_RUN]: run.id,
        };
        // The ID is a hash of (benchmark, trial, scorer): a retry reaches the same judge run.
        const judgeId = await deterministicRunId("trial-judge", row.id, t.id, sc.key);
        await engine.startJudge(req, judgeId);
        ts.judge_run_id = judgeId;
        ts.pending = true;
      } catch (e) {
        ts.error = toOfError(e).error.detail;
      }
      setTrialScore(t, ts);
      changed = true;
    }
    if (!changed) return;
    this.completeTrialScores(plan, t);
    this.persist([t]);
    if (t.scored) await this.releaseTrial(row.id, t);
  }

  /** A trial is scored when each of its scorers has a result or an error. */
  private completeTrialScores(plan: BenchmarkPlan, t: Trial): void {
    if (t.scored) return;
    for (const sc of trialScorers(plan, t)) {
      const ts = trialScore(t, sc.key);
      if (!ts || ts.pending) return;
    }
    t.scored = true;
  }

  /** Ends the session of an idle, blocked or succeeded trial once its evidence is scored. */
  private async releaseTrial(benchId: string, t: Trial): Promise<void> {
    if (!t.run_id || !t.run_state || !(waitsForInput(t.run_state) || t.run_state === "succeeded")) return;
    try {
      await this.port().engine.cancel(t.run_id, "benchmark:" + benchId);
    } catch (e) {
      console.warn(`BenchmarkDO: cancel idle trial run ${t.run_id} failed`, e);
    }
  }

  private async onTrialJudge(judgeRunId: string): Promise<void> {
    const { runs, engine } = this.port();
    const judge = await runs.getRun(judgeRunId);
    if (!judge || !finishedForScoring(judge.state)) return;
    const benchId = judge.metadata?.[META_JUDGE_BENCHMARK] ?? "";
    const trialId = judge.metadata?.[META_JUDGE_TRIAL] ?? "";
    if (!benchId || !trialId) return;
    const row = this.benchRow();
    if (!row || row.id !== benchId || benchmarkTerminal(row.state)) return;
    const plan = JSON.parse(row.data) as BenchmarkPlan;
    const t = this.trialById(trialId);
    if (!t) return;
    const ts = (t.scores ?? []).find((s) => s.judge_run_id === judge.id);
    if (!ts || !ts.pending) return;
    const usage = await treeUsage(runs, judge);
    let answer: { label: string; rationale: string } | undefined;
    let jerr = "";
    try {
      answer = await judgeAnswer(runs, judge);
    } catch (e) {
      jerr = e instanceof Error ? e.message : String(e);
    }
    if (!isTerminalState(judge.state)) {
      try {
        await engine.cancel(judge.id, "scoring");
      } catch (e) {
        if (toOfError(e).error.code !== "resource_not_found")
          console.warn(`BenchmarkDO: stop judge ${judge.id} failed`, e);
      }
    }
    const sc: BenchmarkScorer | undefined = trialScorers(plan, t).find((x) => x.key === ts.scorer);
    ts.pending = false;
    ts.judge_cost_usd = usage.cost_usd;
    t.judge_cost_usd = (t.judge_cost_usd ?? 0) + usage.cost_usd;
    if (!sc) ts.error = "unknown scorer";
    else if (!answer) ts.error = jerr;
    else {
      const l = resolveLabel(sc.labels, answer.label);
      if (l) {
        ts.label = l.value;
        ts.score = l.score;
        ts.passed = l.score >= sc.passing_score;
        ts.rationale = answer.rationale;
      } else {
        ts.error = `judge returned undeclared label ${JSON.stringify(answer.label)}`;
      }
    }
    this.completeTrialScores(plan, t);
    this.persist([t]);
    if (t.scored) await this.releaseTrial(row.id, t);
  }

  // ---------- reconcile ----------

  /** Re-checks every unfinished trial against the runs and scores finished trials (the Go sweep). */
  private async reconcile(): Promise<void> {
    const row = this.benchRow();
    if (!row || benchmarkTerminal(row.state)) return;
    const { runs } = this.port();
    const errs: unknown[] = [];
    for (const t of this.loadTrials()) {
      try {
        if (t.status === "running" && t.run_id) {
          const run = await runs.getRun(t.run_id);
          if (run && finishedForScoring(run.state)) await this.onTrialRun(run.id);
        } else if (trialRunFinished(t) && !trialDone(t)) {
          await this.scoreTrial(t.id);
          for (const ts of this.trialById(t.id)?.scores ?? []) {
            if (!ts.pending || !ts.judge_run_id) continue;
            const judge = await runs.getRun(ts.judge_run_id);
            if (judge && finishedForScoring(judge.state)) await this.onTrialJudge(judge.id);
          }
        }
      } catch (e) {
        errs.push(e);
      }
    }
    // Persist a state change when every trial is done already (for example all errored late).
    const cur = this.benchRow();
    if (cur && !benchmarkTerminal(cur.state)) {
      const trials = this.loadTrials();
      if (benchmarkState(trials) !== cur.state) this.persist([]);
      const after = this.benchRow();
      if (after && !benchmarkTerminal(after.state))
        this.setTimer(T_RECONCILE, T_RECONCILE, this.clock() + RECONCILE_EVERY_MS);
    }
    if (errs.length > 0) throw errs[0];
  }
}

function setTrialScore(t: Trial, ts: TrialScore): void {
  const list = (t.scores ??= []);
  const i = list.findIndex((s) => s.scorer === ts.scorer);
  if (i >= 0) list[i] = ts;
  else list.push(ts);
}

export type { BenchmarkConfiguration, BenchmarkSuite };
