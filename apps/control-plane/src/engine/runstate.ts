// Port of internal/runstate/runstate.go: the run state machine. Pure.
// Checked against schemas/testdata/runstate-transitions.json.
import { lookupFailure, MAX_RETRY_SESSIONS, runnerFailureCode, type FailureCode } from "@openfactory/core";
import type { RunState } from "./types";

export type RunEvent =
  | "assigned"
  | "config_fault"
  | "queue_timeout"
  | "session_started"
  | "setup_failed"
  | "turn_started"
  | "turn_ended"
  | "session_lost"
  | "session_deadline"
  | "message"
  | "cancel"
  | "spend_limit";

export const BLOCKED_HUMAN_REQUEST = "human_request";
export const BLOCKED_HARNESS_NEEDS_INPUT = "harness_needs_input";

export type TurnOutcome = "done" | "error" | "interrupted" | "needs_input";

export interface RunInput {
  event: RunEvent;
  outcome?: TurnOutcome | string;
  failureCode?: string;
  hasOpenRequest?: boolean;
  sandboxRunning?: boolean;
  priorRetries?: number;
}

export interface RunResult {
  state: RunState;
  changed: boolean;
  blockedReason?: string;
  failureCode?: FailureCode;
  retry?: boolean;
}

export class InvalidTransitionError extends Error {
  override readonly name = "InvalidTransitionError";
  constructor(
    readonly state: string,
    readonly event: string,
  ) {
    super(`runstate: invalid transition: event "${event}" in state "${state}"`);
  }
}

const KNOWN: ReadonlySet<string> = new Set([
  "queued",
  "pending",
  "claimed",
  "running",
  "blocked",
  "succeeded",
  "failed",
  "error",
  "cancelled",
]);

/** runstate.Known: one of the nine states of a run. */
export function known(s: string): boolean {
  return KNOWN.has(s);
}

/** runstate.Finished */
export function finished(s: string): boolean {
  return s === "succeeded" || s === "failed" || s === "error" || s === "cancelled";
}

const stay = (s: RunState): RunResult => ({ state: s, changed: false });
const move = (s: RunState): RunResult => ({ state: s, changed: true });
const moveFail = (s: RunState, code: FailureCode): RunResult => ({ state: s, changed: true, failureCode: code });

/**
 * runstate.Next. Throws InvalidTransitionError for an event that cannot
 * happen in the state.
 */
export function next(cur: string, input: RunInput): RunResult {
  const ev = input.event;
  if (!known(cur)) throw new InvalidTransitionError(cur, ev);
  const state = cur as RunState;
  switch (ev) {
    case "assigned":
      if (state === "queued") return move("pending");
      break;
    case "config_fault": {
      if (state !== "queued") break;
      let code: FailureCode = "invalid_configuration";
      const info = lookupFailure(input.failureCode ?? "");
      if (info && info.class === "user") code = info.code;
      return moveFail("failed", code);
    }
    case "queue_timeout":
      if (state === "queued") return moveFail("error", "infrastructure_timeout");
      return stay(state);
    case "session_started":
      if (state === "pending") return move("claimed");
      break;
    case "setup_failed": {
      if (state !== "claimed") break;
      let code: FailureCode = "environment_setup_failed";
      switch (input.failureCode) {
        case "environment_setup_failed":
        case "harness_auth_failed":
        case "invalid_configuration":
          code = input.failureCode;
      }
      return moveFail("failed", code);
    }
    case "turn_started":
      switch (state) {
        case "claimed":
        case "succeeded":
        case "blocked":
        case "failed":
          return move("running");
        case "running":
          return stay(state);
      }
      break;
    case "turn_ended":
      if (state !== "running") break;
      switch (input.outcome) {
        case "done":
        case "interrupted":
          if (input.hasOpenRequest) return { state: "blocked", changed: true, blockedReason: BLOCKED_HUMAN_REQUEST };
          return move("succeeded");
        case "needs_input":
          return { state: "blocked", changed: true, blockedReason: BLOCKED_HARNESS_NEEDS_INPUT };
        case "error":
          return moveFail("failed", runnerFailureCode(input.failureCode ?? ""));
      }
      break;
    case "session_lost":
      switch (state) {
        case "pending":
        case "claimed":
        case "running": {
          let info = lookupFailure(input.failureCode ?? "");
          if (!info) info = lookupFailure("internal_error")!;
          if (info.retry && (input.priorRetries ?? 0) < MAX_RETRY_SESSIONS - 1) {
            return { state: "queued", changed: true, failureCode: info.code, retry: true };
          }
          if (info.class === "user") return moveFail("failed", info.code);
          return moveFail("error", info.code);
        }
      }
      return stay(state);
    case "session_deadline":
      if (state === "running") return moveFail("error", "infrastructure_timeout");
      return stay(state);
    case "message":
      switch (state) {
        case "succeeded":
        case "blocked":
        case "failed":
        case "error":
        case "cancelled":
          if (!input.sandboxRunning) return move("queued");
      }
      return stay(state);
    case "spend_limit":
      switch (state) {
        case "queued":
        case "pending":
        case "claimed":
        case "running":
        case "blocked":
        case "succeeded":
        case "failed":
          return moveFail("error", "spend_limit_exceeded");
      }
      return stay(state);
    case "cancel":
      switch (state) {
        case "queued":
        case "pending":
        case "claimed":
        case "running":
        case "blocked":
          return move("cancelled");
      }
      return stay(state);
  }
  throw new InvalidTransitionError(cur, ev);
}
