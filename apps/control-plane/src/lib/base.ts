// BaseDO: the base class of every control-plane Durable Object. It ties
// together migrations (src/lib/sql), the outbox (src/lib/outbox), timers
// (src/lib/timers) and the one alarm.
//
// Rules:
// - One this.tx(...) (transactionSync) per request or alarm step. No await inside.
// - Effects leave only through this.enqueue(...) inside that transaction.
// - Code that must wait for the commit (live pushes, an outbox kick) goes to
//   this.onCommit(...): it runs after the step commits and is dropped on rollback.
// - alarm() catches every error and always sets the next alarm.
import { DurableObject } from "cloudflare:workers";
import { OfError, toOfError } from "@openfactory/core";
import type { Ack, OutboxEnvelope, OutboxKind, OutboxPayloads, OutboxReceiver } from "../contracts/do-messages";
import type { Env } from "../contracts/env";
import { deliverStandard, dest, teamOfDoName } from "./outbox/dest";
import { Outbox, type DrainResult, type EnqueueInput, type OutboxRow } from "./outbox/outbox";
import { LIB_MIGRATIONS } from "./outbox/schema";
import { applyMigrations, type Migration } from "./sql/migrate";
import { first } from "./sql/query";
import { Timers, type AsyncTimerHandler, type SyncTimerHandler, type TimerRow } from "./timers/timers";

/** Handler of a received outbox message. Runs inside transactionSync with the dedupe row. */
export type MessageHandler<K extends OutboxKind = OutboxKind> = (
  payload: OutboxPayloads[K],
  msg: OutboxEnvelope<K>,
) => Ack | void;

const DEDUPE_TTL_MS = 7 * 24 * 3600_000;
const DEDUPE_PURGE_TIMER = "lib.dedupe_purge";
const DEAD_PURGE_TIMER = "lib.dead_purge";
const DEAD_TTL_MS = 30 * 24 * 3600_000;

class RetrySignal {
  constructor(readonly ack: Ack) {}
}

export abstract class BaseDO extends DurableObject<Env> implements OutboxReceiver {
  protected readonly sql: SqlStorage;
  protected readonly outbox: Outbox;
  protected readonly timers: Timers;
  /** Resolves when migrations are applied (the constructor blocks events until then). */
  protected readonly ready: Promise<void>;

  private readonly syncTimers = new Map<string, SyncTimerHandler>();
  private readonly asyncTimers = new Map<string, AsyncTimerHandler>();
  private readonly messageHandlers = new Map<string, MessageHandler>();
  /** Async timer handlers that run now in this isolate, by timer name. */
  private readonly runningTimers = new Map<string, Promise<void>>();
  private armedAt: number | null = null;
  /** Post-commit callbacks of the running step; null outside a step. */
  private commitCallbacks: (() => void)[] | null = null;

  constructor(ctx: DurableObjectState, env: Env) {
    super(ctx, env);
    this.sql = ctx.storage.sql;
    this.timers = new Timers(ctx.storage);
    this.outbox = new Outbox({
      storage: ctx.storage,
      origin: () => this.doName(),
      deliver: (envelope, row) => this.deliver(envelope, row),
      onDead: (row) => this.onDeadLetter(row),
      putLarge: async (body) => {
        const d = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(body));
        const key = "outbox/" + [...new Uint8Array(d)].map((x) => x.toString(16).padStart(2, "0")).join("");
        await env.DATA.put(key, body, { httpMetadata: { contentType: "application/json" } });
        return key;
      },
    });
    this.onTimer(DEDUPE_PURGE_TIMER, () => {
      this.sql.exec("DELETE FROM inbox_dedupe WHERE received_ms < ?", Date.now() - DEDUPE_TTL_MS);
      if (first(this.sql, "SELECT 1 FROM inbox_dedupe LIMIT 1")) {
        this.timers.set(DEDUPE_PURGE_TIMER, DEDUPE_PURGE_TIMER, Date.now() + 24 * 3600_000);
      }
    });
    this.onTimer(DEAD_PURGE_TIMER, () => {
      this.outbox.purgeDead(Date.now() - DEAD_TTL_MS);
      if (first(this.sql, "SELECT 1 FROM outbox WHERE state = 'dead' LIMIT 1")) {
        this.timers.set(DEAD_PURGE_TIMER, DEAD_PURGE_TIMER, Date.now() + 24 * 3600_000);
      }
    });
    this.ready = ctx.blockConcurrencyWhile(async () => {
      await applyMigrations(ctx.storage, "lib", LIB_MIGRATIONS);
      await applyMigrations(ctx.storage, "do", this.migrations());
      const name = (ctx.id as { name?: string }).name;
      if (name) this.sql.exec("INSERT OR IGNORE INTO meta (k, v) VALUES ('do_name', ?)", name);
      this.armedAt = await ctx.storage.getAlarm();
      this.init();
      this.armAlarm();
    });
  }

  /** The DO's own migrations, ordered. A prototype method: subclass fields are not set yet. */
  protected abstract migrations(): readonly Migration[];

  /** Runs once after migrations, before the first event. Register handlers in the constructor instead. */
  protected init(): void {}

  /** The DO name ("team:default", "team:default|index", ...). */
  doName(): string {
    const name = (this.ctx.id as { name?: string }).name;
    if (name) return name;
    const row = first<{ v: string }>(this.sql, "SELECT v FROM meta WHERE k = 'do_name'");
    return row?.v ?? this.ctx.id.toString();
  }

  /** Team ID from the DO name. */
  teamId(): string {
    return teamOfDoName(this.doName()) || "default";
  }

  // ---------- hibernated WebSockets ----------

  /** Live subscribers never send; a DO with a client protocol overrides it. */
  override async webSocketMessage(_ws: WebSocket, _message: string | ArrayBuffer): Promise<void> {}

  /** Answers the close of a subscriber (code 1005 "no status" becomes 1000). */
  override async webSocketClose(ws: WebSocket, code: number, _reason?: string, _wasClean?: boolean): Promise<void> {
    try {
      ws.close(code === 1005 ? 1000 : code, "bye");
    } catch {
      /* already closed */
    }
  }

  // ---------- transactions ----------

  /** One transactionSync, then re-arms the alarm when timers or outbox changed. */
  protected tx<T>(fn: () => T): T {
    return this.step(fn, true);
  }

  /**
   * Runs fn in transactionSync. Callbacks that fn registers with onCommit run
   * after the commit (after the alarm is re-armed when arm is true), and are
   * dropped when fn throws. A step inside a step is a savepoint: its callbacks
   * join the outer step, and a throw drops only its own.
   */
  private step<T>(fn: () => T, arm: boolean): T {
    const outer = this.commitCallbacks;
    if (outer) {
      const mark = outer.length;
      try {
        return this.ctx.storage.transactionSync(fn);
      } catch (e) {
        outer.length = mark;
        throw e;
      }
    }
    const callbacks: (() => void)[] = [];
    this.commitCallbacks = callbacks;
    let out: T;
    try {
      out = this.ctx.storage.transactionSync(fn);
    } finally {
      this.commitCallbacks = null;
    }
    if (arm) this.armAlarm();
    for (const cb of callbacks) {
      try {
        cb();
      } catch (e) {
        console.error(`${this.constructor.name}: post-commit callback failed`, e);
      }
    }
    return out;
  }

  /**
   * Runs fn after the current step (this.tx, a received message, a sync timer)
   * commits; drops it when the step rolls back. Outside a step fn runs in a
   * microtask. fn must not throw: an error is logged and ignored.
   */
  protected onCommit(fn: () => void): void {
    if (this.commitCallbacks) {
      this.commitCallbacks.push(fn);
      return;
    }
    queueMicrotask(() => {
      try {
        fn();
      } catch (e) {
        console.error(`${this.constructor.name}: post-commit callback failed`, e);
      }
    });
  }

  /** Writes an outbox row. Call inside this.tx() with the state change. */
  protected enqueue<K extends string>(input: EnqueueInput<K>): string {
    return this.outbox.enqueue(input);
  }

  protected setTimer(name: string, kind: string, dueMs: number, payload?: unknown): void {
    this.timers.set(name, kind, dueMs, payload);
  }

  protected clearTimer(name: string): boolean {
    return this.timers.clear(name);
  }

  // ---------- handler registration (call in the subclass constructor) ----------

  /** A timer kind handled inside transactionSync. */
  protected onTimer<P = unknown>(kind: string, handler: SyncTimerHandler<P>): void {
    this.syncTimers.set(kind, handler as SyncTimerHandler);
  }

  /** A timer kind handled outside a transaction (network I/O). Reschedule from the handler to repeat. */
  protected onTimerAsync<P = unknown>(kind: string, handler: AsyncTimerHandler<P>): void {
    this.asyncTimers.set(kind, handler as AsyncTimerHandler);
  }

  /** An outbox kind this DO receives. */
  protected onMessage<K extends OutboxKind>(kind: K, handler: MessageHandler<K>): void {
    this.messageHandlers.set(kind, handler as unknown as MessageHandler);
  }

  // ---------- receiving ----------

  /** OutboxReceiver: dedupes on msgId and runs the handler in one transaction. */
  async receive(msg: OutboxEnvelope): Promise<Ack> {
    const handler = this.messageHandlers.get(msg.kind);
    if (!handler) throw new OfError("operation_not_supported", `${this.constructor.name} does not receive ${msg.kind}`);
    let payload = msg.payload;
    if (msg.payloadR2) {
      const obj = await this.env.DATA.get(msg.payloadR2);
      if (!obj) throw new OfError("resource_unavailable", `outbox payload ${msg.payloadR2} is missing`);
      payload = (await obj.json()) as typeof payload;
    }
    const full = { ...msg, payload };
    try {
      return this.tx(() => {
        if (this.seen(msg.msgId)) return { ok: true } as const;
        const ack = handler(payload, full);
        if (ack && "retry" in ack) throw new RetrySignal(ack);
        this.markSeen(msg.msgId);
        return { ok: true } as const;
      });
    } catch (e) {
      if (e instanceof RetrySignal) return e.ack;
      throw e;
    }
  }

  /** Receiver-side dedupe check. Use inside a transaction. */
  protected seen(msgId: string): boolean {
    return first(this.sql, "SELECT 1 FROM inbox_dedupe WHERE msg_id = ?", msgId) !== undefined;
  }

  /** Records a processed msgId. Use inside the transaction of the effect. */
  protected markSeen(msgId: string): void {
    this.sql.exec("INSERT OR IGNORE INTO inbox_dedupe (msg_id, received_ms) VALUES (?, ?)", msgId, Date.now());
    this.timers.setEarliest(DEDUPE_PURGE_TIMER, DEDUPE_PURGE_TIMER, Date.now() + 24 * 3600_000);
  }

  // ---------- delivering ----------

  /** Network delivery of one row. Override deliverCustom for custom destinations. */
  protected async deliver(envelope: OutboxEnvelope, row: OutboxRow): Promise<Ack | void> {
    const ack = await deliverStandard(this.env, row.dest, envelope);
    if (ack !== undefined) return ack;
    return this.deliverCustom(envelope, row);
  }

  protected async deliverCustom(_envelope: OutboxEnvelope, row: OutboxRow): Promise<Ack | void> {
    throw new Error(`no delivery for destination ${row.dest}`);
  }

  /**
   * Called inside the transaction that marks a row dead. Default: project it to
   * the team's IndexDO (index.deadLetter). IndexDO overrides this.
   */
  protected onDeadLetter(row: OutboxRow): void {
    this.timers.setEarliest(DEAD_PURGE_TIMER, DEAD_PURGE_TIMER, Date.now() + 24 * 3600_000);
    if (row.kind === "index.deadLetter") return;
    this.outbox.enqueue({
      dest: dest.index(this.teamId()),
      kind: "index.deadLetter",
      msgId: `dl:${this.doName()}:${row.msg_id}:${row.attempts}`,
      payload: {
        originDO: this.doName(),
        msgId: row.msg_id,
        kind: row.kind,
        lane: row.lane,
        error: row.last_error,
        attempts: row.attempts,
      },
    });
  }

  /** Resets a dead outbox row (POST /outbox/{id}/redrive). */
  async outboxRedrive(msgId: string): Promise<boolean> {
    const ok = this.tx(() => this.outbox.redrive(msgId));
    return ok;
  }

  /** Delivers due outbox rows now, outside the alarm. */
  protected drainNow(): Promise<DrainResult> {
    return this.outbox.drain().finally(() => this.armAlarm());
  }

  // ---------- alarm ----------

  /** Cron safety net: make sure the alarm is set when work is pending. */
  async ensureAlarm(): Promise<void> {
    this.armedAt = await this.ctx.storage.getAlarm();
    this.armAlarm();
  }

  override async alarm(info?: AlarmInvocationInfo): Promise<void> {
    this.armedAt = null;
    try {
      await this.runTimers();
      await this.outbox.drain();
    } catch (e) {
      console.error(`${this.constructor.name}: alarm failed`, info?.retryCount ?? 0, e);
    } finally {
      try {
        this.armAlarm(true);
      } catch (e) {
        // Storage is gone (shutdown); the platform retries the alarm and the Cron re-arms it.
        console.error(`${this.constructor.name}: cannot re-arm alarm`, e);
      }
    }
  }

  private async runTimers(): Promise<void> {
    for (const t of this.timers.due()) {
      const sync = this.syncTimers.get(t.kind);
      const async_ = this.asyncTimers.get(t.kind);
      try {
        if (sync) {
          this.step(() => {
            if (this.timers.take(t)) sync(t);
          }, false);
        } else if (async_) {
          // A second alarm() (pump(), waitUntil(this.alarm())) can run while the
          // first awaits this handler. Wait for the running handler instead of
          // running it twice: it owns the timer and records its own failure.
          const running = this.runningTimers.get(t.name);
          if (running) {
            await running.catch(() => undefined);
            continue;
          }
          const run = async_(t);
          this.runningTimers.set(t.name, run);
          try {
            await run;
            // Done unless the handler moved the timer.
            this.timers.take(t);
          } finally {
            if (this.runningTimers.get(t.name) === run) this.runningTimers.delete(t.name);
          }
        } else {
          console.error(`${this.constructor.name}: no handler for timer kind ${t.kind}; dropping ${t.name}`);
          this.timers.take(t);
        }
      } catch (e) {
        console.error(`${this.constructor.name}: timer ${t.name} failed`, e);
        try {
          this.timers.fail(t);
        } catch {
          /* storage gone */
        }
      }
    }
  }

  /** Sets the alarm to the earliest pending timer or outbox row when that is earlier than the armed one. */
  protected armAlarm(force = false): void {
    const a = this.timers.nextDue();
    const b = this.outbox.nextDue();
    const min = a === null ? b : b === null ? a : Math.min(a, b);
    if (min === null) return;
    // A due time in the past (or 0) fires as soon as possible; setAlarm refuses times <= 0.
    const next = Math.max(min, 1);
    if (force || this.armedAt === null || next < this.armedAt) {
      this.armedAt = next;
      this.ctx.storage.setAlarm(next).catch((e: unknown) => {
        // Forget the failed time, or a stale armedAt blocks the next armAlarm.
        if (this.armedAt === next) this.armedAt = null;
        console.error(`${this.constructor.name}: cannot set alarm`, e);
      });
    }
  }

  // ---------- errors over RPC ----------

  /**
   * Wraps an RPC body so that an OfError keeps its code across the RPC
   * boundary (the message carries "<code>: <detail>", see toOfError).
   */
  protected async rpc<T>(fn: () => T | Promise<T>): Promise<T> {
    try {
      return await fn();
    } catch (e) {
      const { error, known } = toOfError(e);
      if (!known) console.error(`${this.constructor.name}: internal error`, e);
      throw error;
    }
  }
}

export type { Migration, TimerRow };
