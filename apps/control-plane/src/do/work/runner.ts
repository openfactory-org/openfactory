// The runner protocol inside WorkDO (internal/runnerapi + dispatch hooks).
// Each function is one transaction: events are acknowledged and their
// effects applied together. The Worker verifies tokens and holds
// long-polls; WorkDO decides.
import type { EndRequest, StartRequest } from "@openfactory/runner-proto";
import { isOfError, runnerFailureCode, type FailureCode } from "@openfactory/core";
import { iso, ms, setFailureMessage } from "../../engine/sessions";
import { priorRetries } from "../../engine/sessions";
import type { Run, RunMessage, RunnerCommand, RunnerEvent, RunSession } from "../../engine/types";
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
    // The provider forgets the bootstrap token of the task (providers with ack only).
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
  if (res.fresh.length === 0) return { ack: res.ack };
  const b = new BatchSession(fx, s);
  let wake = false;
  for (const e of res.fresh) {
    processEvent(b, e);
    if (e.type === "session.ready" || e.type === "turn.ended") wake = true;
  }
  b.flush();
  if (wake) deliver(fx, runId);
  return { ack: res.ack };
}

/**
 * The session of one event batch. Changes stay in memory and are written once
 * at the end of the batch, or before an engine step that reads or writes the
 * session itself; after such a step the session is read again.
 */
class BatchSession {
  /** The ack and the last-seen time of the new events are not written yet. */
  private dirty = true;

  constructor(
    readonly fx: Fx,
    readonly s: RunSession,
  ) {}

  /** Marks a change of s for the next write. */
  touch(): void {
    this.dirty = true;
  }

  flush(): void {
    if (!this.dirty) return;
    this.fx.repo.putSession(this.s);
    this.dirty = false;
  }

  /** Runs an engine step that reads the session from storage (transition, delivery). */
  step(fn: () => void): void {
    this.flush();
    fn();
    const cur = this.fx.repo.getSession(this.s.run_id, this.s.session);
    if (cur) Object.assign(this.s, cur);
  }
}

function str(v: unknown): string {
  return typeof v === "string" ? v : "";
}

function processEvent(b: BatchSession, e: RunnerEvent): void {
  const { fx, s } = b;
  const d = (e.data ?? {}) as Record<string, unknown>;
  switch (e.type) {
    case "session.ready": {
      s.capabilities = Array.isArray(d.capabilities) ? (d.capabilities as unknown[]).map(String) : [];
      if (!s.ready_at) s.ready_at = iso(fx.now());
      b.touch();
      if (Array.isArray(d.repositories))
        resolveStartCommits(fx, s.run_id, d.repositories as { path: string; commit: string }[]);
      return;
    }
    case "turn.started": {
      if (!e.turn_id) return;
      s.active_turn_id = e.turn_id;
      b.touch();
      fx.repo.markAcceptedByTurn(s.run_id, e.turn_id);
      b.step(() => transition(fx, s.run_id, { event: "turn_started" }));
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
      onUsage(b, e, d);
      return;
    case "snapshot.saved":
      fx.repo.putSnapshotPart(
        s.run_id,
        s.session,
        str(d.part),
        Array.isArray(d.upload_ids) ? d.upload_ids : [],
        typeof d.bytes === "number" ? d.bytes : 0,
      );
      return;
    case "turn.ended": {
      if (!e.turn_id) return;
      fx.repo.finishTurnInputs(s.run_id, e.turn_id);
      if (s.active_turn_id === e.turn_id) {
        s.active_turn_id = "";
        b.touch();
      }
      const outcome = str(d.outcome);
      const hasOpenRequest = fx.repo.hasOpenRequest(s.run_id);
      b.step(() =>
        transition(
          fx,
          s.run_id,
          { event: "turn_ended", outcome, failureCode: str(d.failure_code), hasOpenRequest },
          (r) => {
            r.turns = (r.turns ?? 0) + 1;
            if (str(d.result)) r.result = str(d.result);
            if (outcome === "error") setFailureMessage(r, str(d.message));
          },
        ),
      );
      return;
    }
  }
}

function appendConversation(fx: Fx, runId: string, role: RunMessage["role"], content: string, e: RunnerEvent): void {
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

/** onUsage: cumulative usage per session; the run gets the sum. Spend goes to DispatchDO. */
function onUsage(b: BatchSession, e: RunnerEvent, d: Record<string, unknown>): void {
  if (d.cumulative !== true) return;
  const { fx, s } = b;
  const num = (v: unknown) => (typeof v === "number" ? v : 0);
  const usage: NonNullable<RunSession["usage"]> = {
    input_tokens: num(d.input_tokens),
    output_tokens: num(d.output_tokens),
    cost_usd: num(d.cost_usd),
    duration_ms: num(d.duration_ms),
    ...(d.coverage === "partial" ? { coverage: "partial" } : {}),
  };
  s.usage = usage;
  b.touch();
  const total = { input_tokens: 0, output_tokens: 0, cost_usd: 0, duration_ms: 0 } as Run["usage"];
  // The stored row of this session is not written yet: its usage comes from s.
  for (const x of fx.repo.listSessions(s.run_id)) {
    const u = x.session === s.session ? usage : x.usage;
    total.input_tokens += u?.input_tokens ?? 0;
    total.output_tokens += u?.output_tokens ?? 0;
    total.cost_usd += u?.cost_usd ?? 0;
    total.duration_ms += u?.duration_ms ?? 0;
    if (u?.coverage === "partial") total.coverage = "partial";
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
      costUsd: usage.cost_usd,
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
