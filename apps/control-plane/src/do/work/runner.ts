// The runner protocol inside WorkDO (internal/runnerapi + dispatch hooks).
// Each function is one transaction: events are acknowledged and their
// effects applied together (C1, N1). The Worker verifies tokens and holds
// long-polls; WorkDO decides.
import type { EndRequest, StartRequest } from "@openfactory/runner-proto";
import { isOfError, runnerFailureCode, type FailureCode } from "@openfactory/core";
import { iso, ms, setFailureMessage } from "../../engine/sessions";
import { priorRetries } from "../../engine/sessions";
import type { Run, RunnerCommand, RunnerEvent, RunSession } from "../../engine/types";
import { deliver, endSession, timeline, timerName, transition, updateRun, type Fx } from "./engine";
import { SeqGapError } from "./repo";

export interface RunnerError {
  error: { status: number; code: string; message: string; ack?: number };
}

export const rerr = (status: number, code: string, message: string, ack?: number): RunnerError => ({
  error: { status, code, message, ...(ack !== undefined ? { ack } : {}) },
});

export const isRunnerError = (v: unknown): v is RunnerError =>
  typeof v === "object" && v !== null && "error" in v && typeof (v as RunnerError).error?.status === "number";

const unauthorized = (m: string) => rerr(401, "unauthorized", m);

/** Re-arms the liveness timer only when its due time moves more than 10 s. */
function armLiveness(fx: Fx, s: RunSession): void {
  const due = (ms(s.last_seen_at) ?? fx.now()) + fx.cfg.lostAfterMs + 1;
  const prev = Number(fx.repo.getMeta(`live_due:${s.task_id}`) ?? "0");
  if (Math.abs(due - prev) <= 10_000 && prev !== 0) return;
  fx.repo.setMeta(`live_due:${s.task_id}`, String(due));
  fx.setTimer(timerName.live(s.task_id), "liveness", due, { runId: s.run_id, session: s.session });
}

/** activeSession + touch (authSession). */
export function authSession(fx: Fx, runId: string, n: number): RunSession | RunnerError {
  const s = fx.repo.getSession(runId, n);
  if (!s) return unauthorized("unknown session");
  if (s.state === "ended") return unauthorized("session ended");
  const latest = fx.repo.latestSession(runId);
  if (!latest) return unauthorized("unknown session");
  if (latest.session > n) return rerr(409, "session_superseded", "a newer session exists");
  if (fx.repo.touchSession(s, fx.now()) && s.state === "active") armLiveness(fx, s);
  return s;
}

export interface StartResult {
  run: Run;
  session: RunSession;
  first: boolean;
}

/**
 * Session start (runnerapi/start.go + Dispatcher.SessionStarted). The
 * bootstrap secret is checked by hash; the first call binds the instance.
 * Idempotent for the same instance.
 */
export function sessionStart(
  fx: Fx,
  input: { bootstrapHash: string; instanceHash: string; req: StartRequest },
): StartResult | RunnerError {
  const s = fx.repo.sessionByBootstrapHash(input.bootstrapHash);
  if (!s) return unauthorized("unknown bootstrap token");
  const run = fx.repo.getRun(s.run_id);
  if (!run) return unauthorized("unknown run");
  if (run.state === "cancelled") return rerr(410, "run_cancelled", "the run was cancelled");
  if (s.state === "ended") return unauthorized("session ended");
  const now = fx.now();
  const exp = ms(s.bootstrap_expires_at);
  if (s.state === "assigned" && exp !== undefined && now > exp) return unauthorized("bootstrap token expired");
  let first = false;
  if (s.state === "assigned") {
    s.state = "active";
    s.instance_hash = input.instanceHash;
    s.started_at = iso(now);
    s.sandbox_id = input.req.sandbox_id;
    s.runner_version = input.req.runner_version;
    s.last_seen_at = iso(now);
    fx.repo.putSession(s);
    first = true;
  }
  if (s.state !== "active" || s.instance_hash !== input.instanceHash) {
    return unauthorized("bootstrap token is bound to another instance");
  }
  if (first) {
    transition(fx, s.run_id, { event: "session_started" }, (r) => {
      if (input.req.sandbox_id) r.sandbox_id = input.req.sandbox_id;
    });
    timeline(fx, s.run_id, s.session, "session.started", "session.started:" + s.task_id, {
      runner_version: s.runner_version ?? "",
    });
    fx.clearTimer(timerName.start(s.task_id));
    // The provider stops its own start watchdog (cloudflare-sandbox ack).
    fx.enqueue({
      dest: `provider:${s.backend ?? ""}`,
      kind: "provider.ack",
      lane: `task:${s.task_id}`,
      msgId: `pack:${s.task_id}`,
      payload: { providerId: s.backend ?? "", taskId: s.task_id, ...(s.worker_id ? { workerId: s.worker_id } : {}) },
    });
    armLiveness(fx, s);
    const assigned = ms(s.assigned_at) ?? now;
    fx.setTimer(timerName.deadline(s.task_id), "deadline", assigned + fx.cfg.maxSessionMs + 1, {
      runId: s.run_id,
      session: s.session,
    });
  }
  return { run: fx.repo.mustRun(s.run_id), session: s, first };
}

/** failStart: a configuration fault of the start config ends the session (422 configuration_error). */
export function failStart(fx: Fx, runId: string, n: number, message: string): RunnerError {
  const s = fx.repo.getSession(runId, n);
  if (s && s.state !== "ended") {
    s.failure_code = "invalid_configuration";
    endSession(fx, s, "setup_failed", "", { destroy: true });
    fx.repo.resetSessionInputs(runId, n);
    applySessionEnd(fx, s, {
      reason: "setup_failed",
      failure_code: "invalid_configuration",
      message,
      last_event_seq: s.event_ack,
    });
  }
  return rerr(422, "configuration_error", message);
}

// ---------- events ----------

interface WireEvent {
  seq: number;
  type: string;
  at?: string;
  turn_id?: string;
  data?: unknown;
}

/**
 * POST /session/events: append + every effect + the delivery decision, in
 * the caller's one transaction. Returns the ack.
 */
export function appendEvents(
  fx: Fx,
  runId: string,
  n: number,
  batch: { events: WireEvent[] | null },
): { ack: number } | RunnerError {
  const auth = authSession(fx, runId, n);
  if ("error" in auth) return auth;
  const s = auth;
  const events = (batch.events ?? []).map((e) => ({
    seq: e.seq,
    type: e.type,
    at: e.at ?? iso(fx.now()),
    ...(e.turn_id ? { turn_id: e.turn_id } : {}),
    ...(e.data !== undefined ? { data: e.data } : {}),
  }));
  let res: { ack: number; fresh: RunnerEvent[] };
  try {
    res = fx.repo.appendEvents(s, events);
  } catch (e) {
    if (e instanceof SeqGapError) return rerr(409, "seq_gap", "event sequence gap", e.ack);
    if (isOfError(e, "invalid_request")) return rerr(400, "bad_request", e.message);
    if (isOfError(e, "conflict")) return unauthorized("session ended");
    throw e;
  }
  fx.repo.putSession(s);
  if (res.fresh.length === 0) return { ack: res.ack };
  let wake = false;
  for (const e of res.fresh) {
    processEvent(fx, s, e);
    if (e.type === "session.ready" || e.type === "turn.ended") wake = true;
  }
  if (wake) deliver(fx, runId);
  return { ack: res.ack };
}

function str(v: unknown): string {
  return typeof v === "string" ? v : "";
}

function processEvent(fx: Fx, s: RunSession, e: RunnerEvent): void {
  const d = (e.data ?? {}) as Record<string, unknown>;
  switch (e.type) {
    case "session.ready": {
      const caps = Array.isArray(d.capabilities) ? (d.capabilities as unknown[]).map(String) : [];
      const cur = fx.repo.getSession(s.run_id, s.session)!;
      cur.capabilities = caps;
      if (!cur.ready_at) cur.ready_at = iso(fx.now());
      fx.repo.putSession(cur);
      Object.assign(s, cur);
      if (Array.isArray(d.repositories))
        resolveStartCommits(fx, s.run_id, d.repositories as { path: string; commit: string }[]);
      return;
    }
    case "turn.started": {
      if (!e.turn_id) return;
      const cur = fx.repo.getSession(s.run_id, s.session)!;
      cur.active_turn_id = e.turn_id;
      fx.repo.putSession(cur);
      Object.assign(s, cur);
      fx.repo.markAcceptedByTurn(s.run_id, e.turn_id);
      transition(fx, s.run_id, { event: "turn_started" });
      return;
    }
    case "input.accepted": {
      const ids = Array.isArray(d.message_ids) ? (d.message_ids as unknown[]).map(String) : [];
      fx.repo.markAccepted(s.run_id, ids);
      return;
    }
    case "message": {
      const role = str(d.role) === "system" ? "system" : "assistant";
      appendConversation(fx, s.run_id, role, str(d.content), e);
      return;
    }
    case "tool_call":
      appendConversation(fx, s.run_id, "tool_call", str(d.name), e);
      return;
    case "tool_result":
      appendConversation(fx, s.run_id, "tool_result", str(d.content), e);
      return;
    case "harness.session":
      updateRun(fx, s.run_id, (r) => {
        r.session_id = str(d.session_id);
      });
      return;
    case "usage":
      onUsage(fx, s, e, d);
      return;
    case "artifact":
      fx.repo.sql.exec(
        "INSERT OR REPLACE INTO run_uploads (run_id, session, kind, name, upload_id, at_ms) VALUES (?, ?, ?, ?, ?, ?)",
        s.run_id,
        s.session,
        "artifact",
        str(d.name),
        str(d.upload_id),
        fx.now(),
      );
      return;
    case "transcript.saved":
      fx.repo.sql.exec(
        "INSERT OR REPLACE INTO run_uploads (run_id, session, kind, name, upload_id, at_ms) VALUES (?, ?, ?, ?, ?, ?)",
        s.run_id,
        s.session,
        "transcript",
        "transcript",
        str(d.upload_id),
        fx.now(),
      );
      return;
    case "snapshot.saved":
      fx.repo.sql.exec(
        "INSERT OR REPLACE INTO snapshot_parts (run_id, session, part, upload_ids, bytes, at_ms) VALUES (?, ?, ?, ?, ?, ?)",
        s.run_id,
        s.session,
        str(d.part),
        JSON.stringify(Array.isArray(d.upload_ids) ? d.upload_ids : []),
        typeof d.bytes === "number" ? d.bytes : 0,
        fx.now(),
      );
      return;
    case "turn.ended": {
      if (!e.turn_id) return;
      fx.repo.finishTurnInputs(s.run_id, e.turn_id);
      const cur = fx.repo.getSession(s.run_id, s.session)!;
      if (cur.active_turn_id === e.turn_id) {
        cur.active_turn_id = "";
        fx.repo.putSession(cur);
      }
      Object.assign(s, cur);
      const outcome = str(d.outcome);
      transition(
        fx,
        s.run_id,
        {
          event: "turn_ended",
          outcome,
          failureCode: str(d.failure_code),
          hasOpenRequest: fx.repo.hasOpenRequest(s.run_id),
        },
        (r) => {
          r.turns = (r.turns ?? 0) + 1;
          if (str(d.result)) r.result = str(d.result);
          if (outcome === "error") setFailureMessage(r, str(d.message));
        },
      );
      return;
    }
  }
}

function appendConversation(fx: Fx, runId: string, role: string, content: string, e: RunnerEvent): void {
  fx.repo.appendMessage({
    id: `msg_${runId.slice(4, 20)}${e.session}x${e.seq}`,
    run_id: runId,
    role,
    content,
    ...(e.data !== undefined ? { data: e.data } : {}),
    created_at: e.at,
  });
  fx.wakeRun(runId);
}

/** onUsage: cumulative usage per session; the run gets the sum. Spend goes to DispatchDO (C5). */
function onUsage(fx: Fx, s: RunSession, e: RunnerEvent, d: Record<string, unknown>): void {
  if (d.cumulative !== true) return;
  const num = (v: unknown) => (typeof v === "number" ? v : 0);
  const cur = fx.repo.getSession(s.run_id, s.session)!;
  cur.usage = {
    input_tokens: num(d.input_tokens),
    output_tokens: num(d.output_tokens),
    cost_usd: num(d.cost_usd),
    duration_ms: num(d.duration_ms),
    ...(d.coverage === "partial" ? { coverage: "partial" } : {}),
  };
  fx.repo.putSession(cur);
  Object.assign(s, cur);
  const total = { input_tokens: 0, output_tokens: 0, cost_usd: 0, duration_ms: 0 } as Run["usage"];
  for (const x of fx.repo.listSessions(s.run_id)) {
    total.input_tokens += x.usage?.input_tokens ?? 0;
    total.output_tokens += x.usage?.output_tokens ?? 0;
    total.cost_usd += x.usage?.cost_usd ?? 0;
    total.duration_ms += x.usage?.duration_ms ?? 0;
    if (x.usage?.coverage === "partial") total.coverage = "partial";
  }
  const run = updateRun(fx, s.run_id, (r) => {
    r.usage = total;
  });
  fx.enqueue({
    dest: `do:DISPATCH:team:${fx.teamId}|dispatch`,
    kind: "dispatch.usage",
    msgId: `usage:${s.task_id}:${e.seq}`,
    payload: {
      runId: s.run_id,
      session: s.session,
      seq: e.seq,
      factoryId: run.factory_id,
      runCreatedMs: ms(run.created_at) ?? fx.now(),
      costUsd: cur.usage.cost_usd,
      rootRunId: rootOf(fx, run),
      workDO: fx.doName,
    } as never,
  });
}

function rootOf(fx: Fx, r: Run): string {
  let cur = r;
  for (let i = 0; i < 64 && cur.parent_run_id; i++) {
    const p = fx.repo.getRun(cur.parent_run_id);
    if (!p) break;
    cur = p;
  }
  return cur.id;
}

function resolveStartCommits(fx: Fx, runId: string, repos: { path: string; commit: string }[]): void {
  const raw = fx.repo.getRunState(runId, "start_commits");
  if (!raw) return;
  const list = JSON.parse(raw) as { path: string; ref: string; resolved?: boolean }[];
  let changed = false;
  for (const it of list) {
    if (it.resolved) continue;
    for (const rc of repos) {
      if (rc.path === it.path && rc.commit) {
        it.ref = rc.commit;
        it.resolved = true;
        changed = true;
      }
    }
  }
  if (changed) fx.repo.setRunState(runId, "start_commits", JSON.stringify(list));
}

// ---------- commands ----------

export function pollCommands(
  fx: Fx,
  runId: string,
  n: number,
  after: number,
): { commands: RunnerCommand[] } | RunnerError {
  const auth = authSession(fx, runId, n);
  if ("error" in auth) return auth;
  if (after > 0 && fx.repo.ackCommands(auth, after)) fx.repo.putSession(auth);
  return { commands: fx.repo.listCommands(runId, n, after) };
}

/** Appends a command to the live session (EnqueueCommand). */
export function enqueueCommand(fx: Fx, s: RunSession, type: string, data: unknown): RunnerCommand {
  const c = fx.repo.appendCommand(s, type, data);
  fx.repo.putSession(s);
  fx.wakeCommands(s.run_id, s.session);
  return c;
}

// ---------- end ----------

const END_REASONS = new Set(["idle", "stopped", "cancelled", "setup_failed", "deadline", "fatal"]);

/** POST /session/end (runnerapi/end.go + Dispatcher.SessionEnded) in one transaction (N2). */
export function endSessionByRunner(
  fx: Fx,
  runId: string,
  n: number,
  req: EndRequest,
): Record<string, never> | RunnerError {
  const auth = authSession(fx, runId, n);
  if ("error" in auth) return auth;
  if (!END_REASONS.has(req.reason)) return rerr(400, "bad_request", `invalid end reason ${JSON.stringify(req.reason)}`);
  if (typeof req.last_event_seq !== "number" || req.last_event_seq < 0)
    return rerr(400, "bad_request", "invalid last_event_seq");
  const s = auth;
  if (req.last_event_seq > s.event_ack) return rerr(409, "seq_gap", "event sequence gap", s.event_ack);
  if (req.failure_code) s.failure_code = runnerFailureCode(req.failure_code);
  endSession(fx, s, req.reason, "", { destroy: true });
  fx.repo.resetSessionInputs(runId, n);
  applySessionEnd(fx, s, req);
  return {};
}

/** Dispatcher.applySessionEnd. */
export function applySessionEnd(
  fx: Fx,
  s: RunSession,
  req: Pick<EndRequest, "reason" | "failure_code" | "message" | "last_event_seq">,
): void {
  const lost = (code: FailureCode) =>
    transition(fx, s.run_id, {
      event: "session_lost",
      failureCode: code,
      priorRetries: priorRetries(fx.repo.listSessions(s.run_id), s.session),
    });
  switch (req.reason) {
    case "setup_failed":
      transition(fx, s.run_id, { event: "setup_failed", failureCode: req.failure_code ?? "" }, (r) =>
        setFailureMessage(r, req.message),
      );
      return;
    case "fatal":
      lost("agent_process_failed");
      return;
    case "idle":
    case "stopped":
    case "deadline": {
      const r = fx.repo.mustRun(s.run_id);
      if (r.state === "running" || r.state === "claimed") lost("runner_lost");
      return;
    }
  }
}
