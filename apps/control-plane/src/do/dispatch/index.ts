// DispatchDO (key team:<t>|dispatch): admission only (C9). A FIFO per
// target, open-session leases by (run, session), the factory limit, provider
// start-rate buckets, spend per (run, session) bucketed by the run's created
// day and month (C5), the queue timeout, lease reconcile and the orphan sweep.
import { doName, parseTaskId, partitionOf } from "@openfactory/core";
import type { DispatchTarget, OutboxPayloads } from "../../contracts/do-messages";
import type { Env } from "../../contracts/env";
import {
  admit,
  refill,
  START_RATES,
  type Bucket,
  type LeaseRow,
  type WaitingRow,
  type WorkerRow,
} from "../../engine/admission";
import { parseDuration } from "../../engine/sessions";
import { BaseDO, type Migration } from "../../lib/base";
import { dest } from "../../lib/outbox/dest";
import { all, first } from "../../lib/sql/query";
import {
  buildLimits,
  dayKey,
  exceeded,
  monthKey,
  noticeText,
  parseSpendLimit,
  warnings,
  warnKey,
  type Limit,
} from "../../spend/spend";
import { m0001 } from "./migrations/0001_init";
import { providerFor, registeredProviders, targetKey } from "./providers";

const SWEEP_EVERY_MS = 60_000;
const ORPHAN_MIN_AGE_MS = 2 * 60_000;
const RECONCILE_EVERY_MS = 24 * 3_600_000;

type UsagePayload = OutboxPayloads["dispatch.usage"] & { rootRunId?: string; workDO?: string };

export class DispatchDO extends BaseDO {
  private seq = 0;
  /**
   * First time the sweep saw a task that reports no startedAt. Lost on
   * eviction, which only delays the destroy of such a task.
   */
  private readonly firstSeen = new Map<string, number>();

  constructor(ctx: DurableObjectState, env: Env) {
    super(ctx, env);
    this.onMessage("dispatch.request", (p) => {
      this.onRequest(p);
    });
    this.onMessage("dispatch.cancel", (p) => {
      this.sql.exec("DELETE FROM waiting WHERE run_id = ? AND session = ?", p.runId, p.session);
      this.clearTimer(qtName(p.runId, p.session));
      this.admission();
    });
    this.onMessage("dispatch.release", (p) => {
      this.sql.exec("DELETE FROM leases WHERE run_id = ? AND session = ?", p.runId, p.session);
      this.sql.exec("DELETE FROM waiting WHERE run_id = ? AND session = ?", p.runId, p.session);
      this.clearTimer(qtName(p.runId, p.session));
      this.admission();
    });
    this.onMessage("dispatch.taskStarted", (p) => {
      this.sql.exec(
        "UPDATE leases SET started_ms = ? WHERE run_id = ? AND session = ?",
        p.startedMs,
        p.runId,
        p.session,
      );
    });
    this.onMessage("dispatch.limits", (p) => {
      const sl = parseSpendLimit(p.spendLimit);
      this.sql.exec(
        `INSERT INTO factory_limits (factory_id, max_concurrent, daily_usd, monthly_usd, version_id) VALUES (?, ?, ?, ?, ?)
         ON CONFLICT(factory_id) DO UPDATE SET max_concurrent = excluded.max_concurrent, daily_usd = excluded.daily_usd,
           monthly_usd = excluded.monthly_usd, version_id = excluded.version_id`,
        p.factoryId,
        p.maxConcurrentRuns ?? 0,
        sl.daily ?? 0,
        sl.monthly ?? 0,
        p.versionId,
      );
      this.admission();
    });
    this.onMessage("capacity.report", (p) => {
      const cur = first<{ generation: number }>(
        this.sql,
        "SELECT generation FROM workers WHERE worker_id = ?",
        p.workerId,
      );
      if (cur && cur.generation > p.generation) return;
      this.sql.exec(
        `INSERT INTO workers (worker_id, kind, online, draining, slots, used, labels, factories, generation, seen_ms)
         VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
         ON CONFLICT(worker_id) DO UPDATE SET kind = excluded.kind, online = excluded.online, draining = excluded.draining,
           slots = excluded.slots, used = excluded.used, labels = excluded.labels, factories = excluded.factories,
           generation = excluded.generation, seen_ms = excluded.seen_ms`,
        p.workerId,
        p.kind,
        p.online ? 1 : 0,
        p.draining ? 1 : 0,
        p.slots,
        p.used,
        JSON.stringify(p.labels ?? {}),
        JSON.stringify(p.factories ?? []),
        p.generation,
        Date.now(),
      );
      this.admission();
    });
    this.onMessage("dispatch.usage", (p) => {
      this.onUsage(p as UsagePayload);
    });
    this.onTimer<{ runId: string; session: number; workDO: string }>("queue_timeout", (t) => {
      const w = first(
        this.sql,
        "SELECT hold FROM waiting WHERE run_id = ? AND session = ?",
        t.payload.runId,
        t.payload.session,
      );
      if (!w) return;
      this.sql.exec("DELETE FROM waiting WHERE run_id = ? AND session = ?", t.payload.runId, t.payload.session);
      this.enqueue({
        dest: workDest(t.payload.workDO),
        kind: "work.queueExpired",
        lane: laneOf(t.payload.workDO),
        msgId: `qexp:${t.payload.runId}:${t.payload.session}`,
        payload: { runId: t.payload.runId, session: t.payload.session },
      });
    });
    this.onTimer("bucket", () => this.admission());
    // A spend hold ends when the UTC day or month of the exceeded limit rolls over.
    this.onTimer("spend", () => this.admission());
    this.onTimerAsync("sweep", () => this.sweep());
    this.onTimerAsync("reconcile", () => this.reconcile());
  }

  protected migrations(): readonly Migration[] {
    return [m0001];
  }

  protected override init(): void {
    const r = first<{ m: number | null }>(this.sql, "SELECT MAX(seq) AS m FROM waiting");
    this.seq = r?.m ?? 0;
  }

  async ping(): Promise<string> {
    return "DispatchDO";
  }

  private queueTimeoutMs(): number {
    return parseDuration(this.env.OF_QUEUE_TIMEOUT, 30 * 60_000);
  }

  private maxSandboxes(): number {
    return Number(this.env.OF_MAX_SANDBOXES ?? "") || 0;
  }

  // ---------- requests ----------

  private onRequest(p: OutboxPayloads["dispatch.request"]): void {
    if (first(this.sql, "SELECT 1 FROM leases WHERE run_id = ? AND session = ?", p.runId, p.session)) return;
    if (first(this.sql, "SELECT 1 FROM waiting WHERE run_id = ? AND session = ?", p.runId, p.session)) return;
    const now = Date.now();
    this.seq++;
    this.sql.exec(
      `INSERT INTO waiting (run_id, session, factory_id, target, target_key, work_do, restricted, run_created_ms, enqueued_ms, seq)
       VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
      p.runId,
      p.session,
      p.factoryId,
      JSON.stringify(p.target),
      targetKey(p.target),
      p.workDO,
      p.restricted ? 1 : 0,
      p.runCreatedMs,
      now,
      this.seq,
    );
    this.setTimer(qtName(p.runId, p.session), "queue_timeout", Math.max(p.queuedMs, 0) + this.queueTimeoutMs() + 1, {
      runId: p.runId,
      session: p.session,
      workDO: p.workDO,
    });
    this.admission();
    this.timers.setEarliest("sweep", "sweep", now + SWEEP_EVERY_MS);
    this.timers.setEarliest("reconcile", "reconcile", now + RECONCILE_EVERY_MS);
  }

  /** The admission pass. Runs inside the caller's transaction. */
  private admission(): void {
    const now = Date.now();
    const waiting = all<{
      run_id: string;
      session: number;
      factory_id: string;
      target: string;
      target_key: string;
      work_do: string;
      hold: string;
    }>(
      this.sql,
      "SELECT run_id, session, factory_id, target, target_key, work_do, hold FROM waiting ORDER BY enqueued_ms, seq",
    );
    if (waiting.length === 0) return;
    const leases = all<LeaseRow & Record<string, SqlStorageValue>>(
      this.sql,
      "SELECT run_id, session, factory_id, target_key, provider, worker_id FROM leases",
    );
    const limits = new Map<string, number>();
    for (const r of all<{ factory_id: string; max_concurrent: number }>(
      this.sql,
      "SELECT factory_id, max_concurrent FROM factory_limits",
    )) {
      limits.set(r.factory_id, r.max_concurrent);
    }
    const workers: WorkerRow[] = all<{
      worker_id: string;
      online: number;
      draining: number;
      slots: number;
      used: number;
      labels: string;
      factories: string;
    }>(this.sql, "SELECT worker_id, online, draining, slots, used, labels, factories FROM workers").map((w) => ({
      worker_id: w.worker_id,
      online: !!w.online,
      draining: !!w.draining,
      slots: w.slots,
      used: w.used,
      labels: JSON.parse(w.labels) as Record<string, string>,
      factories: JSON.parse(w.factories) as string[],
    }));
    const buckets = new Map<string, Bucket[]>();
    const rows: WaitingRow[] = waiting.map((w) => ({ ...w, target: JSON.parse(w.target) as WaitingRow["target"] }));
    for (const w of rows) {
      if (w.target.kind !== "provider" || buckets.has(w.target.providerId)) continue;
      const rates = START_RATES[w.target.providerId];
      if (!rates) continue;
      buckets.set(
        w.target.providerId,
        rates.map((r) =>
          refill(
            this.bucket(w.target.kind === "provider" ? w.target.providerId : "", r.windowSeconds, r.limit, now),
            now,
          ),
        ),
      );
    }
    const spendBlocked = new Set<string>();
    let spendResetMs: number | undefined;
    for (const f of new Set(rows.map((r) => r.factory_id))) {
      const over = exceeded(this.limitsOf(f, now));
      if (!over) continue;
      spendBlocked.add(f);
      const reset = Date.parse(over.resets_at);
      if (spendResetMs === undefined || reset < spendResetMs) spendResetMs = reset;
    }
    // Held rows have no queue timeout, and no other event may come: re-run at the rollover.
    if (spendResetMs !== undefined) this.timers.setEarliest("spend", "spend", spendResetMs);
    const res = admit(
      {
        waiting: rows,
        leases,
        factoryLimits: limits,
        workers,
        buckets,
        spendBlocked,
        maxSandboxes: this.maxSandboxes(),
      },
      now,
    );
    for (const [provider, bs] of res.buckets) {
      for (const b of bs) {
        this.sql.exec(
          `INSERT INTO provider_buckets (provider, window_s, tokens, cap, updated_ms) VALUES (?, ?, ?, ?, ?)
           ON CONFLICT(provider, window_s) DO UPDATE SET tokens = excluded.tokens, cap = excluded.cap, updated_ms = excluded.updated_ms`,
          provider,
          b.window_s,
          b.tokens,
          b.cap,
          b.updated_ms,
        );
      }
    }
    const byKey = new Map(waiting.map((w) => [`${w.run_id}.${w.session}`, w]));
    for (const g of res.grants) {
      const w = byKey.get(`${g.run_id}.${g.session}`)!;
      const target = JSON.parse(w.target) as DispatchTarget;
      const leaseId = `${g.run_id}.${g.session}`;
      this.sql.exec("DELETE FROM waiting WHERE run_id = ? AND session = ?", g.run_id, g.session);
      this.clearTimer(qtName(g.run_id, g.session));
      this.sql.exec(
        `INSERT INTO leases (run_id, session, task_id, factory_id, target_key, provider, worker_id, work_do, granted_ms)
         VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
        g.run_id,
        g.session,
        leaseId,
        w.factory_id,
        w.target_key,
        g.provider,
        g.worker_id,
        w.work_do,
        now,
      );
      this.enqueue({
        dest: workDest(w.work_do),
        kind: "dispatch.grant",
        lane: laneOf(w.work_do),
        msgId: `grant:${leaseId}`,
        payload: {
          runId: g.run_id,
          session: g.session,
          target,
          providerId: g.provider,
          ...(g.worker_id ? { workerId: g.worker_id } : {}),
          leaseId,
        },
      });
    }
    for (const w of res.spendHolds) {
      this.sql.exec("UPDATE waiting SET hold = 'spend' WHERE run_id = ? AND session = ?", w.run_id, w.session);
      // Not armed while on spend hold (03 §7.2 step 3).
      this.clearTimer(qtName(w.run_id, w.session));
      const wd = byKey.get(`${w.run_id}.${w.session}`)!.work_do;
      this.enqueue({
        dest: workDest(wd),
        kind: "work.spendHold",
        lane: laneOf(wd),
        msgId: `shold:${w.run_id}:${w.session}:${dayKey(now)}`,
        payload: { runId: w.run_id, session: w.session, limit: null },
      });
    }
    for (const w of res.spendReleases) {
      this.sql.exec("UPDATE waiting SET hold = '' WHERE run_id = ? AND session = ?", w.run_id, w.session);
      const wd = byKey.get(`${w.run_id}.${w.session}`)!.work_do;
      // Re-based at release.
      this.setTimer(qtName(w.run_id, w.session), "queue_timeout", now + this.queueTimeoutMs() + 1, {
        runId: w.run_id,
        session: w.session,
        workDO: wd,
      });
      this.enqueue({
        dest: workDest(wd),
        kind: "work.spendRelease",
        lane: laneOf(wd),
        msgId: `srel:${w.run_id}:${w.session}:${now}`,
        payload: { runId: w.run_id, session: w.session },
      });
    }
    if (res.retryAtMs !== undefined) this.timers.setEarliest("bucket", "bucket", res.retryAtMs);
  }

  private bucket(provider: string, windowS: number, cap: number, now: number): Bucket {
    const r = first<{ tokens: number; cap: number; updated_ms: number }>(
      this.sql,
      "SELECT tokens, cap, updated_ms FROM provider_buckets WHERE provider = ? AND window_s = ?",
      provider,
      windowS,
    );
    return r
      ? { provider, window_s: windowS, tokens: r.tokens, cap, updated_ms: r.updated_ms }
      : { provider, window_s: windowS, tokens: cap, cap, updated_ms: now };
  }

  // ---------- spend (C5) ----------

  private limitsOf(factoryId: string, now: number): Limit[] {
    const fl = first<{ daily_usd: number; monthly_usd: number }>(
      this.sql,
      "SELECT daily_usd, monthly_usd FROM factory_limits WHERE factory_id = ?",
      factoryId,
    );
    if (!fl || (fl.daily_usd <= 0 && fl.monthly_usd <= 0)) return [];
    const total = (period: string, start: string) =>
      first<{ c: number }>(
        this.sql,
        "SELECT cost_usd AS c FROM spend_totals WHERE scope = 'factory' AND id = ? AND period = ? AND start = ?",
        factoryId,
        period,
        start,
      )?.c ?? 0;
    return buildLimits(
      "factory",
      { daily: fl.daily_usd, monthly: fl.monthly_usd },
      total("daily", dayKey(now)),
      total("monthly", monthKey(now)),
      now,
    );
  }

  private onUsage(p: UsagePayload): void {
    const cur = first<{ cost_usd: number; seq: number }>(
      this.sql,
      "SELECT cost_usd, seq FROM spend WHERE run_id = ? AND session = ?",
      p.runId,
      p.session,
    );
    if (cur && cur.seq >= p.seq) return;
    const delta = p.costUsd - (cur?.cost_usd ?? 0);
    const day = dayKey(p.runCreatedMs);
    const month = monthKey(p.runCreatedMs);
    this.sql.exec(
      `INSERT INTO spend (run_id, session, factory_id, day, month, cost_usd, seq, root_run_id, work_do) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
       ON CONFLICT(run_id, session) DO UPDATE SET cost_usd = excluded.cost_usd, seq = excluded.seq`,
      p.runId,
      p.session,
      p.factoryId,
      day,
      month,
      p.costUsd,
      p.seq,
      p.rootRunId ?? p.runId,
      p.workDO ?? doName.work(this.teamId(), partitionOf(p.runId)),
    );
    for (const [scope, id] of [
      ["factory", p.factoryId],
      ["team", this.teamId()],
    ] as const) {
      for (const [period, start] of [
        ["daily", day],
        ["monthly", month],
      ] as const) {
        this.sql.exec(
          `INSERT INTO spend_totals (scope, id, period, start, cost_usd) VALUES (?, ?, ?, ?, ?)
           ON CONFLICT(scope, id, period, start) DO UPDATE SET cost_usd = cost_usd + excluded.cost_usd`,
          scope,
          id,
          period,
          start,
          delta,
        );
      }
    }
    const now = Date.now();
    const limits = this.limitsOf(p.factoryId, now);
    const over = exceeded(limits);
    const wd = p.workDO ?? doName.work(this.teamId(), partitionOf(p.runId));
    if (over) {
      const root = p.rootRunId ?? p.runId;
      this.enqueue({
        dest: workDest(wd),
        kind: "work.spendStop",
        lane: laneOf(wd),
        msgId: `sstop:${root}:${over.scope}:${over.period}:${over.period_start.slice(0, 10)}`,
        payload: { rootRunId: root, limit: over },
      });
    }
    for (const w of warnings(limits)) {
      const key = warnKey(w, p.factoryId);
      if (first(this.sql, "SELECT 1 FROM spend_warned WHERE key = ?", key)) continue;
      this.sql.exec("INSERT INTO spend_warned (key, at_ms) VALUES (?, ?)", key, now);
      this.enqueue({
        dest: dest.index(this.teamId()),
        kind: "index.inboxCreate",
        msgId: key,
        payload: {
          item: { kind: "spend_limit", factory_id: p.factoryId, title: noticeText(w), body: noticeText(w), limit: w },
        },
      });
    }
    this.admission();
  }

  // ---------- RPC ----------

  async spendGate(factoryId: string): Promise<{ allowed: boolean; limit?: unknown }> {
    return this.rpc(() => {
      const over = exceeded(this.limitsOf(factoryId, Date.now()));
      return over ? { allowed: false, limit: over } : { allowed: true };
    });
  }

  async spend(factoryId: string): Promise<unknown> {
    return this.rpc(() => {
      const now = Date.now();
      const total = (scope: string, id: string, period: string, start: string) =>
        first<{ c: number }>(
          this.sql,
          "SELECT cost_usd AS c FROM spend_totals WHERE scope = ? AND id = ? AND period = ? AND start = ?",
          scope,
          id,
          period,
          start,
        )?.c ?? 0;
      return {
        team_id: this.teamId(),
        factory_id: factoryId,
        factory_daily_usd: total("factory", factoryId, "daily", dayKey(now)),
        factory_monthly_usd: total("factory", factoryId, "monthly", monthKey(now)),
        limits: this.limitsOf(factoryId, now),
      };
    });
  }

  /** Read model for tests and the API: waiting and leases. */
  async state(): Promise<{
    waiting: { run_id: string; session: number; hold: string }[];
    leases: { run_id: string; session: number; factory_id: string }[];
  }> {
    return this.rpc(() => ({
      waiting: all<{ run_id: string; session: number; hold: string }>(
        this.sql,
        "SELECT run_id, session, hold FROM waiting ORDER BY enqueued_ms, seq",
      ),
      leases: all<{ run_id: string; session: number; factory_id: string }>(
        this.sql,
        "SELECT run_id, session, factory_id FROM leases ORDER BY granted_ms, run_id",
      ),
    }));
  }

  async pump(): Promise<void> {
    await this.alarm();
  }

  // ---------- sweep and reconcile ----------

  /**
   * Orphan sweep (dispatch/sweep.go): destroy tasks older than 2 min without a
   * lease. Other requests run while this awaits the provider, so the lease
   * check reads the table just before each destroy, not once up front. A task
   * without startedAt gets its age from the first sweep that saw it.
   */
  private async sweep(): Promise<void> {
    const team = this.teamId();
    const leased = (taskId: string) => !!first(this.sql, "SELECT 1 FROM leases WHERE task_id = ?", taskId);
    const seen = new Set<string>();
    const providers = new Set([
      ...all<{ provider: string }>(this.sql, "SELECT DISTINCT provider FROM leases WHERE provider <> ''").map(
        (r) => r.provider,
      ),
      ...registeredProviders(),
    ]);
    const now = Date.now();
    for (const id of providers) {
      try {
        const p = providerFor(this.env, team, id);
        for (const t of await p.list()) {
          try {
            parseTaskId(t.taskId);
          } catch {
            continue;
          }
          // Read now, with no await before the destroy: a grant can land while
          // list() or an earlier destroy awaits.
          if (leased(t.taskId)) continue;
          const key = `${id}|${t.taskId}`;
          seen.add(key);
          const at = Date.now();
          let started = t.startedAt ? Date.parse(t.startedAt) : NaN;
          if (Number.isNaN(started)) {
            started = this.firstSeen.get(key) ?? at;
            this.firstSeen.set(key, started);
          }
          if (at - started < ORPHAN_MIN_AGE_MS) continue;
          this.firstSeen.delete(key);
          await p.destroy(t.taskId);
        }
      } catch (e) {
        console.warn("dispatch: sweep", id, e instanceof Error ? e.message : String(e));
      }
    }
    for (const k of this.firstSeen.keys()) if (!seen.has(k)) this.firstSeen.delete(k);
    if (first(this.sql, "SELECT 1 FROM leases LIMIT 1") || first(this.sql, "SELECT 1 FROM waiting LIMIT 1")) {
      this.setTimer("sweep", "sweep", now + SWEEP_EVERY_MS);
    }
  }

  /** Lease reconcile: ask each WorkDO about old leases; an ended session frees its lease. */
  private async reconcile(): Promise<void> {
    const now = Date.now();
    const old = all<{ run_id: string; session: number; work_do: string }>(
      this.sql,
      "SELECT run_id, session, work_do FROM leases WHERE granted_ms < ?",
      now - 10 * 60_000,
    );
    for (const l of old) {
      try {
        const st = await this.env.WORK.getByName(l.work_do).leaseCheck(l.run_id, l.session);
        if (st === "ended") {
          this.tx(() => {
            this.sql.exec("DELETE FROM leases WHERE run_id = ? AND session = ?", l.run_id, l.session);
            this.admission();
          });
        }
      } catch (e) {
        console.warn("dispatch: reconcile", l.run_id, e instanceof Error ? e.message : String(e));
      }
    }
    if (first(this.sql, "SELECT 1 FROM leases LIMIT 1"))
      this.setTimer("reconcile", "reconcile", now + RECONCILE_EVERY_MS);
  }
}

function qtName(runId: string, session: number): string {
  return `qt:${runId}.${session}`;
}

function workDest(workDO: string): string {
  return `do:WORK:${workDO}`;
}

function laneOf(workDO: string): string {
  const i = workDO.lastIndexOf("|work:");
  return `work:${i >= 0 ? workDO.slice(i + 6) : workDO}`;
}
