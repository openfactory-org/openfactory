// IndexDO (key team:<id>|index): the read model of the team, rebuilt from
// projections (03 §11.5): run and work-item summaries, inbox items (WorkDO
// items as projections, team-level items owned here), inbox recipients and
// read state, pull request facts, work-item metric events, scores, benchmark
// summaries and dead letters. It also fans out the team SSE stream
// (/api/v1/stream): the Worker opens a hibernatable WebSocket on
// /subscribe/team and IndexDO sends LiveFrame events after each projection.
import { OfError, newRoutedId, newToken } from "@openfactory/core";
import type { CallMeta, IndexRpc, LiveFrame, OutboxPayloads } from "../../contracts/do-messages";
import type { Env } from "../../contracts/env";
import { BaseDO } from "../../lib/base";
import { LiveHub, isUpgrade } from "../../lib/live/index";
import type { OutboxRow } from "../../lib/outbox/outbox";
import { listResponse, msOf, pageFrom, sqlPage } from "../../lib/page";
import type { Migration } from "../../lib/sql/migrate";
import { first } from "../../lib/sql/query";
import m0001 from "./migrations/0001_init";

const MIGRATIONS: readonly Migration[] = [{ version: 1, name: "init", sql: m0001 }];

/** Entities accepted by index.project, with the SSE topic they feed. */
const ENTITY_TOPIC: Record<string, string> = {
  run: "run",
  work_item: "workitem",
  inbox: "inbox",
  score: "",
  benchmark_run: "",
};

type J = Record<string, unknown>;
const s = (v: unknown): string => (typeof v === "string" ? v : "");
const n = (v: unknown): number => (typeof v === "number" && Number.isFinite(v) ? v : 0);
const obj = (v: unknown): J => (typeof v === "object" && v !== null ? (v as J) : {});

interface TeamSub {
  factoryId: string;
  topics: string[];
}

export class IndexDO extends BaseDO implements IndexRpc {
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

  override async webSocketMessage(): Promise<void> {}

  override async webSocketClose(ws: WebSocket, code: number): Promise<void> {
    try {
      ws.close(code === 1005 ? 1000 : code, "bye");
    } catch {
      /* closed */
    }
  }

  // ---------- projections ----------

  /** Upsert if newer (entity, id, version); summary null deletes. */
  private project(p: OutboxPayloads["index.project"]): void {
    if (!(p.entity in ENTITY_TOPIC)) throw new OfError("invalid_request", `unknown entity ${p.entity}`);
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
    switch (p.entity) {
      case "run":
        if (!sum) this.sql.exec("DELETE FROM run_summaries WHERE id = ?", p.id);
        else this.upsertRun(p.id, p.version, sum);
        break;
      case "work_item":
        if (!sum) this.sql.exec("DELETE FROM work_item_summaries WHERE id = ?", p.id);
        else this.upsertWorkItem(p.id, p.version, sum);
        break;
      case "inbox":
        if (!sum) {
          this.sql.exec("DELETE FROM inbox WHERE id = ?", p.id);
          this.sql.exec("DELETE FROM inbox_recipients WHERE inbox_id = ?", p.id);
        } else this.upsertInbox(p.id, p.version, sum, s(sum["owner"]) || "work");
        break;
      case "score":
        if (!sum) this.sql.exec("DELETE FROM scores WHERE id = ?", p.id);
        else this.upsertScore(p.id, p.version, sum);
        break;
      case "benchmark_run":
        if (!sum) this.sql.exec("DELETE FROM benchmark_summaries WHERE id = ?", p.id);
        else {
          this.sql.exec(
            `INSERT INTO benchmark_summaries (id, factory_id, suite, state, created_ms, version, data) VALUES (?, ?, ?, ?, ?, ?, ?)
             ON CONFLICT(id) DO UPDATE SET factory_id = excluded.factory_id, suite = excluded.suite, state = excluded.state,
             version = excluded.version, data = excluded.data`,
            p.id,
            s(sum["factory_id"]),
            s(sum["suite"]),
            s(sum["state"]),
            msOf(sum["created_at"]) || Date.now(),
            p.version,
            JSON.stringify(sum),
          );
        }
        break;
    }
    const topic = ENTITY_TOPIC[p.entity];
    if (topic && sum) this.broadcast(topic, s(sum["factory_id"]), sum);
  }

  private upsertRun(id: string, version: number, r: J): void {
    const cfg = obj(r["config"]);
    const usage = obj(r["usage"]);
    const creator = obj(r["creator"]);
    this.sql.exec(
      `INSERT INTO run_summaries (id, factory_id, work_item_id, parent_run_id, root_run_id, agent_name, name, state, creator_id,
         source, model, created_ms, finished_ms, cost_usd, version, data)
       VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
       ON CONFLICT(id) DO UPDATE SET factory_id = excluded.factory_id, work_item_id = excluded.work_item_id,
         parent_run_id = excluded.parent_run_id, root_run_id = excluded.root_run_id, agent_name = excluded.agent_name,
         name = excluded.name, state = excluded.state, creator_id = excluded.creator_id, source = excluded.source,
         model = excluded.model, finished_ms = excluded.finished_ms, cost_usd = excluded.cost_usd,
         version = excluded.version, data = excluded.data`,
      id,
      s(r["factory_id"]),
      s(r["work_item_id"]),
      s(r["parent_run_id"]),
      s(r["root_run_id"]),
      s(r["agent_name"]),
      s(r["name"]),
      s(r["state"]),
      s(creator["id"]) || s(r["creator_id"]),
      s(r["source"]),
      s(cfg["model"]) || s(r["model"]),
      msOf(r["created_at"]) || Date.now(),
      msOf(r["finished_at"]) || null,
      n(usage["cost_usd"]) || n(r["cost_usd"]),
      version,
      JSON.stringify(r),
    );
  }

  private upsertWorkItem(id: string, version: number, w: J): void {
    this.sql.exec(
      `INSERT INTO work_item_summaries (id, factory_id, stage, creator_id, title, created_ms, updated_ms, completed_ms, version, data)
       VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
       ON CONFLICT(id) DO UPDATE SET factory_id = excluded.factory_id, stage = excluded.stage, creator_id = excluded.creator_id,
         title = excluded.title, updated_ms = excluded.updated_ms, completed_ms = excluded.completed_ms,
         version = excluded.version, data = excluded.data`,
      id,
      s(w["factory_id"]),
      s(w["stage"]),
      s(w["creator_id"]),
      s(w["title"]),
      msOf(w["created_at"]) || Date.now(),
      msOf(w["updated_at"]) || Date.now(),
      msOf(w["completed_at"]) || null,
      version,
      JSON.stringify(w),
    );
  }

  private upsertInbox(id: string, version: number, it: J, owner: string): void {
    this.sql.exec(
      `INSERT INTO inbox (id, owner, factory_id, work_item_id, run_id, kind, resolved, assignee_id, created_ms, resolved_ms, version, data)
       VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
       ON CONFLICT(id) DO UPDATE SET owner = excluded.owner, factory_id = excluded.factory_id, work_item_id = excluded.work_item_id,
         run_id = excluded.run_id, kind = excluded.kind, resolved = excluded.resolved, assignee_id = excluded.assignee_id,
         resolved_ms = excluded.resolved_ms, version = excluded.version, data = excluded.data`,
      id,
      owner,
      s(it["factory_id"]),
      s(it["work_item_id"]),
      s(it["run_id"]),
      s(it["kind"]),
      it["resolved"] === true ? 1 : 0,
      s(it["assignee_id"]),
      msOf(it["created_at"]) || Date.now(),
      msOf(it["resolved_at"]) || null,
      version,
      JSON.stringify(it),
    );
    this.sql.exec("DELETE FROM inbox_recipients WHERE inbox_id = ?", id);
    const recipients = Array.isArray(it["recipients"]) ? (it["recipients"] as unknown[]).map(s).filter(Boolean) : [];
    for (const u of new Set(recipients)) {
      this.sql.exec("INSERT OR IGNORE INTO inbox_recipients (inbox_id, user_id) VALUES (?, ?)", id, u);
    }
  }

  private upsertScore(id: string, version: number, sc: J): void {
    this.sql.exec(
      `INSERT INTO scores (id, factory_id, scorer_id, run_id, state, judge_run_id, created_ms, version, data)
       VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
       ON CONFLICT(id) DO UPDATE SET factory_id = excluded.factory_id, scorer_id = excluded.scorer_id, run_id = excluded.run_id,
         state = excluded.state, judge_run_id = excluded.judge_run_id, version = excluded.version, data = excluded.data`,
      id,
      s(sc["factory_id"]),
      s(sc["scorer_id"]),
      s(sc["run_id"]),
      s(sc["state"]) || "done",
      s(sc["judge_run_id"]),
      msOf(sc["created_at"]) || Date.now(),
      version,
      JSON.stringify(sc),
    );
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
    this.upsertInbox(id, version, full, "index");
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

  /**
   * Paged reads of the read model. op:
   *   runs          factory_id, work_item_id, parent_run_id, root_run_id, creator, state (comma list), source, model
   *   workItems     factory_id, stage (comma list)
   *   inbox         user_id (recipient or assignee), factory_id, resolved ("true"/"false"), unread ("true", with user_id)
   *   scores        factory_id, scorer_id, run_id
   *   benchmarkRuns factory_id, suite
   *   deadLetters
   *   prFacts       pr_key, repo, head_branch
   *   metricEvents  factory_id or work_item_id, since_ms
   * Paging: limit, cursor, sort_order. Returns {items, page_info}.
   */
  async list(_meta: CallMeta, op: string, params: Record<string, string>): Promise<unknown> {
    return this.rpc(() => this.listSync(op, params));
  }

  private listSync(op: string, q: Record<string, string>): unknown {
    const page = pageFrom(q);
    const where: string[] = [];
    const args: unknown[] = [];
    const eq = (col: string, key: string) => {
      if (q[key]) {
        where.push(`${col} = ?`);
        args.push(q[key]);
      }
    };
    const inList = (col: string, key: string) => {
      const vals = (q[key] ?? "").split(",").map((x) => x.trim()).filter(Boolean);
      if (vals.length) {
        where.push(`${col} IN (${vals.map(() => "?").join(",")})`);
        args.push(...vals);
      }
    };
    const data = (rows: Record<string, SqlStorageValue>[]) => rows.map((r) => JSON.parse(String(r["data"])) as unknown);
    switch (op) {
      case "runs": {
        eq("factory_id", "factory_id");
        eq("work_item_id", "work_item_id");
        eq("parent_run_id", "parent_run_id");
        eq("root_run_id", "root_run_id");
        eq("creator_id", "creator");
        eq("source", "source");
        eq("model", "model");
        inList("state", "state");
        const r = sqlPage(this.sql, { select: "SELECT id, created_ms, data FROM run_summaries", where, args }, page);
        return listResponse(data(r.rows), r.next);
      }
      case "workItems": {
        eq("factory_id", "factory_id");
        inList("stage", "stage");
        const r = sqlPage(this.sql, { select: "SELECT id, created_ms, data FROM work_item_summaries", where, args }, page);
        return listResponse(data(r.rows), r.next);
      }
      case "inbox": {
        eq("factory_id", "factory_id");
        if (q["resolved"] === "true" || q["resolved"] === "false") {
          where.push("resolved = ?");
          args.push(q["resolved"] === "true" ? 1 : 0);
        }
        const user = q["user_id"] ?? "";
        if (user && q["scope"] !== "team") {
          where.push("(assignee_id = ? OR id IN (SELECT inbox_id FROM inbox_recipients WHERE user_id = ?))");
          args.push(user, user);
        }
        if (user && q["unread"] === "true") {
          where.push("id NOT IN (SELECT inbox_id FROM inbox_reads WHERE user_id = ?)");
          args.push(user);
        }
        const r = sqlPage(this.sql, { select: "SELECT id, created_ms, data FROM inbox", where, args }, page);
        const items = r.rows.map((row) => {
          const it = JSON.parse(String(row["data"])) as J;
          if (user) {
            const read = first(this.sql, "SELECT 1 FROM inbox_reads WHERE inbox_id = ? AND user_id = ?", row["id"], user);
            it["read"] = read !== undefined;
          }
          return it;
        });
        return listResponse(items, r.next);
      }
      case "scores": {
        eq("factory_id", "factory_id");
        eq("scorer_id", "scorer_id");
        eq("run_id", "run_id");
        const r = sqlPage(this.sql, { select: "SELECT id, created_ms, data FROM scores", where, args }, page);
        return listResponse(data(r.rows), r.next);
      }
      case "benchmarkRuns": {
        eq("factory_id", "factory_id");
        eq("suite", "suite");
        const r = sqlPage(this.sql, { select: "SELECT id, created_ms, data FROM benchmark_summaries", where, args }, page);
        return listResponse(data(r.rows), r.next);
      }
      case "deadLetters": {
        where.push("redriven = 0");
        const r = sqlPage(
          this.sql,
          { select: "SELECT id, origin_do, origin_msg_id, kind, lane, attempts, last_error, created_ms FROM dead_letters", where, args },
          page,
        );
        return listResponse(r.rows, r.next);
      }
      case "prFacts": {
        eq("pr_key", "pr_key");
        eq("repo", "repo");
        eq("head_branch", "head_branch");
        const r = sqlPage(
          this.sql,
          { select: "SELECT id, received_ms, data FROM pull_request_facts", where, args, timeCol: "received_ms" },
          page,
        );
        return listResponse(data(r.rows), r.next);
      }
      case "metricEvents": {
        eq("factory_id", "factory_id");
        eq("work_item_id", "work_item_id");
        if (q["since_ms"]) {
          where.push("at_ms >= ?");
          args.push(Number(q["since_ms"]));
        }
        const r = sqlPage(
          this.sql,
          { select: "SELECT id, work_item_id, kind, at_ms, data FROM work_item_metric_events", where, args, timeCol: "at_ms" },
          page,
        );
        return listResponse(
          r.rows.map((x) => ({ id: x["id"], work_item_id: x["work_item_id"], kind: x["kind"], at_ms: x["at_ms"], data: JSON.parse(String(x["data"])) as unknown })),
          r.next,
        );
      }
    }
    throw new OfError("invalid_request", `unknown list ${op}`);
  }

  /** One inbox item from the read model (team-level items are owned here). */
  async getInboxItem(_meta: CallMeta, inboxId: string): Promise<unknown> {
    return this.rpc(() => {
      const r = first<{ data: string }>(this.sql, "SELECT data FROM inbox WHERE id = ?", inboxId);
      if (!r) throw new OfError("resource_not_found", `inbox item ${inboxId} not found`);
      return JSON.parse(r.data) as unknown;
    });
  }

  /** POST /inbox/{id}/read: per-user read state. */
  async markInboxRead(meta: CallMeta, inboxId: string): Promise<void> {
    return this.rpc(() => {
      const user = meta.principal?.userId ?? meta.principal?.id ?? "";
      if (!user) throw new OfError("authentication_required");
      this.tx(() => {
        if (!first(this.sql, "SELECT 1 FROM inbox WHERE id = ?", inboxId)) {
          throw new OfError("resource_not_found", `inbox item ${inboxId} not found`);
        }
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
        this.tx(() => this.sql.exec("UPDATE dead_letters SET redriven = 1 WHERE origin_do = ? AND origin_msg_id = ?", originDO, msgId))
          .rowsWritten > 0,
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
}
