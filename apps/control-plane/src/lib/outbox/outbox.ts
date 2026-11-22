// The per-DO outbox (03 §6.1). Rows are written with enqueue() inside the
// same transactionSync as the state change, and delivered by the alarm
// (BaseDO) with backoff. Delivery is at least once; receivers dedupe on msgId.
//
// - Rows of one non-empty lane are delivered in id order (head of line).
//   A dead row does not block its lane.
// - Backoff: 1 s x 2^attempts, cap 5 min, jitter +-20 %; dead after 12 attempts.
// - A receiver that answers { retry: true } keeps the row pending without
//   counting an attempt.
// - A payload above 512 KiB is written to R2 by hash at delivery time and
//   sent as payloadR2.
import type { Ack, OutboxEnvelope, OutboxKind, OutboxPayloads } from "../../contracts/do-messages";
import { first } from "../sql/query";

export const OUTBOX_MAX_ATTEMPTS = 12;
export const OUTBOX_BASE_BACKOFF_MS = 1000;
export const OUTBOX_MAX_BACKOFF_MS = 5 * 60_000;
export const OUTBOX_LEASE_MS = 30_000;
export const OUTBOX_BATCH = 50;
/** Workers wait on at most 6 connections at once. */
export const OUTBOX_PARALLEL = 6;
export const OUTBOX_INLINE_LIMIT = 512 * 1024;

export interface OutboxRow {
  id: number;
  msg_id: string;
  dest: string;
  lane: string;
  kind: string;
  payload: string;
  state: "pending" | "dead";
  attempts: number;
  next_ms: number;
  last_error: string;
  created_ms: number;
}

export interface EnqueueInput<K extends string = string> {
  /** Where the row goes: see dest.ts ("do:<BINDING>:<name>", "queue:JOBS", or a custom scheme). */
  dest: string;
  kind: K;
  payload: K extends OutboxKind ? OutboxPayloads[K] : unknown;
  /** Idempotency key seen by the receiver. Default: a random ID. A second enqueue with the same msgId is ignored. */
  msgId?: string;
  /** "" (default) = no order. */
  lane?: string;
  /** Deliver no earlier than now + delayMs. */
  delayMs?: number;
}

export interface DrainResult {
  delivered: number;
  retried: number;
  dead: number;
}

export interface OutboxDeps {
  storage: DurableObjectStorage;
  /** Name of this DO, sent as envelope.origin. */
  origin: () => string;
  /** Network I/O to the receiver. Throw to count a failed attempt. */
  deliver: (envelope: OutboxEnvelope, row: OutboxRow) => Promise<Ack | void>;
  /** Called inside the transaction that marks a row dead. */
  onDead: (row: OutboxRow) => void;
  /** Writes a large payload to R2 and returns its key. */
  putLarge?: (body: string) => Promise<string>;
  now?: () => number;
  random?: () => number;
}

/** Backoff before attempt `attempts + 1` (attempts already failed >= 1). */
export function outboxBackoff(attempts: number, random: () => number = Math.random): number {
  const base = Math.min(OUTBOX_MAX_BACKOFF_MS, OUTBOX_BASE_BACKOFF_MS * 2 ** Math.max(0, attempts - 1));
  return Math.round(base * (0.8 + random() * 0.4));
}

export class Outbox {
  private readonly inflight = new Set<number>();
  private draining: Promise<DrainResult> | null = null;

  constructor(private readonly d: OutboxDeps) {}

  private get sql(): SqlStorage {
    return this.d.storage.sql;
  }

  private now(): number {
    return (this.d.now ?? Date.now)();
  }

  /** Writes one row. Call inside the transactionSync of the state change. Returns the msgId. */
  enqueue<K extends string>(input: EnqueueInput<K>): string {
    const msgId = input.msgId ?? `obx_${crypto.randomUUID()}`;
    const now = this.now();
    this.sql.exec(
      `INSERT OR IGNORE INTO outbox (msg_id, dest, lane, kind, payload, next_ms, created_ms) VALUES (?, ?, ?, ?, ?, ?, ?)`,
      msgId,
      input.dest,
      input.lane ?? "",
      input.kind,
      JSON.stringify(input.payload ?? null),
      now + Math.max(0, input.delayMs ?? 0),
      now,
    );
    return msgId;
  }

  /** Earliest next_ms of a pending row, or null. */
  nextDue(): number | null {
    const r = first<{ t: number | null }>(this.sql, "SELECT MIN(next_ms) AS t FROM outbox WHERE state = 'pending'");
    return r?.t ?? null;
  }

  pendingCount(): number {
    return Number(first(this.sql, "SELECT COUNT(*) AS n FROM outbox WHERE state = 'pending'")?.n ?? 0);
  }

  get(msgId: string): OutboxRow | undefined {
    return first<OutboxRow & Record<string, SqlStorageValue>>(this.sql, "SELECT * FROM outbox WHERE msg_id = ?", msgId);
  }

  listDead(limit = 100): OutboxRow[] {
    return this.sql
      .exec<OutboxRow & Record<string, SqlStorageValue>>(
        "SELECT * FROM outbox WHERE state = 'dead' ORDER BY id DESC LIMIT ?",
        limit,
      )
      .toArray();
  }

  /** Resets a dead row to pending. Returns false when no dead row has this msgId. */
  redrive(msgId: string): boolean {
    return (
      this.sql.exec(
        "UPDATE outbox SET state = 'pending', attempts = 0, next_ms = ?, last_error = '' WHERE msg_id = ? AND state = 'dead'",
        this.now(),
        msgId,
      ).rowsWritten > 0
    );
  }

  /** Deletes dead rows older than `beforeMs`. */
  purgeDead(beforeMs: number): number {
    return this.sql.exec("DELETE FROM outbox WHERE state = 'dead' AND created_ms < ?", beforeMs).rowsWritten;
  }

  /** Delivers due rows. Concurrent calls share one pass. Never throws. */
  drain(limit = OUTBOX_BATCH): Promise<DrainResult> {
    this.draining ??= this.drainOnce(limit).finally(() => {
      this.draining = null;
    });
    return this.draining;
  }

  private async drainOnce(limit: number): Promise<DrainResult> {
    const res: DrainResult = { delivered: 0, retried: 0, dead: 0 };
    const now = this.now();
    const rows = this.sql
      .exec<OutboxRow & Record<string, SqlStorageValue>>(
        `SELECT o.* FROM outbox o
         WHERE o.state = 'pending' AND o.next_ms <= ?
           AND (o.lane = '' OR o.id = (SELECT MIN(id) FROM outbox WHERE lane = o.lane AND state = 'pending'))
         ORDER BY o.id LIMIT ?`,
        now,
        limit,
      )
      .toArray()
      .filter((r) => !this.inflight.has(r.id));
    if (rows.length === 0) return res;
    // Lease before the first await, so an interleaved drain or a restart does not resend at once.
    for (const r of rows) {
      this.inflight.add(r.id);
      this.sql.exec("UPDATE outbox SET next_ms = ? WHERE id = ?", now + OUTBOX_LEASE_MS, r.id);
    }
    try {
      for (let i = 0; i < rows.length; i += OUTBOX_PARALLEL) {
        await Promise.all(rows.slice(i, i + OUTBOX_PARALLEL).map((r) => this.deliverOne(r, res)));
      }
    } finally {
      for (const r of rows) this.inflight.delete(r.id);
    }
    return res;
  }

  private async deliverOne(row: OutboxRow, res: DrainResult): Promise<void> {
    try {
      const envelope: OutboxEnvelope = {
        msgId: row.msg_id,
        kind: row.kind as OutboxKind,
        lane: row.lane,
        origin: this.d.origin(),
        payload: JSON.parse(row.payload) as OutboxEnvelope["payload"],
      };
      if (row.payload.length > OUTBOX_INLINE_LIMIT && this.d.putLarge) {
        envelope.payloadR2 = await this.d.putLarge(row.payload);
        envelope.payload = {} as OutboxEnvelope["payload"];
      }
      const ack = await this.d.deliver(envelope, row);
      if (ack && "retry" in ack && ack.retry) {
        this.sql.exec("UPDATE outbox SET next_ms = ? WHERE id = ?", this.now() + Math.max(0, ack.afterMs ?? 1000), row.id);
        res.retried++;
        return;
      }
      this.sql.exec("DELETE FROM outbox WHERE id = ?", row.id);
      res.delivered++;
    } catch (e) {
      const err = (e instanceof Error ? e.message : String(e)).slice(0, 2000);
      const attempts = row.attempts + 1;
      try {
        if (attempts >= OUTBOX_MAX_ATTEMPTS) {
          this.d.storage.transactionSync(() => {
            this.sql.exec(
              "UPDATE outbox SET state = 'dead', attempts = ?, last_error = ? WHERE id = ?",
              attempts,
              err,
              row.id,
            );
            this.d.onDead({ ...row, state: "dead", attempts, last_error: err });
          });
          res.dead++;
        } else {
          this.sql.exec(
            "UPDATE outbox SET attempts = ?, next_ms = ?, last_error = ? WHERE id = ?",
            attempts,
            this.now() + outboxBackoff(attempts, this.d.random),
            err,
            row.id,
          );
          res.retried++;
        }
      } catch (e2) {
        console.error("outbox: cannot record failure", row.msg_id, e2);
      }
    }
  }
}
