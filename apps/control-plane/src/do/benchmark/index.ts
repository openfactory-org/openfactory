// BenchmarkDO (key team:<t>|bench:<token>): one benchmark run. It owns the
// run, its trials and the chunk cursor; the summary goes to IndexDO
// (`index.project`, entity benchmark_run) for the lists.
//
// Port of the benchmark part of internal/scoring (benchmark.go) with these
// changes:
// - Trials are dispatched in chunks driven by one alarm timer, each dispatch
//   idempotent on the trial ID, so a reset DO repeats no trial.
// - The in-process lockKey is a per-DO promise chain. Network calls (engine,
//   runs) run outside any transaction; state changes are one `tx` each.
// - A cancel marks the trials at once and cancels the runs in chunks.
// - A reconcile timer re-checks unfinished trials every 15 minutes (the Go sweep).
//
// The tables and projections are in repo.ts (BenchmarkRepo); the scoring of
// trials (judge runs, reconcile) is in trial-scoring.ts (TrialScoringCoordinator).
import { OfError, newRoutedId, toOfError, type BenchmarkConfiguration, type BenchmarkSuite } from "@openfactory/core";
import type { CallMeta, BenchmarkRpc } from "../../contracts/do-messages";
import type { Env } from "../../contracts/env";
import { requireRole } from "../../auth/roles";
import { BaseDO } from "../../lib/base";
import { dest } from "../../lib/outbox/dest";
import type { Migration } from "../../lib/sql/migrate";
import {
  BENCHMARK_CANCELLED,
  BENCHMARK_FAILED,
  BENCHMARK_RUNNING,
  applyRepoSubstitutions,
  benchmarkState,
  benchmarkTerminal,
  trialDispatch,
  type Trial,
} from "../../scoring/benchmark";
import { workEngine, workRunsPort } from "../../scoring/clients";
import m0001 from "./migrations/0001_init";
import {
  BenchmarkRepo,
  benchmarkView,
  indexSummary,
  iso,
  parsePlan,
  type BenchRow,
  type BenchmarkPlan,
  type BenchmarkRunJson,
  type StartBenchmarkInput,
} from "./repo";
import { TrialScoringCoordinator, type BenchmarkDeps } from "./trial-scoring";

export type { BenchmarkPlan, BenchmarkRunJson, StartBenchmarkInput } from "./repo";
export type { BenchmarkDeps } from "./trial-scoring";

const MIGRATIONS: readonly Migration[] = [{ version: 1, name: "init", sql: m0001 }];

/** Trials dispatched per alarm step. */
export const DISPATCH_CHUNK = 5;
/** Run cancels per alarm step. */
export const CANCEL_CHUNK = 10;
export const RECONCILE_EVERY_MS = 15 * 60_000;

const T_DISPATCH = "bench.dispatch";
const T_CANCEL = "bench.cancel";
const T_RECONCILE = "bench.reconcile";

export class BenchmarkDO extends BaseDO implements BenchmarkRpc {
  /** Overridden by tests. The default talks to WorkDO and TeamDO. */
  deps: BenchmarkDeps | undefined;
  /**
   * The clock of the timestamps in the run and its trials. Overridden by tests.
   * Timers use the real time: the alarm fires on it.
   */
  clock: () => number = Date.now;
  private readonly repo: BenchmarkRepo;
  private readonly scoring: TrialScoringCoordinator;
  private chain: Promise<unknown> = Promise.resolve();

  constructor(ctx: DurableObjectState, env: Env) {
    super(ctx, env);
    this.repo = new BenchmarkRepo(this.sql);
    this.scoring = new TrialScoringCoordinator({
      repo: this.repo,
      port: () => this.port(),
      clock: () => this.clock(),
      persist: (changed) => this.persist(changed),
    });
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

  private mustBench(): { row: BenchRow; plan: BenchmarkPlan } {
    const row = this.repo.row();
    if (!row) throw new OfError("resource_not_found", "benchmark run not found");
    return { row, plan: parsePlan(row) };
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
      for (const t of changed) this.repo.writeTrial(t);
      opts.extra?.();
      const trials = this.repo.trials();
      let state = row.state;
      if (opts.state) state = opts.state;
      else if (!benchmarkTerminal(row.state)) state = benchmarkState(trials);
      let finished = row.finished_ms;
      if (benchmarkTerminal(state) && finished === null) finished = this.clock();
      const version = row.version + 1;
      this.repo.updateRun(state, version, finished, plan);
      const next: BenchRow = { ...row, state, version, finished_ms: finished, data: JSON.stringify(plan) };
      const v = benchmarkView(next, plan, trials);
      this.project(next, v);
      return v;
    });
  }

  private project(row: BenchRow, v: BenchmarkRunJson): void {
    this.enqueue({
      dest: dest.index(this.teamId()),
      kind: "index.project",
      msgId: `bench:${row.id}:${row.version}`,
      payload: { entity: "benchmark_run", id: row.id, version: row.version, summary: indexSummary(v) },
    });
  }

  // ---------- RPC ----------

  /** POST /factories/{uid}/benchmarks/{suite}/runs: creates the run and its trials and starts dispatching. */
  async start(meta: CallMeta, input: unknown): Promise<BenchmarkRunJson> {
    return this.rpc(() =>
      this.serial(async () => {
        const inp = input as StartBenchmarkInput;
        requireRole(meta.principal, inp.factoryId, "editor");
        const existing = this.repo.row();
        if (existing) return benchmarkView(existing, parsePlan(existing), this.repo.trials());
        const plan = inp.plan;
        if (!plan?.suite?.tasks || plan.suite.tasks.length === 0)
          throw new OfError("invalid_request", `benchmark suite ${JSON.stringify(plan?.suite?.slug)} has no tasks`);
        const token = this.token();
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
          const row = this.repo.insertRun(inp, BENCHMARK_RUNNING, this.clock(), trials);
          const wall = Date.now();
          this.setTimer(T_DISPATCH, T_DISPATCH, wall);
          this.setTimer(T_RECONCILE, T_RECONCILE, wall + RECONCILE_EVERY_MS);
          const v = benchmarkView(row, plan, trials);
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
      return benchmarkView(row, plan, this.repo.trials());
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
        if (row.state === BENCHMARK_CANCELLED) return benchmarkView(row, plan, this.repo.trials());
        if (benchmarkTerminal(row.state)) throw new OfError("conflict", `benchmark run ${row.id} is ${row.state}`);
        const actor = meta.principal?.userId ? "user:" + meta.principal.userId : (meta.principal?.id ?? "api");
        const trials = this.repo.trials();
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
            for (const id of queue) this.repo.queueCancel(id, actor);
            if (queue.length > 0) this.setTimer(T_CANCEL, T_CANCEL, Date.now());
            this.clearTimer(T_DISPATCH);
            this.clearTimer(T_RECONCILE);
          },
        });
      }),
    );
  }

  /** A root run of a trial finished or waits for input (queue job bench.trialUpdate / scoring.runFinished). */
  async trialRunUpdate(runId: string): Promise<void> {
    return this.rpc(() => this.serial(() => this.scoring.onTrialRun(runId)));
  }

  /** A judge run of a trial finished or waits for input. */
  async trialJudgeUpdate(judgeRunId: string): Promise<void> {
    return this.rpc(() => this.serial(() => this.scoring.onTrialJudge(judgeRunId)));
  }

  // ---------- dispatch in chunks ----------

  private async dispatchStep(): Promise<void> {
    const row = this.repo.row();
    if (!row || benchmarkTerminal(row.state)) return;
    const plan = parsePlan(row);
    const pending = this.repo.pendingTrials(DISPATCH_CHUNK);
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
      const fresh = this.repo.row()!;
      const cancelled = fresh.state === BENCHMARK_CANCELLED;
      const now = this.clock();
      for (const r of results) {
        const t = this.repo.trial(r.t.id)!;
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
        this.repo.writeTrial(t);
      }
      for (const id of cancelRuns) this.repo.queueCancel(id, "benchmark");
      if (cancelRuns.length > 0) this.setTimer(T_CANCEL, T_CANCEL, Date.now());
      if (cancelled) return;
      if (this.repo.hasPendingTrial()) {
        this.setTimer(T_DISPATCH, T_DISPATCH, Date.now());
        return;
      }
      // Every trial is dispatched. No trial started: the run failed.
      const trials = this.repo.trials();
      const state = this.repo.hasStartedTrial() ? benchmarkState(trials) : BENCHMARK_FAILED;
      const version = fresh.version + 1;
      const finished = benchmarkTerminal(state) ? now : null;
      this.repo.updateRun(state, version, finished);
      const next: BenchRow = { ...fresh, state, version, finished_ms: finished };
      this.project(next, benchmarkView(next, parsePlan(fresh), trials));
    });
  }

  // ---------- cancel in chunks ----------

  private async cancelStep(): Promise<void> {
    const rows = this.repo.cancelBatch(CANCEL_CHUNK);
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
      for (const r of rows) this.repo.dequeueCancel(r.run_id);
      if (this.repo.hasQueuedCancel()) this.setTimer(T_CANCEL, T_CANCEL, Date.now());
    });
  }

  // ---------- reconcile ----------

  /** Re-checks every unfinished trial (the Go sweep), then moves the run state and the next check. */
  private async reconcile(): Promise<void> {
    const row = this.repo.row();
    if (!row || benchmarkTerminal(row.state)) return;
    const errs = await this.scoring.reconcileTrials();
    // Persist a state change when every trial is done already (for example all errored late).
    const cur = this.repo.row();
    if (cur && !benchmarkTerminal(cur.state)) {
      if (benchmarkState(this.repo.trials()) !== cur.state) this.persist([]);
      const after = this.repo.row();
      if (after && !benchmarkTerminal(after.state))
        this.setTimer(T_RECONCILE, T_RECONCILE, Date.now() + RECONCILE_EVERY_MS);
    }
    if (errs.length > 0) throw errs[0];
  }
}

export type { BenchmarkConfiguration, BenchmarkSuite };
