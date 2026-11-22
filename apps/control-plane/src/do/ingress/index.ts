// IngressDO (key team:<t>|ingress:<partition>): inbound events of one
// partition, deduplicated and delivered in order with backoff, dead letter
// and redrive (03 §7.1, §11.6). One event at a time: a pending head blocks
// the partition; a dead event does not.
//
// The intake step (HandleEvent: automations, routing, continuations) is a
// pluggable handler. It must be idempotent per (actionKey, target): the WorkDO
// ledger deduplicates, so a retried event creates no second run.
import { OfError } from "@openfactory/core";
import type { CoreEvent } from "../../contracts/do-messages";
import type { Env } from "../../contracts/env";
import { BaseDO, type Migration } from "../../lib/base";
import { outboxBackoff, type EnqueueInput } from "../../lib/outbox/outbox";
import { all, first } from "../../lib/sql/query";

export const INGRESS_MAX_ATTEMPTS = 12;
const EVENT_TTL_MS = 7 * 24 * 3_600_000;

/** Result of an intake step: "done" (served or dropped) or a transient failure. */
export type IntakeResult = { done: true } | { retry: true; error: string };

export type IntakeHandler = (env: Env, ctx: { teamId: string; partition: string }, event: CoreEvent) => Promise<IntakeResult>;

let intake: IntakeHandler = async () => ({ done: true });

/** The integrations workstream sets the HandleEvent port here. */
export function setIntakeHandler(fn: IntakeHandler): void {
  intake = fn;
}

const m0001: Migration = {
  version: 1,
  name: "init",
  sql: `
CREATE TABLE events (seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT NOT NULL UNIQUE, action_key TEXT NOT NULL DEFAULT '',
  received_ms INTEGER NOT NULL, payload TEXT, payload_r2 TEXT, state TEXT NOT NULL DEFAULT 'pending',
  attempts INTEGER NOT NULL DEFAULT 0, next_ms INTEGER NOT NULL, last_error TEXT NOT NULL DEFAULT '');
CREATE INDEX events_head ON events (state, seq);
CREATE TABLE action_targets (action_key TEXT NOT NULL, target TEXT NOT NULL, ref TEXT NOT NULL DEFAULT '', created_ms INTEGER NOT NULL,
  PRIMARY KEY (action_key, target));
`,
};

interface EventRow {
  seq: number;
  id: string;
  payload: string;
  attempts: number;
  next_ms: number;
}

export class IngressDO extends BaseDO {
  constructor(ctx: DurableObjectState, env: Env) {
    super(ctx, env);
    this.onMessage("ingress.deliver", (p) => {
      this.insert(p.event, p.dedupeKey);
    });
    this.onTimerAsync("deliver", () => this.deliverHead());
    this.onTimer("purge", () => {
      this.sql.exec("DELETE FROM events WHERE state = 'done' AND received_ms < ?", Date.now() - EVENT_TTL_MS);
      if (first(this.sql, "SELECT 1 FROM events WHERE state = 'done' LIMIT 1")) this.setTimer("purge", "purge", Date.now() + 86_400_000);
    });
  }

  protected migrations(): readonly Migration[] {
    return [m0001];
  }

  async ping(): Promise<string> {
    return "IngressDO";
  }

  private partition(): string {
    const name = this.doName();
    const i = name.indexOf("|ingress:");
    return i >= 0 ? name.slice(i + 9) : "";
  }

  /** Inserts an event unless its dedupe key is known. Runs in the caller's transaction. */
  private insert(event: CoreEvent, dedupeKey: string): { accepted: boolean; deduplicated: boolean } {
    const key = dedupeKey || event.id;
    if (!key) throw new OfError("invalid_request", "event has no id or dedupe key");
    if (first(this.sql, "SELECT 1 FROM events WHERE id = ?", key)) return { accepted: false, deduplicated: true };
    const now = Date.now();
    this.sql.exec(
      "INSERT INTO events (id, action_key, received_ms, payload, next_ms) VALUES (?, ?, ?, ?, ?)",
      key,
      event.action_key ?? "",
      now,
      JSON.stringify(event),
      now,
    );
    this.timers.setEarliest("deliver", "deliver", now);
    this.timers.setEarliest("purge", "purge", now + 86_400_000);
    return { accepted: true, deduplicated: false };
  }

  // IngressRpc.enqueue (frozen contract) shares its name with BaseDO.enqueue
  // (outbox). The overloads keep both: an object argument is an outbox row.
  override enqueue<K extends string>(input: EnqueueInput<K>): string;
  override enqueue(partition: string, event: CoreEvent, dedupeKey: string): Promise<{ accepted: boolean; deduplicated: boolean }>;
  override enqueue<K extends string>(
    a: EnqueueInput<K> | string,
    event?: CoreEvent,
    dedupeKey?: string,
  ): string | Promise<{ accepted: boolean; deduplicated: boolean }> {
    if (typeof a !== "string") return this.outbox.enqueue(a);
    return this.rpc(() => this.tx(() => this.insert(event!, dedupeKey ?? ""))).then((res) => {
      this.ctx.waitUntil(this.alarm());
      return res;
    });
  }

  /** Delivers pending events in order until one fails or none is due. */
  private async deliverHead(): Promise<void> {
    for (let i = 0; i < 25; i++) {
      const head = first<EventRow & Record<string, SqlStorageValue>>(
        this.sql,
        "SELECT seq, id, payload, attempts, next_ms FROM events WHERE state = 'pending' ORDER BY seq LIMIT 1",
      );
      if (!head) return;
      if (head.next_ms > Date.now()) {
        this.setTimer("deliver", "deliver", head.next_ms);
        return;
      }
      let res: IntakeResult;
      try {
        res = await intake(this.env, { teamId: this.teamId(), partition: this.partition() }, JSON.parse(head.payload) as CoreEvent);
      } catch (e) {
        const permanent = e instanceof OfError && ["invalid_request", "resource_not_found", "not_authorized", "conflict"].includes(e.code);
        res = permanent ? { done: true } : { retry: true, error: e instanceof Error ? e.message : String(e) };
        if (permanent) console.warn("ingress: event dropped", head.id, (e as Error).message);
      }
      if ("done" in res) {
        this.tx(() => this.sql.exec("UPDATE events SET state = 'done', last_error = '' WHERE seq = ?", head.seq));
        continue;
      }
      const attempts = head.attempts + 1;
      if (attempts >= INGRESS_MAX_ATTEMPTS) {
        this.tx(() => {
          this.sql.exec("UPDATE events SET state = 'dead', attempts = ?, last_error = ? WHERE seq = ?", attempts, res.error, head.seq);
          this.enqueue_dead(head.id, attempts, res.error);
        });
        continue;
      }
      const nextMs = Date.now() + outboxBackoff(attempts);
      this.tx(() => {
        this.sql.exec("UPDATE events SET attempts = ?, next_ms = ?, last_error = ? WHERE seq = ?", attempts, nextMs, res.error, head.seq);
        this.setTimer("deliver", "deliver", nextMs);
      });
      return;
    }
    this.setTimer("deliver", "deliver", Date.now());
  }

  private enqueue_dead(id: string, attempts: number, error: string): void {
    this.outbox.enqueue({
      dest: `do:INDEX:team:${this.teamId()}|index`,
      kind: "index.deadLetter",
      msgId: `dl:${this.doName()}:${id}:${attempts}`,
      payload: { originDO: this.doName(), msgId: id, kind: "ingress.event", lane: this.partition(), error, attempts },
    });
  }

  /** Redrive a dead event (or a dead outbox row). */
  async redrive(msgId: string): Promise<boolean> {
    return this.rpc(() =>
      this.tx(() => {
        const n = this.sql.exec(
          "UPDATE events SET state = 'pending', attempts = 0, next_ms = ?, last_error = '' WHERE id = ? AND state = 'dead'",
          Date.now(),
          msgId,
        ).rowsWritten;
        if (n > 0) {
          this.timers.setEarliest("deliver", "deliver", Date.now());
          return true;
        }
        return this.outbox.redrive(msgId);
      }),
    );
  }

  async deliveries(_meta: unknown, op: string, _params: Record<string, string>): Promise<unknown> {
    return this.rpc(() => {
      if (op !== "listEvents") throw new OfError("feature_not_available", `IngressDO does not serve ${op}`);
      return {
        events: all(this.sql, "SELECT seq, id, action_key, received_ms, state, attempts, last_error FROM events ORDER BY seq DESC LIMIT 200"),
      };
    });
  }

  async pump(): Promise<void> {
    await this.alarm();
  }
}
