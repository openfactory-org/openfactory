// IndexDO (key team:<id>|index): the read model of the team, rebuilt from
// projections (03 §11.5): run and work-item summaries, inbox items (WorkDO
// items as projections, team-level items owned here), inbox recipients and
// read state, pull request facts, work-item metric events, scores, benchmark
// summaries and dead letters. projections.ts writes the projected entities;
// lists.ts reads the lists. IndexDO also fans out the team SSE stream
// (/api/v1/stream): the Worker opens a hibernatable WebSocket on
// /subscribe/team and IndexDO sends LiveFrame events after each projection.
import { OfError, newRoutedId, newToken } from "@openfactory/core";
import { requireRole } from "../../auth/roles";
import type { CallMeta, IndexRpc, LiveFrame, OutboxPayloads, Role } from "../../contracts/do-messages";
import type { Env } from "../../contracts/env";
import { BaseDO } from "../../lib/base";
import { LiveHub, isUpgrade } from "../../lib/live/index";
import type { OutboxRow } from "../../lib/outbox/outbox";
import { msOf } from "../../lib/page";
import type { Migration } from "../../lib/sql/migrate";
import { first } from "../../lib/sql/query";
import { pruneIndex, type RetentionRpc } from "../../retention/sweep";
import { runList } from "./lists";
import m0001 from "./migrations/0001_init";
import { PROJECTIONS, n, obj, s, upsertInbox, type J } from "./projections";

const MIGRATIONS: readonly Migration[] = [{ version: 1, name: "init", sql: m0001 }];

interface TeamSub {
  factoryId: string;
  topics: string[];
}

export class IndexDO extends BaseDO implements IndexRpc, RetentionRpc {
  readonly live: LiveHub;

  constructor(ctx: DurableObjectState, env: Env) {
    super(ctx, env);
    this.live = new LiveHub(ctx);
    this.onMessage("index.project", (p) => this.project(p));
    this.onMessage("index.workItemEvent", (p) => {
      this.sql.exec(
        `INSERT OR IGNORE INTO work_item_metric_events (id, work_item_id, factory_id, kind, at_ms, data) VALUES (?, ?, ?, ?, ?, ?)`,
        p.eventId,
        p.wiId,
        p.factoryId,
        p.kind,
        p.atMs,
        JSON.stringify(p.data ?? {}),
      );
    });
    this.onMessage("index.prFacts", (p) => this.prFacts(p));
    this.onMessage("index.inboxCreate", (p) => this.inboxCreate(p.item));
    this.onMessage("index.deadLetter", (p) => this.deadLetter(p));
  }

  protected migrations(): readonly Migration[] {
    return MIGRATIONS;
  }

  async ping(): Promise<string> {
    return "IndexDO";
  }

  /** GET /subscribe/team?factory_id=&topics=run,inbox (WebSocket upgrade from the Worker). */
  override async fetch(request: Request): Promise<Response> {
    const url = new URL(request.url);
    if (url.pathname === "/subscribe/team" && isUpgrade(request)) {
      const topics = (url.searchParams.get("topics") ?? "").split(",").filter(Boolean);
      const sub: TeamSub = { factoryId: url.searchParams.get("factory_id") ?? "", topics };
      return this.live.accept(["team"], sub);
    }
    return new Response("not found", { status: 404 });
  }

  // ---------- projections ----------

  /** Upsert if newer (entity, id, version); summary null deletes. */
  private project(p: OutboxPayloads["index.project"]): void {
    const proj = Object.hasOwn(PROJECTIONS, p.entity) ? PROJECTIONS[p.entity] : undefined;
    if (!proj) throw new OfError("invalid_request", `unknown entity ${p.entity}`);
    const cur = first<{ version: number }>(
      this.sql,
      "SELECT version FROM entity_versions WHERE entity = ? AND id = ?",
      p.entity,
      p.id,
    );
    if (cur && cur.version >= p.version) return;
    this.sql.exec(
      "INSERT INTO entity_versions (entity, id, version) VALUES (?, ?, ?) ON CONFLICT(entity, id) DO UPDATE SET version = excluded.version",
      p.entity,
      p.id,
      p.version,
    );
    const sum = p.summary;
    if (!sum) {
      proj.remove(this.sql, p.id);
      return;
    }
    proj.upsert(this.sql, p.id, p.version, sum);
    if (proj.topic) this.broadcast(proj.topic, s(sum["factory_id"]), sum);
  }

  private prFacts(p: OutboxPayloads["index.prFacts"]): void {
    const src = obj(p.event.source);
    const attrs = obj(p.event.attributes);
    const state = s(attrs["state"]) || s(attrs["pr_state"]);
    this.sql.exec(
      `INSERT INTO pull_request_facts (id, pr_key, repo, head_branch, is_open, received_ms, data) VALUES (?, ?, ?, ?, ?, ?, ?)
       ON CONFLICT(id) DO NOTHING`,
      p.event.id,
      p.prKey,
      s(src["repo"]),
      s(attrs["head_branch"]) || s(src["branch"]),
      state === "closed" || state === "merged" ? 0 : 1,
      msOf(p.event.received_at) || Date.now(),
      JSON.stringify(p.event),
    );
  }

  /** Team-level inbox items (spend notices, proposal reviews). The ID carries an "i" token. */
  private inboxCreate(item: Record<string, unknown>): void {
    const id = s(item["id"]) || newRoutedId("inb", newToken("i"));
    const version = n(item["version"]) || 1;
    const full: J = { ...item, id, version, created_at: s(item["created_at"]) || new Date().toISOString() };
    if (first(this.sql, "SELECT 1 FROM inbox WHERE id = ?", id)) return;
    this.sql.exec(
      "INSERT INTO entity_versions (entity, id, version) VALUES ('inbox', ?, ?) ON CONFLICT(entity, id) DO NOTHING",
      id,
      version,
    );
    upsertInbox(this.sql, id, version, full, "index");
    this.broadcast("inbox", s(full["factory_id"]), full);
  }

  private deadLetter(p: OutboxPayloads["index.deadLetter"]): void {
    this.sql.exec(
      `INSERT INTO dead_letters (id, origin_do, origin_msg_id, kind, lane, attempts, last_error, created_ms, data)
       VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
       ON CONFLICT(origin_do, origin_msg_id) DO UPDATE SET attempts = excluded.attempts, last_error = excluded.last_error,
         redriven = 0, created_ms = excluded.created_ms`,
      `dl_${crypto.randomUUID().replace(/-/g, "")}`,
      p.originDO,
      p.msgId,
      p.kind,
      p.lane,
      p.attempts,
      p.error,
      Date.now(),
      JSON.stringify(p),
    );
  }

  /** IndexDO's own dead rows go straight into dead_letters. */
  protected override onDeadLetter(row: OutboxRow): void {
    this.deadLetter({
      originDO: this.doName(),
      msgId: row.msg_id,
      kind: row.kind,
      lane: row.lane,
      error: row.last_error,
      attempts: row.attempts,
    });
  }

  private broadcast(topic: string, factoryId: string, data: unknown): void {
    const frame: LiveFrame = { type: "event", id: "", event: topic, data };
    this.live.broadcast("team", frame, (a) => {
      const sub = a as TeamSub | null;
      if (!sub) return true;
      if (sub.topics.length > 0 && !sub.topics.includes(topic)) return false;
      return sub.factoryId === "" || sub.factoryId === factoryId;
    });
  }

  // ---------- reads ----------

  /** Paged reads of the read model (lists.ts LISTS names each op and its query keys). Returns {items, page_info}. */
  async list(meta: CallMeta, op: string, params: Record<string, string>): Promise<unknown> {
    return this.rpc(() => {
      this.authorize(meta, params["factory_id"] ?? "", op === "deadLetters" ? "admin" : "user");
      return runList(this.sql, op, params);
    });
  }

  /**
   * Authorizes a caller that comes from a REST route: the principal must be of
   * this team and have at least `min` on the factory ("" = the team role).
   * The Worker does not check the role of factory-scoped routes, so this DO
   * does. Internal calls (integrations, scoring, metrics) carry no principal.
   */
  private authorize(meta: CallMeta | undefined, factoryId: string, min: Role): void {
    const p = meta?.principal;
    if (!p) return;
    if (p.teamId && p.teamId !== this.teamId()) throw new OfError("not_authorized", "resource belongs to another team");
    requireRole(p, factoryId, min);
  }

  /** One inbox item from the read model (team-level items are owned here). */
  async getInboxItem(meta: CallMeta, inboxId: string): Promise<unknown> {
    return this.rpc(() => {
      const r = first<{ data: string; factory_id: string }>(
        this.sql,
        "SELECT data, factory_id FROM inbox WHERE id = ?",
        inboxId,
      );
      if (!r) throw new OfError("resource_not_found", `inbox item ${inboxId} not found`);
      this.authorize(meta, r.factory_id, "user");
      return JSON.parse(r.data) as unknown;
    });
  }

  /** POST /inbox/{id}/read: per-user read state. */
  async markInboxRead(meta: CallMeta, inboxId: string): Promise<void> {
    return this.rpc(() => {
      const user = meta.principal?.userId ?? meta.principal?.id ?? "";
      if (!user) throw new OfError("authentication_required");
      this.tx(() => {
        const r = first<{ factory_id: string }>(this.sql, "SELECT factory_id FROM inbox WHERE id = ?", inboxId);
        if (!r) throw new OfError("resource_not_found", `inbox item ${inboxId} not found`);
        this.authorize(meta, r.factory_id, "user");
        this.sql.exec(
          "INSERT OR IGNORE INTO inbox_reads (inbox_id, user_id, read_ms) VALUES (?, ?, ?)",
          inboxId,
          user,
          Date.now(),
        );
      });
    });
  }

  /** Marks a dead letter redriven (after the origin DO reset its row). */
  async deadLetterRedriven(originDO: string, msgId: string): Promise<boolean> {
    return this.rpc(
      () =>
        this.tx(() =>
          this.sql.exec(
            "UPDATE dead_letters SET redriven = 1 WHERE origin_do = ? AND origin_msg_id = ?",
            originDO,
            msgId,
          ),
        ).rowsWritten > 0,
    );
  }

  /** Looks up a dead letter by its ID (GET /outbox/dead, POST /outbox/{id}/redrive). */
  async findDeadLetter(id: string): Promise<{ originDO: string; msgId: string } | null> {
    return this.rpc(() => {
      const r = first<{ origin_do: string; origin_msg_id: string }>(
        this.sql,
        "SELECT origin_do, origin_msg_id FROM dead_letters WHERE id = ?",
        id,
      );
      return r ? { originDO: r.origin_do, msgId: r.origin_msg_id } : null;
    });
  }

  /** One bounded retention step of the read model (retention.sweep job). */
  async retentionSweep(): Promise<{ more: boolean }> {
    return this.rpc(() => this.tx(() => ({ more: pruneIndex(this.sql, Date.now()).more })));
  }
}
