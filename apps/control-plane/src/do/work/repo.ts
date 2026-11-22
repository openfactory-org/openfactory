// Synchronous SQL repository of WorkDO. Every function runs inside the
// caller's transactionSync (03 §4.2: all RunnerStore methods join the
// calling transaction). Semantics follow core.RunnerStore and core.Store.
import { conflict, invalid, newRoutedId, notFound, partitionOf } from "@openfactory/core";
import { all, first } from "../../lib/sql/query";
import { iso, ms } from "../../engine/sessions";
import type {
  InboxItem,
  Run,
  RunInput,
  RunInputState,
  RunMessage,
  RunnerCommand,
  RunnerEvent,
  RunSession,
  TimelineEvent,
  WorkItem,
  WorkItemEvent,
  WorkItemRef,
} from "../../engine/types";

type Data = { data: string };

/**
 * The diagnostic events of a run. The literal type lets SQLite use the partial
 * index runner_events_diag; an ORDER BY in SQL would make it walk the primary
 * key over every event of the run instead, so the few rows sort in JS.
 */
export const DIAGNOSTICS_SQL =
  "SELECT session, seq, type, turn_id, at_ms, data FROM runner_events WHERE run_id = ? AND type = 'diagnostic'";

/** The user messages of one actor on a run after a time (the send_message limit). */
export const SENT_MESSAGES_SQL =
  "SELECT COUNT(*) AS n FROM run_messages WHERE run_id = ? AND role = 'user' AND created_ms > ? AND json_extract(data, '$.data.actor') = ?";

export class SeqGapError extends Error {
  override readonly name = "SeqGapError";
  constructor(readonly ack: number) {
    super("sequence gap");
  }
}

export class Repo {
  constructor(
    readonly sql: SqlStorage,
    readonly now: () => number,
  ) {}

  // ---------- runs ----------

  getRun(id: string): Run | undefined {
    const r = first<Data>(this.sql, "SELECT data FROM runs WHERE id = ?", id);
    return r ? (JSON.parse(r.data) as Run) : undefined;
  }

  mustRun(id: string): Run {
    const r = this.getRun(id);
    if (!r) throw notFound(`run ${id}`);
    return r;
  }

  createRun(r: Run): void {
    if (this.getRun(r.id)) throw conflict(`run ${r.id} exists`);
    r.version = 1;
    this.sql.exec(
      `INSERT INTO runs (id, factory_id, work_item_id, parent_run_id, agent_name, name, state, worker_host, triggered_by,
        created_ms, version, data) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
      r.id,
      r.factory_id,
      r.work_item_id ?? "",
      r.parent_run_id ?? "",
      r.agent_name ?? "",
      r.name ?? "",
      r.state,
      r.config?.worker_host || "server",
      r.triggered_by ?? "",
      ms(r.created_at) ?? this.now(),
      1,
      JSON.stringify(r),
    );
  }

  /** Writes the run and adds 1 to its version. */
  putRun(r: Run): void {
    r.version = (r.version ?? 0) + 1;
    this.sql.exec(
      `UPDATE runs SET state = ?, parent_run_id = ?, work_item_id = ?, name = ?, version = ?, data = ? WHERE id = ?`,
      r.state,
      r.parent_run_id ?? "",
      r.work_item_id ?? "",
      r.name ?? "",
      r.version,
      JSON.stringify(r),
      r.id,
    );
  }

  listRuns(filter: { parentRunId?: string; workItemId?: string; states?: string[] } = {}): Run[] {
    const where: string[] = [];
    const args: unknown[] = [];
    if (filter.parentRunId !== undefined) {
      where.push("parent_run_id = ?");
      args.push(filter.parentRunId);
    }
    if (filter.workItemId !== undefined) {
      where.push("work_item_id = ?");
      args.push(filter.workItemId);
    }
    if (filter.states?.length) {
      where.push(`state IN (${filter.states.map(() => "?").join(",")})`);
      args.push(...filter.states);
    }
    const q = `SELECT data FROM runs ${where.length ? "WHERE " + where.join(" AND ") : ""} ORDER BY created_ms, id`;
    return all<Data>(this.sql, q, ...args).map((r) => JSON.parse(r.data) as Run);
  }

  /** Finished runs whose R2 data is not pruned, after afterId by ID (one retention page). */
  listUnprunedFinishedRuns(afterId: string, limit: number): Run[] {
    return all<Data>(
      this.sql,
      `SELECT data FROM runs WHERE id > ? AND state IN ('succeeded', 'failed', 'error', 'cancelled')
       AND NOT EXISTS (SELECT 1 FROM run_state WHERE run_state.run_id = runs.id AND k = 'data_pruned')
       ORDER BY id LIMIT ?`,
      afterId,
      limit,
    ).map((r) => JSON.parse(r.data) as Run);
  }

  // ---------- messages (conversation) ----------

  appendMessage(m: Omit<RunMessage, "seq"> & { seq?: number }): RunMessage {
    const row = first<{ s: number }>(this.sql, "SELECT msg_seq + 1 AS s FROM runs WHERE id = ?", m.run_id);
    if (!row) throw notFound(`run ${m.run_id}`);
    const exists = first(this.sql, "SELECT 1 FROM run_messages WHERE id = ?", m.id);
    if (exists) return this.getMessage(m.id)!;
    const msg: RunMessage = { ...m, seq: row.s };
    this.sql.exec("UPDATE runs SET msg_seq = ? WHERE id = ?", row.s, m.run_id);
    this.sql.exec(
      "INSERT INTO run_messages (run_id, seq, id, role, created_ms, data) VALUES (?, ?, ?, ?, ?, ?)",
      m.run_id,
      row.s,
      m.id,
      m.role,
      ms(m.created_at) ?? this.now(),
      JSON.stringify(msg),
    );
    return msg;
  }

  getMessage(id: string): RunMessage | undefined {
    const r = first<Data>(this.sql, "SELECT data FROM run_messages WHERE id = ?", id);
    return r ? (JSON.parse(r.data) as RunMessage) : undefined;
  }

  listMessages(runId: string, afterSeq = 0, limit = 0): RunMessage[] {
    const lim = limit > 0 ? ` LIMIT ${Math.floor(limit)}` : "";
    return all<Data>(
      this.sql,
      `SELECT data FROM run_messages WHERE run_id = ? AND seq > ? ORDER BY seq${lim}`,
      runId,
      afterSeq,
    ).map((r) => JSON.parse(r.data) as RunMessage);
  }

  /** The raw rows of the messages of a run after afterSeq (compaction to R2). */
  messageRows(runId: string, afterSeq: number, limit: number): { seq: number; data: string }[] {
    return all<{ seq: number; data: string }>(
      this.sql,
      "SELECT seq, data FROM run_messages WHERE run_id = ? AND seq > ? ORDER BY seq LIMIT ?",
      runId,
      afterSeq,
      limit,
    );
  }

  /** Deletes the messages of a run up to seq (they are in R2). Returns the deleted rows. */
  deleteMessagesUpTo(runId: string, upto: number): number {
    return this.sql.exec("DELETE FROM run_messages WHERE run_id = ? AND seq <= ?", runId, upto).rowsWritten;
  }

  /**
   * The user messages that actor (engine.MessageActor: data.actor) stored on
   * the run after afterMs. SQLite filters and counts the rows.
   */
  countUserMessagesBy(runId: string, actor: string, afterMs: number): number {
    return Number(first<{ n: number }>(this.sql, SENT_MESSAGES_SQL, runId, afterMs, actor)?.n ?? 0);
  }

  // ---------- sessions ----------

  getSession(runId: string, n: number): RunSession | undefined {
    const r = first<Data>(this.sql, "SELECT data FROM run_sessions WHERE run_id = ? AND session = ?", runId, n);
    return r ? (JSON.parse(r.data) as RunSession) : undefined;
  }

  latestSession(runId: string): RunSession | undefined {
    const r = first<Data>(
      this.sql,
      "SELECT data FROM run_sessions WHERE run_id = ? ORDER BY session DESC LIMIT 1",
      runId,
    );
    return r ? (JSON.parse(r.data) as RunSession) : undefined;
  }

  listSessions(runId: string): RunSession[] {
    return all<Data>(this.sql, "SELECT data FROM run_sessions WHERE run_id = ? ORDER BY session", runId).map(
      (r) => JSON.parse(r.data) as RunSession,
    );
  }

  sessionByBootstrapHash(hash: string): RunSession | undefined {
    if (!hash) return undefined;
    const r = first<Data>(this.sql, "SELECT data FROM run_sessions WHERE bootstrap_hash = ?", hash);
    return r ? (JSON.parse(r.data) as RunSession) : undefined;
  }

  /** CreateRunSession: n must be max+1 and older sessions ended. */
  createSession(s: RunSession): void {
    if (!this.getRun(s.run_id)) throw notFound(`run ${s.run_id}`);
    const latest = this.latestSession(s.run_id);
    const want = latest ? latest.session + 1 : 1;
    if (s.session !== want) throw conflict(`session ${s.session} of run ${s.run_id}: want ${want}`);
    if (latest && latest.state !== "ended") throw conflict(`session ${latest.session} of run ${s.run_id} is open`);
    const now = iso(this.now());
    s.version = 1;
    s.event_ack = 0;
    s.command_seq = 0;
    s.command_ack = 0;
    s.assigned_at ||= now;
    s.last_seen_at ||= now;
    this.sql.exec(
      `INSERT INTO run_sessions (run_id, session, task_id, state, bootstrap_hash, active_turn_id, last_seen_ms, provider, worker_id,
        version, data) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
      s.run_id,
      s.session,
      s.task_id,
      s.state,
      s.bootstrap_hash ?? "",
      s.active_turn_id ?? "",
      ms(s.last_seen_at) ?? this.now(),
      s.backend ?? "",
      s.worker_id ?? "",
      1,
      JSON.stringify(s),
    );
  }

  /** Writes the session (no version check needed inside one DO) and adds 1 to its version. */
  putSession(s: RunSession): void {
    s.version = (s.version ?? 0) + 1;
    this.sql.exec(
      `UPDATE run_sessions SET state = ?, bootstrap_hash = ?, active_turn_id = ?, last_seen_ms = ?, version = ?, data = ?
       WHERE run_id = ? AND session = ?`,
      s.state,
      s.bootstrap_hash ?? "",
      s.active_turn_id ?? "",
      ms(s.last_seen_at) ?? this.now(),
      s.version,
      JSON.stringify(s),
      s.run_id,
      s.session,
    );
  }

  touchSession(s: RunSession, atMs: number): boolean {
    const cur = ms(s.last_seen_at) ?? 0;
    if (atMs <= cur) return false;
    s.last_seen_at = iso(atMs);
    this.sql.exec(
      "UPDATE run_sessions SET last_seen_ms = ?, data = json_set(data, '$.last_seen_at', ?) WHERE run_id = ? AND session = ?",
      atMs,
      s.last_seen_at,
      s.run_id,
      s.session,
    );
    return true;
  }

  stopSentMs(runId: string, n: number): number | undefined {
    const r = first<{ t: number | null }>(
      this.sql,
      "SELECT stop_sent_ms AS t FROM run_sessions WHERE run_id = ? AND session = ?",
      runId,
      n,
    );
    return r?.t ?? undefined;
  }

  setStopSent(runId: string, n: number, atMs: number): void {
    this.sql.exec("UPDATE run_sessions SET stop_sent_ms = ? WHERE run_id = ? AND session = ?", atMs, runId, n);
  }

  // ---------- runner events ----------

  /**
   * AppendRunnerEvents: returns the new ack and the stored (fresh) events.
   * Seq must increase by 1 from >= 1 (invalid). Events at or below the ack are
   * ignored. A gap throws SeqGapError with the stored ack.
   */
  appendEvents(
    s: RunSession,
    events: readonly Omit<RunnerEvent, "run_id" | "session">[],
  ): { ack: number; fresh: RunnerEvent[] } {
    for (let i = 0; i < events.length; i++) {
      const e = events[i]!;
      if (!Number.isInteger(e.seq) || e.seq < 1) throw invalid(`event seq ${e.seq} must be 1 or more`);
      if (i > 0 && e.seq !== events[i - 1]!.seq + 1) throw invalid("event seqs must increase by 1");
    }
    if (s.state === "ended") throw conflict("session ended");
    const fresh = events.filter((e) => e.seq > s.event_ack);
    if (fresh.length === 0) return { ack: s.event_ack, fresh: [] };
    if (fresh[0]!.seq !== s.event_ack + 1) throw new SeqGapError(s.event_ack);
    const out: RunnerEvent[] = [];
    for (const e of fresh) {
      const ev: RunnerEvent = { ...e, run_id: s.run_id, session: s.session };
      this.sql.exec(
        "INSERT INTO runner_events (run_id, session, seq, type, turn_id, at_ms, data) VALUES (?, ?, ?, ?, ?, ?, ?)",
        s.run_id,
        s.session,
        e.seq,
        e.type,
        e.turn_id ?? "",
        ms(e.at) ?? this.now(),
        e.data === undefined ? null : JSON.stringify(e.data),
      );
      out.push(ev);
    }
    s.event_ack = fresh[fresh.length - 1]!.seq;
    s.last_seen_at = iso(Math.max(this.now(), ms(s.last_seen_at) ?? 0));
    return { ack: s.event_ack, fresh: out };
  }

  listEvents(runId: string, session: number, afterSeq = 0, limit = 0): RunnerEvent[] {
    const lim = limit > 0 ? ` LIMIT ${Math.floor(limit)}` : "";
    return all<{ seq: number; type: string; turn_id: string; at_ms: number; data: string | null }>(
      this.sql,
      `SELECT seq, type, turn_id, at_ms, data FROM runner_events WHERE run_id = ? AND session = ? AND seq > ? ORDER BY seq${lim}`,
      runId,
      session,
      afterSeq,
    ).map((r) => ({
      run_id: runId,
      session,
      seq: r.seq,
      type: r.type,
      ...(r.turn_id ? { turn_id: r.turn_id } : {}),
      at: iso(r.at_ms),
      ...(r.data !== null ? { data: JSON.parse(r.data) as unknown } : {}),
    }));
  }

  /** The diagnostic events of a run in session and seq order. */
  listDiagnostics(runId: string): RunnerEvent[] {
    return all<{ session: number; seq: number; type: string; turn_id: string; at_ms: number; data: string | null }>(
      this.sql,
      DIAGNOSTICS_SQL,
      runId,
    )
      .sort((a, b) => a.session - b.session || a.seq - b.seq)
      .map((r) => ({
        run_id: runId,
        session: r.session,
        seq: r.seq,
        type: r.type,
        ...(r.turn_id ? { turn_id: r.turn_id } : {}),
        at: iso(r.at_ms),
        ...(r.data !== null ? { data: JSON.parse(r.data) as unknown } : {}),
      }));
  }

  // ---------- runner commands ----------

  appendCommand(s: RunSession, type: string, data: unknown): RunnerCommand {
    if (s.state === "ended") throw conflict("session ended");
    s.command_seq += 1;
    const c: RunnerCommand = {
      run_id: s.run_id,
      session: s.session,
      seq: s.command_seq,
      type,
      created_at: iso(this.now()),
    };
    if (data !== undefined && data !== null) c.data = data;
    this.sql.exec(
      "INSERT INTO runner_commands (run_id, session, seq, type, created_ms, data) VALUES (?, ?, ?, ?, ?, ?)",
      s.run_id,
      s.session,
      c.seq,
      type,
      this.now(),
      c.data === undefined ? null : JSON.stringify(c.data),
    );
    return c;
  }

  listCommands(runId: string, session: number, afterSeq: number): RunnerCommand[] {
    return all<{ seq: number; type: string; created_ms: number; data: string | null }>(
      this.sql,
      "SELECT seq, type, created_ms, data FROM runner_commands WHERE run_id = ? AND session = ? AND seq > ? ORDER BY seq",
      runId,
      session,
      afterSeq,
    ).map((r) => ({
      run_id: runId,
      session,
      seq: r.seq,
      type: r.type,
      created_at: iso(r.created_ms),
      ...(r.data !== null ? { data: JSON.parse(r.data) as unknown } : {}),
    }));
  }

  ackCommands(s: RunSession, ack: number): boolean {
    const a = Math.min(ack, s.command_seq);
    if (a <= s.command_ack) return false;
    s.command_ack = a;
    return true;
  }

  // ---------- inputs (message queue) ----------

  private inputRow(r: { data: string; state: string; session: number; turn_id: string; accepted: number }): RunInput {
    const m = JSON.parse(r.data) as RunInput;
    m.state = r.state as RunInputState;
    if (r.session) m.session = r.session;
    else delete m.session;
    if (r.turn_id) m.turn_id = r.turn_id;
    else delete m.turn_id;
    if (r.accepted) m.accepted = true;
    else delete m.accepted;
    return m;
  }

  /** AppendRunInput: returns created=false and the existing message for a known dedupe key. */
  appendInput(m: Omit<RunInput, "seq" | "state" | "created_at"> & { created_at?: string }): {
    created: boolean;
    input: RunInput;
  } {
    const run = first<{ s: number }>(this.sql, "SELECT input_seq + 1 AS s FROM runs WHERE id = ?", m.run_id);
    if (!run) throw notFound(`run ${m.run_id}`);
    if (m.dedupe_key) {
      const ex = first<{ data: string; state: string; session: number; turn_id: string; accepted: number }>(
        this.sql,
        "SELECT data, state, session, turn_id, accepted FROM run_inputs WHERE run_id = ? AND dedupe_key = ?",
        m.run_id,
        m.dedupe_key,
      );
      if (ex) return { created: false, input: this.inputRow(ex) };
    }
    const input: RunInput = {
      ...m,
      message_id: m.message_id || newRoutedId("msg", partitionOf(m.run_id)),
      seq: run.s,
      state: "pending",
      created_at: m.created_at ?? iso(this.now()),
    };
    delete input.session;
    delete input.turn_id;
    delete input.accepted;
    this.sql.exec("UPDATE runs SET input_seq = ? WHERE id = ?", run.s, m.run_id);
    this.sql.exec(
      `INSERT INTO run_inputs (id, run_id, seq, kind, state, session, turn_id, accepted, dedupe_key, created_ms, data)
       VALUES (?, ?, ?, ?, 'pending', 0, '', 0, ?, ?, ?)`,
      input.message_id,
      input.run_id,
      input.seq,
      input.kind,
      input.dedupe_key ?? "",
      ms(input.created_at) ?? this.now(),
      JSON.stringify(input),
    );
    return { created: true, input };
  }

  getInput(id: string): RunInput | undefined {
    const r = first<{ data: string; state: string; session: number; turn_id: string; accepted: number }>(
      this.sql,
      "SELECT data, state, session, turn_id, accepted FROM run_inputs WHERE id = ?",
      id,
    );
    return r ? this.inputRow(r) : undefined;
  }

  listInputs(runId: string, afterSeq = 0, states: RunInputState[] = []): RunInput[] {
    const st = states.length ? ` AND state IN (${states.map(() => "?").join(",")})` : "";
    return all<{ data: string; state: string; session: number; turn_id: string; accepted: number }>(
      this.sql,
      `SELECT data, state, session, turn_id, accepted FROM run_inputs WHERE run_id = ? AND seq > ?${st} ORDER BY seq`,
      runId,
      afterSeq,
      ...states,
    ).map((r) => this.inputRow(r));
  }

  markDelivered(runId: string, ids: string[], session: number, turnId: string): number {
    let n = 0;
    for (const id of ids) {
      n += this.sql.exec(
        "UPDATE run_inputs SET state = 'delivered', session = ?, turn_id = ?, accepted = 0 WHERE run_id = ? AND id = ? AND state = 'pending'",
        session,
        turnId,
        runId,
        id,
      ).rowsWritten;
    }
    return n;
  }

  markAccepted(runId: string, ids: string[]): number {
    let n = 0;
    for (const id of ids) {
      n += this.sql.exec(
        "UPDATE run_inputs SET accepted = 1 WHERE run_id = ? AND id = ? AND state = 'delivered' AND accepted = 0",
        runId,
        id,
      ).rowsWritten;
    }
    return n;
  }

  markAcceptedByTurn(runId: string, turnId: string): number {
    return this.sql.exec(
      "UPDATE run_inputs SET accepted = 1 WHERE run_id = ? AND turn_id = ? AND state = 'delivered' AND accepted = 0",
      runId,
      turnId,
    ).rowsWritten;
  }

  /** FinishRunInputsOfTurn: accepted -> consumed, others back to pending. Updates consumed_seq of the run. */
  finishTurnInputs(runId: string, turnId: string): { consumed: number; returned: number; maxConsumedSeq: number } {
    const top = first<{ m: number | null }>(
      this.sql,
      "SELECT MAX(seq) AS m FROM run_inputs WHERE run_id = ? AND turn_id = ? AND state = 'delivered' AND accepted = 1",
      runId,
      turnId,
    );
    const consumed = this.sql.exec(
      "UPDATE run_inputs SET state = 'consumed' WHERE run_id = ? AND turn_id = ? AND state = 'delivered' AND accepted = 1",
      runId,
      turnId,
    ).rowsWritten;
    const returned = this.sql.exec(
      "UPDATE run_inputs SET state = 'pending', session = 0, turn_id = '', accepted = 0 WHERE run_id = ? AND turn_id = ? AND state = 'delivered'",
      runId,
      turnId,
    ).rowsWritten;
    return { consumed, returned, maxConsumedSeq: top?.m ?? 0 };
  }

  resetSessionInputs(runId: string, session: number): number {
    return this.sql.exec(
      "UPDATE run_inputs SET state = 'pending', session = 0, turn_id = '', accepted = 0 WHERE run_id = ? AND session = ? AND state = 'delivered'",
      runId,
      session,
    ).rowsWritten;
  }

  // ---------- timeline ----------

  appendTimeline(ev: TimelineEvent): boolean {
    if (!ev.event_id) throw invalid("timeline event id is empty");
    return (
      this.sql.exec(
        "INSERT OR IGNORE INTO run_timeline (run_id, event_id, session, type, at_ms, data) VALUES (?, ?, ?, ?, ?, ?)",
        ev.run_id,
        ev.event_id,
        ev.session,
        ev.type,
        ms(ev.at) ?? this.now(),
        ev.data === undefined ? null : JSON.stringify(ev.data),
      ).rowsWritten > 0
    );
  }

  listTimeline(runId: string, afterMs = 0, limit = 0): TimelineEvent[] {
    const lim = limit > 0 ? ` LIMIT ${Math.floor(limit)}` : "";
    return all<{ event_id: string; session: number; type: string; at_ms: number; data: string | null }>(
      this.sql,
      `SELECT event_id, session, type, at_ms, data FROM run_timeline WHERE run_id = ? AND at_ms > ? ORDER BY at_ms, event_id${lim}`,
      runId,
      afterMs,
    ).map((r) => ({
      event_id: r.event_id,
      run_id: runId,
      session: r.session,
      type: r.type,
      at: iso(r.at_ms),
      ...(r.data !== null ? { data: JSON.parse(r.data) as unknown } : {}),
    }));
  }

  // ---------- run uploads and snapshot parts ----------

  /** An upload that the runner reported (an artifact or the transcript); a later report of the same name wins. */
  putUpload(runId: string, session: number, kind: string, name: string, uploadId: string): void {
    this.sql.exec(
      "INSERT OR REPLACE INTO run_uploads (run_id, session, kind, name, upload_id, at_ms) VALUES (?, ?, ?, ?, ?, ?)",
      runId,
      session,
      kind,
      name,
      uploadId,
      this.now(),
    );
  }

  /** A saved part of a workspace snapshot (workspace, harness_state); a later save of the same part wins. */
  putSnapshotPart(runId: string, session: number, part: string, uploadIds: unknown[], bytes: number): void {
    this.sql.exec(
      "INSERT OR REPLACE INTO snapshot_parts (run_id, session, part, upload_ids, bytes, at_ms) VALUES (?, ?, ?, ?, ?, ?)",
      runId,
      session,
      part,
      JSON.stringify(uploadIds),
      bytes,
      this.now(),
    );
  }

  /** The snapshot parts that each session of a run saved, in one query. */
  snapshotPartsBySession(runId: string): Map<number, Set<string>> {
    const out = new Map<number, Set<string>>();
    for (const r of all<{ session: number; part: string }>(
      this.sql,
      "SELECT session, part FROM snapshot_parts WHERE run_id = ?",
      runId,
    )) {
      const n = Number(r.session);
      let set = out.get(n);
      if (!set) out.set(n, (set = new Set()));
      set.add(r.part);
    }
    return out;
  }

  /** For each part, the upload IDs of the newest session before `before` that saved it (snapshotFiles). */
  latestSnapshotParts(runId: string, before: number): { session: number; part: string; uploadIds: string[] }[] {
    const rows = all<{ session: number; part: string; upload_ids: string }>(
      this.sql,
      "SELECT session, part, upload_ids FROM snapshot_parts WHERE run_id = ? AND session < ? ORDER BY session DESC",
      runId,
      before,
    );
    const out: { session: number; part: string; uploadIds: string[] }[] = [];
    const seen = new Set<string>();
    for (const r of rows) {
      if (seen.has(r.part)) continue;
      seen.add(r.part);
      out.push({ session: Number(r.session), part: r.part, uploadIds: JSON.parse(r.upload_ids) as string[] });
    }
    return out;
  }

  // ---------- work item ----------

  getWorkItem(id?: string): WorkItem | undefined {
    const r = id
      ? first<Data>(this.sql, "SELECT data FROM work_item WHERE id = ?", id)
      : first<Data>(this.sql, "SELECT data FROM work_item LIMIT 1");
    return r ? (JSON.parse(r.data) as WorkItem) : undefined;
  }

  createWorkItem(wi: WorkItem): void {
    wi.version = 1;
    this.sql.exec(
      `INSERT INTO work_item (id, factory_id, stage, source_key, creator_id, root_run_id, created_ms, updated_ms, completed_ms, version, data)
       VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
      wi.id,
      wi.factory_id,
      wi.stage,
      wi.source_key ?? "",
      wi.creator_id ?? "",
      wi.root_run_id ?? "",
      ms(wi.created_at) ?? this.now(),
      ms(wi.updated_at) ?? this.now(),
      ms(wi.completed_at) ?? null,
      1,
      JSON.stringify(wi),
    );
  }

  putWorkItem(wi: WorkItem): void {
    wi.version = (wi.version ?? 0) + 1;
    this.sql.exec(
      `UPDATE work_item SET stage = ?, root_run_id = ?, updated_ms = ?, completed_ms = ?, version = ?, data = ? WHERE id = ?`,
      wi.stage,
      wi.root_run_id ?? "",
      ms(wi.updated_at) ?? this.now(),
      ms(wi.completed_at) ?? null,
      wi.version,
      JSON.stringify(wi),
      wi.id,
    );
  }

  /** Indexes a reference of the work item by its key. The caller adds it to wi.references. */
  addWorkItemRef(ref: WorkItemRef): void {
    this.sql.exec(
      "INSERT OR IGNORE INTO work_item_refs (key, kind, added_ms, data) VALUES (?, ?, ?, ?)",
      ref.key,
      ref.kind,
      ms(typeof ref["added_at"] === "string" ? ref["added_at"] : undefined) ?? this.now(),
      JSON.stringify(ref),
    );
  }

  /** A value of work_item_state (repo_refs: the starting repository refs of the work item). */
  getWorkItemState(k: string): string | undefined {
    return first<{ v: string }>(this.sql, "SELECT v FROM work_item_state WHERE k = ?", k)?.v;
  }

  appendWorkItemEvent(ev: WorkItemEvent): void {
    this.sql.exec(
      "INSERT OR IGNORE INTO work_item_events (id, kind, run_id, actor, at_ms, data) VALUES (?, ?, ?, ?, ?, ?)",
      ev.id,
      ev.kind,
      ev.run_id ?? "",
      ev.actor ?? "",
      ms(ev.created_at) ?? this.now(),
      JSON.stringify(ev.data ?? {}),
    );
  }

  listWorkItemEvents(wiId: string): WorkItemEvent[] {
    return all<{ id: string; kind: string; run_id: string; actor: string; at_ms: number; data: string }>(
      this.sql,
      "SELECT id, kind, run_id, actor, at_ms, data FROM work_item_events ORDER BY seq",
    ).map((r) => ({
      id: r.id,
      work_item_id: wiId,
      kind: r.kind,
      ...(r.run_id ? { run_id: r.run_id } : {}),
      ...(r.actor ? { actor: r.actor } : {}),
      data: JSON.parse(r.data) as unknown,
      created_at: iso(r.at_ms),
    }));
  }

  // ---------- inbox ----------

  getInbox(id: string): InboxItem | undefined {
    const r = first<Data>(this.sql, "SELECT data FROM inbox_items WHERE id = ?", id);
    return r ? (JSON.parse(r.data) as InboxItem) : undefined;
  }

  createInbox(it: InboxItem): void {
    it.version = 1;
    this.sql.exec(
      "INSERT INTO inbox_items (id, run_id, kind, resolved, created_ms, version, data) VALUES (?, ?, ?, ?, ?, ?, ?)",
      it.id,
      it.run_id ?? "",
      it.kind,
      it.resolved ? 1 : 0,
      ms(it.created_at) ?? this.now(),
      1,
      JSON.stringify(it),
    );
  }

  putInbox(it: InboxItem): void {
    it.version = (it.version ?? 0) + 1;
    this.sql.exec(
      "UPDATE inbox_items SET resolved = ?, version = ?, data = ? WHERE id = ?",
      it.resolved ? 1 : 0,
      it.version,
      JSON.stringify(it),
      it.id,
    );
  }

  listInbox(opts: { resolved?: boolean; runId?: string } = {}): InboxItem[] {
    const where: string[] = [];
    const args: unknown[] = [];
    if (opts.resolved !== undefined) {
      where.push("resolved = ?");
      args.push(opts.resolved ? 1 : 0);
    }
    if (opts.runId !== undefined) {
      where.push("run_id = ?");
      args.push(opts.runId);
    }
    return all<Data>(
      this.sql,
      `SELECT data FROM inbox_items ${where.length ? "WHERE " + where.join(" AND ") : ""} ORDER BY created_ms DESC, id`,
      ...args,
    ).map((r) => JSON.parse(r.data) as InboxItem);
  }

  /** hasOpenRequest: an unresolved question or approval of the run. */
  hasOpenRequest(runId: string): boolean {
    return !!first(
      this.sql,
      "SELECT 1 FROM inbox_items WHERE run_id = ? AND resolved = 0 AND kind IN ('question', 'approval') LIMIT 1",
      runId,
    );
  }

  // ---------- event context ----------

  /** The R2 key of the event context of a message (or of the first prompt of a run), read when it is delivered. */
  putEventContext(messageKey: string, runId: string, r2Key: string): void {
    this.sql.exec(
      "INSERT OR IGNORE INTO event_context (message_id, run_id, r2_key, bytes, created_ms) VALUES (?, ?, ?, 0, ?)",
      messageKey,
      runId,
      r2Key,
      this.now(),
    );
  }

  eventContextKey(messageKey: string): string | undefined {
    return first<{ r2_key: string }>(this.sql, "SELECT r2_key FROM event_context WHERE message_id = ?", messageKey)
      ?.r2_key;
  }

  /** Drops the event context rows of a finished run. */
  deleteEventContexts(runId: string): void {
    this.sql.exec("DELETE FROM event_context WHERE run_id = ?", runId);
  }

  // ---------- dispatch ledger ----------

  hasDispatch(actionKey: string, target: string): string | undefined {
    const r = first<{ ref: string }>(
      this.sql,
      "SELECT ref FROM dispatches WHERE action_key = ? AND target = ?",
      actionKey,
      target,
    );
    return r?.ref;
  }

  recordDispatch(actionKey: string, target: string, ref: string): void {
    this.sql.exec(
      "INSERT OR IGNORE INTO dispatches (action_key, target, ref, created_ms) VALUES (?, ?, ?, ?)",
      actionKey,
      target,
      ref,
      this.now(),
    );
  }

  // ---------- grants and bot actions ----------

  /** Records a slot grant of DispatchDO; a repeated grant keeps the first row. */
  addGrant(g: {
    runId: string;
    session: number;
    target: unknown;
    providerId: string;
    workerId?: string;
    leaseId: string;
  }): void {
    this.sql.exec(
      `INSERT OR IGNORE INTO grants (run_id, session, target, provider, worker_id, lease_id, created_ms) VALUES (?, ?, ?, ?, ?, ?, ?)`,
      g.runId,
      g.session,
      JSON.stringify(g.target),
      g.providerId,
      g.workerId ?? "",
      g.leaseId,
      this.now(),
    );
  }

  /** The state of a grant (pending, used, released), undefined without one. */
  grantState(runId: string, session: number): string | undefined {
    return first<{ state: string }>(
      this.sql,
      "SELECT state FROM grants WHERE run_id = ? AND session = ?",
      runId,
      session,
    )?.state;
  }

  setGrantState(runId: string, session: number, state: "used" | "released"): void {
    this.sql.exec("UPDATE grants SET state = ? WHERE run_id = ? AND session = ?", state, runId, session);
  }

  /**
   * Counts a bot action in the window (allowBotAction): true when it is
   * already counted or fits under max; false when the window is full.
   */
  claimBotAction(actionKey: string, nowMs: number, max: number, windowMs: number): boolean {
    if (actionKey && first(this.sql, "SELECT 1 FROM bot_actions WHERE action_key = ?", actionKey)) return true;
    this.sql.exec("DELETE FROM bot_actions WHERE at_ms < ?", nowMs - windowMs);
    const n = Number(first<{ n: number }>(this.sql, "SELECT COUNT(*) AS n FROM bot_actions")?.n ?? 0);
    if (n >= max) return false;
    this.sql.exec(
      "INSERT OR IGNORE INTO bot_actions (action_key, at_ms) VALUES (?, ?)",
      actionKey || `anon:${nowMs}:${Math.random()}`,
      nowMs,
    );
    return true;
  }

  // ---------- tool calls (orchestration idempotency) ----------

  getToolCall(key: string): string | undefined {
    return first<{ result: string }>(this.sql, "SELECT result FROM tool_calls WHERE key = ?", key)?.result;
  }

  putToolCall(key: string, runId: string, turnId: string, tool: string, result: string): void {
    this.sql.exec(
      "INSERT OR IGNORE INTO tool_calls (key, run_id, turn_id, tool, result, created_ms) VALUES (?, ?, ?, ?, ?, ?)",
      key,
      runId,
      turnId,
      tool,
      result,
      this.now(),
    );
  }

  // ---------- small state ----------

  getRunState(runId: string, k: string): string | undefined {
    return first<{ v: string }>(this.sql, "SELECT v FROM run_state WHERE run_id = ? AND k = ?", runId, k)?.v;
  }

  setRunState(runId: string, k: string, v: string): void {
    this.sql.exec(
      "INSERT INTO run_state (run_id, k, v) VALUES (?, ?, ?) ON CONFLICT(run_id, k) DO UPDATE SET v = excluded.v",
      runId,
      k,
      v,
    );
  }

  getMeta(k: string): string | undefined {
    return first<{ v: string }>(this.sql, "SELECT v FROM meta WHERE k = ?", k)?.v;
  }

  setMeta(k: string, v: string): void {
    this.sql.exec("INSERT INTO meta (k, v) VALUES (?, ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v", k, v);
  }

  deleteMeta(k: string): void {
    this.sql.exec("DELETE FROM meta WHERE k = ?", k);
  }
}
