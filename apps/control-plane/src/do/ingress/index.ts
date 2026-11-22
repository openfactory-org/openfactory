// IngressDO (key team:<t>|ingress:<partition>): inbound events of one
// partition, deduplicated and delivered in order with backoff, dead letter
// and redrive. One event at a time: a pending head blocks
// the partition; a dead event does not.
//
// The intake step (HandleEvent: automations, routing, continuations) is a
// pluggable handler. It must be idempotent per (actionKey, target): the WorkDO
// ledger deduplicates, so a retried event creates no second run.
import { OfError, type WebhookDelivery } from "@openfactory/core";
import type { CoreEvent } from "../../contracts/do-messages";
import type { Env } from "../../contracts/env";
import { BaseDO, type Migration } from "../../lib/base";
import { outboxBackoff } from "../../lib/outbox/outbox";
import { all, first } from "../../lib/sql/query";
import { m0003 } from "./migrations/0003_drop_unused";

export const INGRESS_MAX_ATTEMPTS = 12;
const EVENT_TTL_MS = 7 * 24 * 3_600_000;

/** Result of an intake step: "done" (served or dropped) or a transient failure. */
export type IntakeResult = { done: true } | { retry: true; error: string };

export type IntakeHandler = (
  env: Env,
  ctx: { teamId: string; partition: string },
  event: CoreEvent,
) => Promise<IntakeResult>;

let intake: IntakeHandler | undefined;

/**
 * Sets the HandleEvent port (bootstrapControlPlane in src/wiring.ts; tests
 * replace it). Returns the handler it replaces, so a test can restore it.
 */
export function setIntakeHandler(fn: IntakeHandler | undefined): IntakeHandler | undefined {
  const prev = intake;
  intake = fn;
  return prev;
}

/**
 * The HandleEvent port. Without a registration it throws: the event stays
 * pending, retries with backoff and goes to the dead letter list, from where
 * an operator can redrive it. It is never dropped in silence.
 */
export function intakeHandler(): IntakeHandler {
  if (!intake) {
    console.error("ingress: no intake handler is registered: call bootstrapControlPlane()");
    throw new OfError("internal_error", "no intake handler is registered");
  }
  return intake;
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

// The delivery log of a webhook partition and the shared accepted-delivery
// limit (webhooks.go ListWebhookDeliveries / GetWebhookDelivery).
const m0002: Migration = {
  version: 2,
  name: "deliveries",
  sql: `
CREATE TABLE deliveries (id TEXT PRIMARY KEY, webhook_id TEXT NOT NULL, status TEXT NOT NULL, received_ms INTEGER NOT NULL, data TEXT NOT NULL);
CREATE INDEX deliveries_received ON deliveries (webhook_id, received_ms);
CREATE TABLE rate_windows (key TEXT PRIMARY KEY, start_ms INTEGER NOT NULL, count INTEGER NOT NULL);
`,
};

/** Accepted deliveries per webhook per minute (DEFAULT_RATE_LIMIT of webhooks). */
export const WEBHOOK_RATE_LIMIT = 60;
const RATE_WINDOW_MS = 60_000;
/** Delivery log entries kept per webhook. */
export const DELIVERY_LOG_KEEP = 500;

/** DeliveryView (webhooks.go): a delivery with a label and the size of its body. */
export function deliveryView(d: WebhookDelivery, withBody: boolean): Record<string, unknown> {
  const size =
    d.body === undefined || d.body === null
      ? 0
      : new TextEncoder().encode(typeof d.body === "string" ? d.body : JSON.stringify(d.body)).length;
  let label: string = d.status;
  const reason = d.reason ?? "";
  switch (label) {
    case "accepted":
      label = "Accepted";
      break;
    case "deduplicated":
    case "duplicate":
      label = "Duplicate";
      break;
    case "rejected":
      if (reason === "rate_limited") label = "Rejected (rate limited)";
      else if (reason === "secret_unavailable") label = "Rejected (secret unavailable)";
      else if (reason.includes("json")) label = "Rejected (invalid JSON)";
      else if (reason.includes("large") || reason.includes("size")) label = "Rejected (too large)";
      else label = "Rejected (auth)";
  }
  const { body, ...rest } = d;
  return { ...rest, ...(withBody && body !== undefined ? { body } : {}), label, size };
}

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
      if (first(this.sql, "SELECT 1 FROM events WHERE state = 'done' LIMIT 1"))
        this.setTimer("purge", "purge", Date.now() + 86_400_000);
    });
  }

  protected migrations(): readonly Migration[] {
    return [m0001, m0002, m0003];
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

  /** Stores one event for ordered delivery (IngressRpc.enqueueEvent). */
  async enqueueEvent(
    _partition: string,
    event: CoreEvent,
    dedupeKey: string,
  ): Promise<{ accepted: boolean; deduplicated: boolean }> {
    const res = await this.rpc(() => this.tx(() => this.insert(event, dedupeKey)));
    this.ctx.waitUntil(this.alarm());
    return res;
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
        this.tx(() => this.setTimer("deliver", "deliver", head.next_ms));
        return;
      }
      let res: IntakeResult;
      try {
        res = await intakeHandler()(
          this.env,
          { teamId: this.teamId(), partition: this.partition() },
          JSON.parse(head.payload) as CoreEvent,
        );
      } catch (e) {
        const permanent =
          e instanceof OfError &&
          ["invalid_request", "resource_not_found", "not_authorized", "conflict"].includes(e.code);
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
          this.sql.exec(
            "UPDATE events SET state = 'dead', attempts = ?, last_error = ? WHERE seq = ?",
            attempts,
            res.error,
            head.seq,
          );
          this.enqueue_dead(head.id, attempts, res.error);
        });
        continue;
      }
      const nextMs = Date.now() + outboxBackoff(attempts);
      this.tx(() => {
        this.sql.exec(
          "UPDATE events SET attempts = ?, next_ms = ?, last_error = ? WHERE seq = ?",
          attempts,
          nextMs,
          res.error,
          head.seq,
        );
        this.setTimer("deliver", "deliver", nextMs);
      });
      return;
    }
    // More events wait: continue in the next alarm. The new timer row has a new
    // seq, so BaseDO does not delete it as the timer that fired.
    this.tx(() => this.setTimer("deliver", "deliver", Date.now()));
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

  /** Writes one delivery log entry; keeps the newest DELIVERY_LOG_KEEP of the webhook. */
  async recordDelivery(rec: WebhookDelivery): Promise<void> {
    return this.rpc(() =>
      this.tx(() => {
        const ms = Date.parse(rec.received_at) || Date.now();
        this.sql.exec(
          "INSERT INTO deliveries (id, webhook_id, status, received_ms, data) VALUES (?, ?, ?, ?, ?) ON CONFLICT(id) DO UPDATE SET status = excluded.status, data = excluded.data",
          rec.id,
          rec.webhook_id,
          rec.status,
          ms,
          JSON.stringify(rec),
        );
        this.sql.exec(
          "DELETE FROM deliveries WHERE webhook_id = ? AND id NOT IN (SELECT id FROM deliveries WHERE webhook_id = ? ORDER BY received_ms DESC, rowid DESC LIMIT ?)",
          rec.webhook_id,
          rec.webhook_id,
          DELIVERY_LOG_KEEP,
        );
      }),
    );
  }

  /** The shared accepted-delivery limit: a fixed one-minute window per webhook. */
  async rateLimit(
    webhookId: string,
    nowMs: number,
    limit = WEBHOOK_RATE_LIMIT,
  ): Promise<{ ok: boolean; retryMs: number }> {
    return this.rpc(() =>
      this.tx(() => {
        const w = first<{ start_ms: number; count: number }>(
          this.sql,
          "SELECT start_ms, count FROM rate_windows WHERE key = ?",
          webhookId,
        );
        let start = w?.start_ms ?? nowMs;
        let count = w?.count ?? 0;
        if (!w || nowMs - start >= RATE_WINDOW_MS || nowMs < start) {
          start = nowMs;
          count = 0;
        }
        if (count >= limit) return { ok: false, retryMs: Math.max(1, start + RATE_WINDOW_MS - nowMs) };
        this.sql.exec(
          "INSERT INTO rate_windows (key, start_ms, count) VALUES (?, ?, ?) ON CONFLICT(key) DO UPDATE SET start_ms = excluded.start_ms, count = excluded.count",
          webhookId,
          start,
          count + 1,
        );
        return { ok: true, retryMs: 0 };
      }),
    );
  }

  private listDeliveryRows(webhookId: string, limit: number): WebhookDelivery[] {
    const rows = webhookId
      ? all<{ data: string }>(
          this.sql,
          "SELECT data FROM deliveries WHERE webhook_id = ? ORDER BY received_ms DESC, rowid DESC LIMIT ?",
          webhookId,
          limit,
        )
      : all<{ data: string }>(
          this.sql,
          "SELECT data FROM deliveries ORDER BY received_ms DESC, rowid DESC LIMIT ?",
          limit,
        );
    return rows.map((r) => JSON.parse(r.data) as WebhookDelivery);
  }

  /**
   * The delivery log (params: id = webhook UID, deliveryId, ?limit):
   *   listDeliveries          newest first, without bodies ({items, page_info})
   *   getDelivery             one delivery with its body (404 when it is not this webhook's)
   *   latestAcceptedDelivery  the newest accepted delivery with a body among the last 50, or null
   *   listEvents              the inbound event queue (diagnostics)
   */
  async deliveries(_meta: unknown, op: string, params: Record<string, string>): Promise<unknown> {
    return this.rpc(() => {
      const webhookId = params["id"] ?? (this.partition().startsWith("webhook:") ? this.partition().slice(8) : "");
      switch (op) {
        case "listDeliveries": {
          let limit = Number(params["?limit"] ?? "50");
          if (!Number.isInteger(limit))
            return { $error: { code: "invalid_request", detail: `invalid limit ${JSON.stringify(params["?limit"])}` } };
          if (limit <= 0 || limit > 200) limit = 200;
          return {
            items: this.listDeliveryRows(webhookId, limit).map((d) => deliveryView(d, false)),
            page_info: { has_next_page: false },
          };
        }
        case "getDelivery": {
          const id = params["deliveryId"] ?? "";
          const r = first<{ data: string }>(this.sql, "SELECT data FROM deliveries WHERE id = ?", id);
          const d = r ? (JSON.parse(r.data) as WebhookDelivery) : undefined;
          // An op error (not a throw): the caller maps it with opResult.
          if (!d || (webhookId && d.webhook_id !== webhookId))
            return { $error: { code: "resource_not_found", detail: `delivery ${id} not found` } };
          return deliveryView(d, true);
        }
        case "latestAcceptedDelivery": {
          const d = this.listDeliveryRows(webhookId, 50).find(
            (x) => x.status === "accepted" && x.body !== undefined && x.body !== null,
          );
          return d ? deliveryView(d, true) : null;
        }
        case "listEvents":
          return {
            events: all(
              this.sql,
              "SELECT seq, id, action_key, received_ms, state, attempts, last_error FROM events ORDER BY seq DESC LIMIT 200",
            ),
          };
      }
      throw new OfError("feature_not_available", `IngressDO does not serve ${op}`);
    });
  }

  async pump(): Promise<void> {
    await this.alarm();
  }
}
