// Sessions of a run: the slot requests to DispatchDO, the end of a session
// (lease release, timers, task destroy) and its cancel. None of these
// functions change the run state; lifecycle.ts does that.
import { dest } from "../../lib/outbox/dest";
import { computeSeconds, iso, ms } from "../../engine/sessions";
import { sessionEndedRow } from "../../engine/timeline";
import type { Run, RunSession } from "../../engine/types";
import { timerName, type Fx } from "./fx";
import { writeTimeline } from "./projections";

// ---------- slots (DispatchDO) ----------

/** dispatch.request for the next session of a queued run (03 §7.2 step 1). */
export function requestSlot(fx: Fx, r: Run): void {
  const latest = fx.repo.latestSession(r.id);
  if (latest && latest.state !== "ended") return;
  // After a start that failed for capacity, wait before the next request
  // (the Go dispatcher retried on its next poll, not at once).
  const after = Number(fx.repo.getMeta(`slot_after:${r.id}`) ?? "0");
  if (after > fx.now()) {
    fx.setTimer(timerName.requeue(r.id), "requeue", after, { runId: r.id });
    return;
  }
  const n = latest ? latest.session + 1 : 1;
  const queuedMs = ms(r.queued_at) ?? ms(r.created_at) ?? fx.now();
  fx.enqueue({
    dest: dest.dispatch(fx.teamId),
    kind: "dispatch.request",
    lane: "dispatch",
    msgId: `dreq:${r.id}:${n}:${queuedMs}`,
    payload: {
      runId: r.id,
      session: n,
      factoryId: r.factory_id,
      target: fx.target(r.config?.worker_host),
      queuedMs,
      runCreatedMs: ms(r.created_at) ?? fx.now(),
      restricted: !!r.config?.network_policy?.restricted,
      workDO: fx.doName,
    },
  });
}

export function cancelSlotRequest(fx: Fx, r: Run): void {
  const latest = fx.repo.latestSession(r.id);
  const n = latest ? latest.session + 1 : 1;
  fx.enqueue({
    dest: dest.dispatch(fx.teamId),
    kind: "dispatch.cancel",
    lane: "dispatch",
    msgId: `dcancel:${r.id}:${n}:${fx.now()}`,
    payload: { runId: r.id, session: n },
  });
}

function providerDest(s: Pick<RunSession, "backend">): string {
  return `provider:${s.backend ?? ""}`;
}

export function enqueueProvider(
  fx: Fx,
  s: RunSession,
  kind: "provider.cancel" | "provider.destroy",
  graceSeconds = 0,
): void {
  const base = { providerId: s.backend ?? "", taskId: s.task_id, ...(s.worker_id ? { workerId: s.worker_id } : {}) };
  if (kind === "provider.cancel") {
    fx.enqueue({
      dest: providerDest(s),
      kind,
      lane: `task:${s.task_id}`,
      msgId: `pcancel:${s.task_id}:${fx.now()}`,
      payload: { ...base, graceSeconds },
    });
  } else {
    fx.enqueue({
      dest: providerDest(s),
      kind,
      lane: `task:${s.task_id}`,
      msgId: `pdestroy:${s.task_id}`,
      payload: base,
    });
  }
}

// ---------- sessions ----------

/** Dispatcher.endSession: false when the session was already ended. Releases the lease. */
export function endSession(
  fx: Fx,
  s: RunSession,
  reason: string,
  sandboxReason: string,
  opts: { destroy?: boolean } = {},
): boolean {
  if (s.state === "ended") return false;
  const now = fx.now();
  s.state = "ended";
  s.end_reason = reason;
  if (sandboxReason) s.sandbox_reason = sandboxReason;
  s.ended_at = iso(now);
  s.compute_seconds = computeSeconds(s, now);
  s.active_turn_id = "";
  fx.repo.putSession(s);
  writeTimeline(fx, s.run_id, [sessionEndedRow(s.run_id, s.session, reason, sandboxReason, now)]);
  for (const t of [timerName.start(s.task_id), timerName.live(s.task_id), timerName.deadline(s.task_id)])
    fx.clearTimer(t);
  fx.enqueue({
    dest: dest.dispatch(fx.teamId),
    kind: "dispatch.release",
    lane: "dispatch",
    msgId: `drel:${s.task_id}`,
    payload: { runId: s.run_id, session: s.session, computeSeconds: s.compute_seconds ?? 0, endReason: reason },
  });
  if (opts.destroy) enqueueProvider(fx, s, "provider.destroy");
  fx.wakeCommands(s.run_id, s.session);
  return true;
}

/** Dispatcher.cancelSession: session.cancel, and a task cancel before session.ready. */
export function cancelSession(fx: Fx, s: RunSession): void {
  if (s.state === "ended") return;
  fx.repo.appendCommand(s, "session.cancel", null);
  fx.repo.putSession(s);
  fx.wakeCommands(s.run_id, s.session);
  if (s.state === "assigned" || !s.ready_at) {
    enqueueProvider(fx, s, "provider.cancel", Math.round(fx.cfg.stopGraceMs / 1000));
    endSession(fx, s, "cancelled", "", { destroy: true });
  }
}

/** Dispatcher.StopSession. */
export function stopSession(fx: Fx, runId: string): void {
  const latest = fx.repo.latestSession(runId);
  if (latest && latest.state !== "ended") cancelSession(fx, latest);
}
