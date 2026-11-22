// checkSilent and enforceDeadline of internal/dispatch/liveness.go and
// deadline.go, rewritten as pure decisions over (session, now, status).
import type { TaskStatus } from "@openfactory/sandbox";
import { ms } from "./sessions";
import type { RunSession } from "./types";

export const DEFAULT_LOST_AFTER_MS = 90_000;

export type LivenessAction =
  /** Nothing to do now; check again at `recheckAtMs`. */
  | { kind: "wait"; recheckAtMs: number }
  /** Ask the provider for the task status, then call decideSilent again. */
  | { kind: "ask_status" }
  /** End the session as lost. `cancel`: cancel the task first. Empty code: no run event. */
  | { kind: "lose"; reason: string; code: string; cancel: boolean; useReasonMap: boolean };

export interface LivenessOptions {
  lostAfterMs: number;
}

/**
 * The start-timeout part: an assigned session past its bootstrap expiry loses
 * its task with sandbox_create_failed.
 */
export function decideStartTimeout(s: Pick<RunSession, "state" | "bootstrap_expires_at">, nowMs: number): LivenessAction | undefined {
  const exp = ms(s.bootstrap_expires_at);
  if (s.state === "assigned" && exp !== undefined && nowMs > exp) {
    return { kind: "lose", reason: "deadline_exceeded", code: "sandbox_create_failed", cancel: true, useReasonMap: false };
  }
  return undefined;
}

/**
 * decideSilent. `status` is undefined when the provider was not asked yet,
 * "not_found" when the provider does not know the task, "unavailable" when it
 * did not answer (treated as running, as Go does for ErrUnavailable).
 */
export function decideSilent(
  s: Pick<RunSession, "state" | "bootstrap_expires_at" | "last_seen_at">,
  nowMs: number,
  opts: LivenessOptions,
  status?: TaskStatus | "not_found" | "unavailable" | { error: string },
): LivenessAction {
  const start = decideStartTimeout(s, nowMs);
  if (start) return start;
  const last = ms(s.last_seen_at) ?? nowMs;
  const silentFor = nowMs - last;
  if (silentFor <= opts.lostAfterMs) return { kind: "wait", recheckAtMs: last + opts.lostAfterMs + 1 };
  if (status === undefined) return { kind: "ask_status" };
  const assigned = s.state === "assigned";
  if (status === "not_found") {
    return { kind: "lose", reason: "unknown", code: "sandbox_lost", cancel: false, useReasonMap: false };
  }
  const st: TaskStatus | undefined = status === "unavailable" ? { taskId: "", state: "running" } : "state" in status ? status : undefined;
  if (st && st.state === "exited") {
    return { kind: "lose", reason: st.reason ?? "unknown", code: "", cancel: false, useReasonMap: true };
  }
  if (assigned) {
    // An assigned session is lost only on exited / not found / start timeout.
    const exp = ms(s.bootstrap_expires_at) ?? nowMs + opts.lostAfterMs;
    return { kind: "wait", recheckAtMs: Math.min(exp + 1, nowMs + opts.lostAfterMs) };
  }
  if (silentFor > 2 * opts.lostAfterMs) {
    return { kind: "lose", reason: "unknown", code: "runner_lost", cancel: true, useReasonMap: false };
  }
  return { kind: "wait", recheckAtMs: last + 2 * opts.lostAfterMs + 1 };
}

export type DeadlineAction =
  | { kind: "none" }
  | { kind: "wait"; atMs: number }
  /** Send session.stop {reason: deadline, snapshot: true}; apply session_deadline when a turn is active. */
  | { kind: "stop"; applyDeadlineEvent: boolean }
  /** Cancel the task and lose the session (deadline_exceeded); apply the event when a turn is active. */
  | { kind: "lose"; applyEvent: boolean };

export interface DeadlineOptions {
  maxSessionMs: number;
  stopGraceMs: number;
  lostAfterMs: number;
}

/** enforceDeadline: `stopSentMs` replaces the Go KV marker. */
export function decideDeadline(
  s: Pick<RunSession, "state" | "assigned_at" | "active_turn_id">,
  stopSentMs: number | undefined,
  nowMs: number,
  o: DeadlineOptions,
): DeadlineAction {
  if (s.state !== "active") return { kind: "none" };
  const assigned = ms(s.assigned_at) ?? nowMs;
  const age = nowMs - assigned;
  if (age <= o.maxSessionMs) return { kind: "wait", atMs: assigned + o.maxSessionMs + 1 };
  if (stopSentMs === undefined) return { kind: "stop", applyDeadlineEvent: !!s.active_turn_id };
  if (age <= o.maxSessionMs + o.stopGraceMs + o.lostAfterMs) {
    return { kind: "wait", atMs: assigned + o.maxSessionMs + o.stopGraceMs + o.lostAfterMs + 1 };
  }
  return { kind: "lose", applyEvent: !!s.active_turn_id };
}
