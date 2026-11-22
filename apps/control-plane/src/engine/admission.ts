// DispatchDO admission decision (03 §7.2 step 2): pure. Oldest waiting
// request first. A worker run needs a free worker slot; the factory limit
// (maxConcurrentRuns) and the provider start-rate buckets are exact, because
// DispatchDO is the only writer and decides in one transaction.
import type { RateWindow } from "@openfactory/sandbox";

export interface WaitingRow {
  run_id: string;
  session: number;
  factory_id: string;
  target: { kind: "provider"; providerId: string } | { kind: "worker"; workerId: string } | { kind: "pool"; label: string };
  target_key: string;
  hold: string;
}

export interface LeaseRow {
  run_id: string;
  session: number;
  factory_id: string;
  target_key: string;
  provider: string;
  worker_id: string;
}

export interface WorkerRow {
  worker_id: string;
  online: boolean;
  draining: boolean;
  slots: number;
  used: number;
  labels: Record<string, string>;
  factories: string[];
}

export interface Bucket {
  provider: string;
  window_s: number;
  tokens: number;
  cap: number;
  updated_ms: number;
}

/** Documented start-rate windows of providers (boat.dev: 12/min, 60/h, 200/day). */
export const START_RATES: Record<string, RateWindow[]> = {
  boatdev: [
    { limit: 12, windowSeconds: 60 },
    { limit: 60, windowSeconds: 3600 },
    { limit: 200, windowSeconds: 86400 },
  ],
};

/** Refills a bucket to `now`. */
export function refill(b: Bucket, nowMs: number): Bucket {
  const rate = b.cap / b.window_s; // tokens per second
  const tokens = Math.min(b.cap, b.tokens + ((nowMs - b.updated_ms) / 1000) * rate);
  return { ...b, tokens, updated_ms: nowMs };
}

/** Milliseconds until a bucket has one token. */
export function msUntilToken(b: Bucket): number {
  if (b.tokens >= 1) return 0;
  const rate = b.cap / b.window_s;
  return Math.ceil(((1 - b.tokens) / rate) * 1000);
}

export interface AdmissionInput {
  waiting: readonly WaitingRow[];
  leases: readonly LeaseRow[];
  /** factory ID -> maxConcurrentRuns (0 = no limit). */
  factoryLimits: ReadonlyMap<string, number>;
  workers: readonly WorkerRow[];
  /** provider -> its buckets (refilled to now). */
  buckets: ReadonlyMap<string, Bucket[]>;
  /** Factories over a spend limit. */
  spendBlocked: ReadonlySet<string>;
  /** OF_MAX_SANDBOXES for built-in providers; 0 = no limit. */
  maxSandboxes: number;
}

export interface Grant {
  run_id: string;
  session: number;
  provider: string;
  worker_id: string;
}

export interface AdmissionResult {
  grants: Grant[];
  /** Waiting rows to put on (or keep on) spend hold. */
  spendHolds: WaitingRow[];
  /** Spend-held rows whose hold ends. */
  spendReleases: WaitingRow[];
  /** Buckets after the grants (tokens taken). */
  buckets: Map<string, Bucket[]>;
  /** Earliest time a bucket gets a token for a blocked target, or undefined. */
  retryAtMs?: number;
}

export function admit(input: AdmissionInput, nowMs: number): AdmissionResult {
  const byFactory = new Map<string, number>();
  let builtin = 0;
  const workerUsed = new Map<string, number>();
  for (const l of input.leases) {
    byFactory.set(l.factory_id, (byFactory.get(l.factory_id) ?? 0) + 1);
    if (l.worker_id) workerUsed.set(l.worker_id, (workerUsed.get(l.worker_id) ?? 0) + 1);
    else builtin++;
  }
  const buckets = new Map<string, Bucket[]>();
  for (const [k, v] of input.buckets) buckets.set(k, v.map((b) => ({ ...b })));
  const out: AdmissionResult = { grants: [], spendHolds: [], spendReleases: [], buckets };
  const blockedTargets = new Set<string>();

  const freeWorker = (w: WorkerRow, factoryId: string): boolean =>
    w.online &&
    !w.draining &&
    (w.factories.length === 0 || w.factories.includes(factoryId)) &&
    (workerUsed.get(w.worker_id) ?? 0) < w.slots;

  for (const row of input.waiting) {
    if (input.spendBlocked.has(row.factory_id)) {
      if (row.hold !== "spend") out.spendHolds.push(row);
      continue;
    }
    if (row.hold === "spend") out.spendReleases.push(row);
    if (blockedTargets.has(row.target_key)) continue;
    const limit = input.factoryLimits.get(row.factory_id) ?? 0;
    if (limit > 0 && (byFactory.get(row.factory_id) ?? 0) >= limit) continue;
    let provider = "";
    let workerId = "";
    if (row.target.kind === "worker") {
      const w = input.workers.find((x) => x.worker_id === (row.target as { workerId: string }).workerId);
      if (!w || !freeWorker(w, row.factory_id)) {
        blockedTargets.add(row.target_key);
        continue;
      }
      workerId = w.worker_id;
    } else if (row.target.kind === "pool") {
      const label = row.target.label;
      const cands = input.workers
        .filter((w) => w.labels.pool === label || w.labels[label] !== undefined)
        .filter((w) => freeWorker(w, row.factory_id))
        .sort((a, b) => (workerUsed.get(a.worker_id) ?? 0) / a.slots - (workerUsed.get(b.worker_id) ?? 0) / b.slots);
      const w = cands[0];
      if (!w) {
        blockedTargets.add(row.target_key);
        continue;
      }
      workerId = w.worker_id;
    } else {
      provider = row.target.providerId;
      if (input.maxSandboxes > 0 && builtin >= input.maxSandboxes) {
        blockedTargets.add(row.target_key);
        continue;
      }
      const bs = buckets.get(provider);
      if (bs && bs.some((b) => b.tokens < 1)) {
        blockedTargets.add(row.target_key);
        const wait = Math.max(...bs.map(msUntilToken));
        out.retryAtMs = Math.min(out.retryAtMs ?? Infinity, nowMs + wait);
        continue;
      }
      if (bs) for (const b of bs) b.tokens -= 1;
      builtin++;
    }
    byFactory.set(row.factory_id, (byFactory.get(row.factory_id) ?? 0) + 1);
    if (workerId) workerUsed.set(workerId, (workerUsed.get(workerId) ?? 0) + 1);
    out.grants.push({ run_id: row.run_id, session: row.session, provider, worker_id: workerId });
  }
  return out;
}
