// The run state machine. Port of internal/runstate. Checked against
// schemas/testdata/runstate-transitions.json (generated from the Go code).

import { OfError } from "./errors";
import { lookupFailure, MAX_RETRY_SESSIONS, runnerFailureCode, type FailureCode } from "./failure";
import type { RunState } from "./run";

/** Something that happens to a run. */
export const RUN_EVENTS = [
  "assigned", // a dispatcher gave the task to a backend
  "config_fault", // the control plane found a configuration fault before the assignment
  "queue_timeout", // the queue timeout passed
  "session_started", // the runner called session start
  "setup_failed", // the setup of the runner failed
  "turn_started", // the runner reported the start of a turn
  "turn_ended", // the runner reported the end of a turn
  "session_lost", // the sandbox or the runner is gone without a session end
  "session_deadline", // the maximum session time passed
  "message", // a message arrived in the queue of the run
  "cancel", // a cancel request
  "spend_limit", // a turn end found the spend over its limit
] as const;

export type RunEvent = (typeof RUN_EVENTS)[number];

/** The nine states of the run model (internal/runstate Known). */
export const RUN_STATES = [
  "queued",
  "pending",
  "claimed",
  "running",
  "blocked",
  "succeeded",
  "failed",
  "error",
  "cancelled",
] as const satisfies readonly RunState[];

/** The finished states. A finished run can continue with a new message. */
export const FINISHED_RUN_STATES = ["succeeded", "failed", "error", "cancelled"] as const satisfies readonly RunState[];

/** Outcome of a turn (runnerproto.TurnOutcome). */
export type TurnOutcome = "done" | "interrupted" | "needs_input" | "error";

/** Blocked reason: the run has an open request to a person. */
export const BLOCKED_HUMAN_REQUEST = "human_request";
/** Blocked reason: the harness stopped and asked for input. */
export const BLOCKED_HARNESS_NEEDS_INPUT = "harness_needs_input";

/** One event with the facts that the decision needs. */
export interface RunStateInput {
  event: RunEvent | string;
  /** The outcome of the turn, for turn_ended. */
  outcome?: TurnOutcome | string;
  /** For config_fault, setup_failed, session_lost, and turn_ended with the outcome error. */
  failureCode?: string;
  /** The run has an open request to a person, for turn_ended. */
  hasOpenRequest?: boolean;
  /** A session of the run is alive, for message. */
  sandboxRunning?: boolean;
  /** Sessions directly before this one that ended with a retry, for session_lost. */
  priorRetries?: number;
}

/** The decision. */
export interface RunStateResult {
  /** The state after the event. */
  state: RunState;
  /** False when the run stays in the same state and nothing is written. */
  changed: boolean;
  /** Set when state is blocked. */
  blockedReason?: string;
  /** Set when state is failed or error, and for a retry. */
  failureCode?: FailureCode;
  /** True when the run goes to queued for a new session after a lost session. */
  retry?: boolean;
}

/** The event cannot happen in the state (runstate.ErrInvalidTransition). */
export class InvalidTransitionError extends OfError {
  readonly state: string;
  readonly event: string;
  constructor(state: string, event: string) {
    super("conflict", `runstate: invalid transition: event "${event}" in state "${state}"`, {
      reason: "invalid_transition",
    });
    this.state = state;
    this.event = event;
  }
}

/** Reports whether s is one of the nine states of a run. */
export function isKnownRunState(s: string): s is (typeof RUN_STATES)[number] {
  return (RUN_STATES as readonly string[]).includes(s);
}

/** Reports whether s is a finished state. */
export function isFinishedRunState(s: string): boolean {
  return (FINISHED_RUN_STATES as readonly string[]).includes(s);
}

const stay = (state: RunState): RunStateResult => ({ state, changed: false });
const move = (state: RunState): RunStateResult => ({ state, changed: true });
const moveFail = (state: RunState, failureCode: FailureCode): RunStateResult => ({ state, changed: true, failureCode });

/**
 * The result of the event in the state cur, or null when the event cannot
 * happen in the state.
 */
export function tryNextRunState(cur: string, input: RunStateInput): RunStateResult | null {
  if (!isKnownRunState(cur)) return null;
  const fc = input.failureCode ?? "";
  switch (input.event) {
    case "assigned":
      return cur === "queued" ? move("pending") : null;

    case "config_fault": {
      if (cur !== "queued") return null;
      const info = lookupFailure(fc);
      return moveFail("failed", info && info.class === "user" ? info.code : "invalid_configuration");
    }

    case "queue_timeout":
      return cur === "queued" ? moveFail("error", "infrastructure_timeout") : stay(cur);

    case "session_started":
      return cur === "pending" ? move("claimed") : null;

    case "setup_failed": {
      if (cur !== "claimed") return null;
      switch (fc) {
        case "environment_setup_failed":
        case "harness_auth_failed":
        case "invalid_configuration":
          return moveFail("failed", fc);
      }
      return moveFail("failed", "environment_setup_failed");
    }

    case "turn_started":
      switch (cur) {
        case "claimed":
        case "succeeded":
        case "blocked":
        case "failed":
          return move("running");
        case "running":
          return stay(cur);
      }
      return null;

    case "turn_ended":
      if (cur !== "running") return null;
      switch (input.outcome) {
        case "done":
        case "interrupted":
          if (input.hasOpenRequest) return { state: "blocked", changed: true, blockedReason: BLOCKED_HUMAN_REQUEST };
          return move("succeeded");
        case "needs_input":
          return { state: "blocked", changed: true, blockedReason: BLOCKED_HARNESS_NEEDS_INPUT };
        case "error":
          return moveFail("failed", runnerFailureCode(fc));
      }
      return null;

    case "session_lost":
      switch (cur) {
        case "pending":
        case "claimed":
        case "running": {
          const info = lookupFailure(fc) ?? lookupFailure("internal_error")!;
          if (info.retry && (input.priorRetries ?? 0) < MAX_RETRY_SESSIONS - 1) {
            return { state: "queued", changed: true, failureCode: info.code, retry: true };
          }
          return moveFail(info.class === "user" ? "failed" : "error", info.code);
        }
      }
      return stay(cur);

    case "session_deadline":
      return cur === "running" ? moveFail("error", "infrastructure_timeout") : stay(cur);

    case "message":
      switch (cur) {
        case "succeeded":
        case "blocked":
        case "failed":
        case "error":
        case "cancelled":
          if (!input.sandboxRunning) return move("queued");
      }
      return stay(cur);

    case "spend_limit":
      switch (cur) {
        case "error":
        case "cancelled":
          return stay(cur);
      }
      return moveFail("error", "spend_limit_exceeded");

    case "cancel":
      switch (cur) {
        case "queued":
        case "pending":
        case "claimed":
        case "running":
        case "blocked":
          return move("cancelled");
      }
      return stay(cur);
  }
  return null;
}

/** The result of the event in the state cur (runstate.Next). Throws InvalidTransitionError. */
export function nextRunState(cur: string, input: RunStateInput): RunStateResult {
  const r = tryNextRunState(cur, input);
  if (r === null) throw new InvalidTransitionError(cur, input.event);
  return r;
}

/** Where an event leads from one state, for the default input of the event. */
export type TransitionTarget =
  { kind: "move"; to: RunState } | { kind: "stay" } | { kind: "depends"; to: readonly RunState[]; on: string };

/**
 * The transition table of the run state machine: for each event, the states
 * in which it can happen and where it leads. A state that is not listed for an
 * event is an invalid transition. tryNextRunState is the implementation; a
 * test checks that both agree.
 */
export const RUN_TRANSITIONS: Readonly<
  Record<RunEvent, Partial<Record<(typeof RUN_STATES)[number], TransitionTarget>>>
> = {
  assigned: { queued: { kind: "move", to: "pending" } },
  config_fault: { queued: { kind: "move", to: "failed" } },
  queue_timeout: {
    queued: { kind: "move", to: "error" },
    pending: { kind: "stay" },
    claimed: { kind: "stay" },
    running: { kind: "stay" },
    blocked: { kind: "stay" },
    succeeded: { kind: "stay" },
    failed: { kind: "stay" },
    error: { kind: "stay" },
    cancelled: { kind: "stay" },
  },
  session_started: { pending: { kind: "move", to: "claimed" } },
  setup_failed: { claimed: { kind: "move", to: "failed" } },
  turn_started: {
    claimed: { kind: "move", to: "running" },
    succeeded: { kind: "move", to: "running" },
    blocked: { kind: "move", to: "running" },
    failed: { kind: "move", to: "running" },
    running: { kind: "stay" },
  },
  turn_ended: { running: { kind: "depends", to: ["succeeded", "blocked", "failed"], on: "outcome, hasOpenRequest" } },
  session_lost: {
    pending: { kind: "depends", to: ["queued", "failed", "error"], on: "failureCode, priorRetries" },
    claimed: { kind: "depends", to: ["queued", "failed", "error"], on: "failureCode, priorRetries" },
    running: { kind: "depends", to: ["queued", "failed", "error"], on: "failureCode, priorRetries" },
    queued: { kind: "stay" },
    blocked: { kind: "stay" },
    succeeded: { kind: "stay" },
    failed: { kind: "stay" },
    error: { kind: "stay" },
    cancelled: { kind: "stay" },
  },
  session_deadline: {
    running: { kind: "move", to: "error" },
    queued: { kind: "stay" },
    pending: { kind: "stay" },
    claimed: { kind: "stay" },
    blocked: { kind: "stay" },
    succeeded: { kind: "stay" },
    failed: { kind: "stay" },
    error: { kind: "stay" },
    cancelled: { kind: "stay" },
  },
  message: {
    succeeded: { kind: "depends", to: ["queued", "succeeded"], on: "sandboxRunning" },
    blocked: { kind: "depends", to: ["queued", "blocked"], on: "sandboxRunning" },
    failed: { kind: "depends", to: ["queued", "failed"], on: "sandboxRunning" },
    error: { kind: "depends", to: ["queued", "error"], on: "sandboxRunning" },
    cancelled: { kind: "depends", to: ["queued", "cancelled"], on: "sandboxRunning" },
    queued: { kind: "stay" },
    pending: { kind: "stay" },
    claimed: { kind: "stay" },
    running: { kind: "stay" },
  },
  cancel: {
    queued: { kind: "move", to: "cancelled" },
    pending: { kind: "move", to: "cancelled" },
    claimed: { kind: "move", to: "cancelled" },
    running: { kind: "move", to: "cancelled" },
    blocked: { kind: "move", to: "cancelled" },
    succeeded: { kind: "stay" },
    failed: { kind: "stay" },
    error: { kind: "stay" },
    cancelled: { kind: "stay" },
  },
  spend_limit: {
    queued: { kind: "move", to: "error" },
    pending: { kind: "move", to: "error" },
    claimed: { kind: "move", to: "error" },
    running: { kind: "move", to: "error" },
    blocked: { kind: "move", to: "error" },
    succeeded: { kind: "move", to: "error" },
    failed: { kind: "move", to: "error" },
    error: { kind: "stay" },
    cancelled: { kind: "stay" },
  },
};
