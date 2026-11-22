// Named timers multiplexed onto the one DO alarm (01 §5.3). A timer is a row
// (name, kind, due_ms, payload). BaseDO.alarm runs the due timers through the
// handler registered for their kind.
import { first } from "../sql/query";

export interface TimerRow<P = unknown> {
  name: string;
  kind: string;
  due_ms: number;
  payload: P;
  attempts: number;
}

/** Runs inside transactionSync. Writes and enqueues only; no await. */
export type SyncTimerHandler<P = unknown> = (t: TimerRow<P>) => void;
/** Runs outside any transaction (network I/O allowed). */
export type AsyncTimerHandler<P = unknown> = (t: TimerRow<P>) => Promise<void>;

export const TIMER_BATCH = 50;
export const TIMER_MAX_BACKOFF_MS = 5 * 60_000;

/** Backoff after a failed timer handler. */
export function timerBackoff(attempts: number): number {
  return Math.min(TIMER_MAX_BACKOFF_MS, 1000 * 2 ** Math.max(0, attempts - 1));
}

export class Timers {
  constructor(
    private readonly storage: DurableObjectStorage,
    private readonly now: () => number = Date.now,
  ) {}

  private get sql(): SqlStorage {
    return this.storage.sql;
  }

  /** Creates or moves a timer. Call inside transactionSync with the state change. */
  set(name: string, kind: string, dueMs: number, payload: unknown = null): void {
    this.sql.exec(
      `INSERT INTO timers (name, kind, due_ms, payload, attempts) VALUES (?, ?, ?, ?, 0)
       ON CONFLICT(name) DO UPDATE SET kind = excluded.kind, due_ms = excluded.due_ms, payload = excluded.payload, attempts = 0`,
      name,
      kind,
      Math.round(dueMs),
      JSON.stringify(payload ?? null),
    );
  }

  /** Sets the timer only when it does not exist or is due later than dueMs. */
  setEarliest(name: string, kind: string, dueMs: number, payload: unknown = null): void {
    const cur = this.get(name);
    if (!cur || cur.due_ms > dueMs) this.set(name, kind, dueMs, payload);
  }

  clear(name: string): boolean {
    return this.sql.exec("DELETE FROM timers WHERE name = ?", name).rowsWritten > 0;
  }

  /** Deletes every timer whose name starts with prefix. */
  clearPrefix(prefix: string): number {
    return this.sql.exec("DELETE FROM timers WHERE substr(name, 1, ?) = ?", prefix.length, prefix).rowsWritten;
  }

  get<P = unknown>(name: string): TimerRow<P> | undefined {
    const r = first<{ name: string; kind: string; due_ms: number; payload: string; attempts: number }>(
      this.sql,
      "SELECT name, kind, due_ms, payload, attempts FROM timers WHERE name = ?",
      name,
    );
    return r ? { ...r, payload: JSON.parse(r.payload) as P } : undefined;
  }

  nextDue(): number | null {
    return first<{ t: number | null }>(this.sql, "SELECT MIN(due_ms) AS t FROM timers")?.t ?? null;
  }

  due(limit = TIMER_BATCH): TimerRow[] {
    return this.sql
      .exec<{ name: string; kind: string; due_ms: number; payload: string; attempts: number }>(
        "SELECT name, kind, due_ms, payload, attempts FROM timers WHERE due_ms <= ? ORDER BY due_ms, name LIMIT ?",
        this.now(),
        limit,
      )
      .toArray()
      .map((r) => ({ ...r, payload: JSON.parse(r.payload) as unknown }));
  }

  /** Deletes the row if it is still the one that fired (same due_ms). */
  take(t: TimerRow): boolean {
    return this.sql.exec("DELETE FROM timers WHERE name = ? AND due_ms = ?", t.name, t.due_ms).rowsWritten > 0;
  }

  /** Records a failed run: attempts + 1 and a backoff, unless the timer moved meanwhile. */
  fail(t: TimerRow): void {
    const attempts = t.attempts + 1;
    this.sql.exec(
      "UPDATE timers SET attempts = ?, due_ms = ? WHERE name = ? AND due_ms = ?",
      attempts,
      this.now() + timerBackoff(attempts),
      t.name,
      t.due_ms,
    );
  }
}
