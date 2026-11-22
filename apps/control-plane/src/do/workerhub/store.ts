// SQLite state of one WorkerHubDO: the lease cache (hub_tasks) and the hub
// key/value state (hub_state). The reads are synchronous; call the methods
// that decide a write inside the DO transaction.
import type { SandboxTask, TaskStatus } from "@openfactory/sandbox";
import { all, first } from "../../lib/sql/query";
import type { WorkerRecord } from "../team/workers";

export interface TaskRow extends Record<string, SqlStorageValue> {
  task_id: string;
  run_id: string;
  session: number;
  /** JSON TaskStatus. */
  status: string;
  /** 1 = destroy asked while the worker was away; reconcile() sends it. */
  destroyed: number;
  leased_ms: number;
  updated_ms: number;
}

/** Hello facts kept by the hub for live() and the TeamDO push. */
export interface HelloSummary {
  version: string;
  capacity: number;
  labels: Record<string, string>;
  facts: Record<string, unknown> | null;
  features: string[];
  slots: number;
}

/** What attach() found in the hello's task list. */
export interface AttachResult {
  /** Tasks that exited while the worker was away (status changed to exited). */
  exited: TaskStatus[];
  /** Held task IDs that were never leased to this worker. */
  ignored: string[];
}

export function statusOf(r: TaskRow): TaskStatus {
  return JSON.parse(r.status) as TaskStatus;
}

export class HubStore {
  constructor(private readonly sql: SqlStorage) {}

  // ---------- hub_state ----------

  get(k: string): string | undefined {
    return first<{ v: string }>(this.sql, "SELECT v FROM hub_state WHERE k = ?", k)?.v;
  }

  set(k: string, v: string): void {
    this.sql.exec("INSERT INTO hub_state (k, v) VALUES (?, ?) ON CONFLICT (k) DO UPDATE SET v = excluded.v", k, v);
  }

  del(k: string): void {
    this.sql.exec("DELETE FROM hub_state WHERE k = ?", k);
  }

  record(): WorkerRecord | undefined {
    const v = this.get("record");
    return v ? (JSON.parse(v) as WorkerRecord) : undefined;
  }

  setRecord(rec: WorkerRecord): void {
    this.set("record", JSON.stringify(rec));
  }

  hello(): HelloSummary | undefined {
    const v = this.get("hello");
    return v ? (JSON.parse(v) as HelloSummary) : undefined;
  }

  setHello(h: HelloSummary): void {
    this.set("hello", JSON.stringify(h));
  }

  revoked(): boolean {
    return this.get("revoked") === "1" || this.record()?.revoked === true;
  }

  /** Increments and returns the capacity.report generation. */
  nextGeneration(): number {
    const gen = Number(this.get("generation") ?? "0") + 1;
    this.set("generation", String(gen));
    return gen;
  }

  /** The slot count of the record, else the capacity of the last hello. */
  slotLimit(): number {
    const rec = this.record();
    if (rec && rec.slots > 0) return rec.slots;
    return this.hello()?.capacity ?? 0;
  }

  // ---------- hub_tasks (lease cache) ----------

  tasks(): TaskRow[] {
    return all<TaskRow>(this.sql, "SELECT * FROM hub_tasks ORDER BY leased_ms, task_id");
  }

  task(taskId: string): TaskRow | undefined {
    return first<TaskRow>(this.sql, "SELECT * FROM hub_tasks WHERE task_id = ?", taskId);
  }

  /** Statuses of the leased tasks that are not destroyed, in lease order. */
  liveStatuses(): TaskStatus[] {
    return all<{ status: string }>(
      this.sql,
      "SELECT status FROM hub_tasks WHERE destroyed = 0 ORDER BY leased_ms, task_id",
    ).map((r) => JSON.parse(r.status) as TaskStatus);
  }

  /** IDs of the tasks whose destroy is still owed to the worker, in lease order. */
  destroyedIds(): string[] {
    return all<{ task_id: string }>(
      this.sql,
      "SELECT task_id FROM hub_tasks WHERE destroyed = 1 ORDER BY leased_ms, task_id",
    ).map((r) => r.task_id);
  }

  /** Leased tasks that hold a slot: not exited and not destroyed. One row comes back. */
  slotsUsed(): number {
    const r = first<{ n: number }>(
      this.sql,
      "SELECT COUNT(*) AS n FROM hub_tasks WHERE destroyed = 0 AND IFNULL(json_extract(status, '$.state'), '') <> 'exited'",
    );
    return r?.n ?? 0;
  }

  /** Reserves a slot: the task row in state accepted. */
  lease(task: SandboxTask, s: TaskStatus, now: number): void {
    this.sql.exec(
      "INSERT OR REPLACE INTO hub_tasks (task_id, run_id, session, status, destroyed, leased_ms, updated_ms) VALUES (?, ?, ?, ?, 0, ?, ?)",
      task.taskId,
      task.runId,
      task.session,
      JSON.stringify(s),
      now,
      now,
    );
  }

  setStatus(taskId: string, s: TaskStatus, now: number): void {
    this.sql.exec("UPDATE hub_tasks SET status = ?, updated_ms = ? WHERE task_id = ?", JSON.stringify(s), now, taskId);
  }

  /** Deletes the row; returns true when there was one. */
  remove(taskId: string): boolean {
    return this.sql.exec("DELETE FROM hub_tasks WHERE task_id = ?", taskId).rowsWritten > 0;
  }

  /** Deletes the row only while its destroy is still owed (reconcile() got the answer). */
  removeDestroyed(taskId: string): void {
    this.sql.exec("DELETE FROM hub_tasks WHERE task_id = ? AND destroyed = 1", taskId);
  }

  /** Keeps the destroy for the next hello; returns true when the row exists. */
  markDestroyed(taskId: string, now: number): boolean {
    return (
      this.sql.exec("UPDATE hub_tasks SET destroyed = 1, updated_ms = ? WHERE task_id = ?", now, taskId).rowsWritten > 0
    );
  }

  /**
   * The hello's task list replaces the cache. A leased task the worker does
   * not hold is gone; a held task that was never leased is ignored; a row
   * with a destroy still owed keeps its status. Reads the cache once.
   */
  attach(held: ReadonlyMap<string, TaskStatus>, now: number): AttachResult {
    const exited: TaskStatus[] = [];
    const leased = new Set<string>();
    for (const r of this.tasks()) {
      leased.add(r.task_id);
      const s = held.get(r.task_id);
      if (!s) {
        this.remove(r.task_id);
        continue;
      }
      if (r.destroyed === 1) continue;
      const old = statusOf(r);
      this.setStatus(r.task_id, s, now);
      if (s.state === "exited" && old.state !== "exited") exited.push(s);
    }
    const ignored = [...held.keys()].filter((id) => !leased.has(id));
    return { exited, ignored };
  }
}
