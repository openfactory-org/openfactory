import { describe, expect, it } from "vitest";
import vectors from "../../../schemas/testdata/runstate-transitions.json";
import {
  InvalidTransitionError,
  isFinishedRunState,
  isKnownRunState,
  isOfError,
  nextRunState,
  RUN_EVENTS,
  RUN_STATES,
  RUN_TRANSITIONS,
  tryNextRunState,
  type RunStateInput,
  type RunStateResult,
} from "../src/index";

interface VectorCase {
  state: string;
  input: {
    event: string;
    outcome?: string;
    failure_code?: string;
    has_open_request?: boolean;
    sandbox_running?: boolean;
    prior_retries?: number;
  };
  want: {
    state?: string;
    changed: boolean;
    invalid?: boolean;
    blocked_reason?: string;
    failure_code?: string;
    retry?: boolean;
  };
}

function toInput(i: VectorCase["input"]): RunStateInput {
  const out: RunStateInput = { event: i.event };
  if (i.outcome !== undefined) out.outcome = i.outcome;
  if (i.failure_code !== undefined) out.failureCode = i.failure_code;
  if (i.has_open_request !== undefined) out.hasOpenRequest = i.has_open_request;
  if (i.sandbox_running !== undefined) out.sandboxRunning = i.sandbox_running;
  if (i.prior_retries !== undefined) out.priorRetries = i.prior_retries;
  return out;
}

function toWant(w: VectorCase["want"]): RunStateResult {
  const out: RunStateResult = { state: w.state as RunStateResult["state"], changed: w.changed };
  if (w.blocked_reason) out.blockedReason = w.blocked_reason;
  if (w.failure_code) out.failureCode = w.failure_code as RunStateResult["failureCode"];
  if (w.retry) out.retry = true;
  return out;
}

const cases = (vectors as { cases: VectorCase[] }).cases;

describe("runstate vectors from the Go table", () => {
  it("has many cases", () => {
    expect(cases.length).toBeGreaterThan(400);
  });
  it.each(cases.map((c, i) => [i, c] as const))("case %i", (_i, c) => {
    const input = toInput(c.input);
    if (c.want.invalid) {
      expect(tryNextRunState(c.state, input)).toBeNull();
      expect(() => nextRunState(c.state, input)).toThrow(InvalidTransitionError);
      return;
    }
    expect(nextRunState(c.state, input)).toEqual(toWant(c.want));
  });
  it("known and finished match", () => {
    for (const [s, v] of Object.entries(vectors.known)) expect(isKnownRunState(s)).toBe(v);
    for (const [s, v] of Object.entries(vectors.finished)) expect(isFinishedRunState(s)).toBe(v);
  });
});

describe("runstate", () => {
  it("rejects unknown states and events", () => {
    for (const s of ["starting", "idle", "nope", ""]) {
      for (const ev of RUN_EVENTS) expect(tryNextRunState(s, { event: ev, outcome: "done" })).toBeNull();
      expect(isKnownRunState(s)).toBe(false);
      expect(isFinishedRunState(s)).toBe(false);
    }
    for (const s of RUN_STATES) expect(tryNextRunState(s, { event: "nope" })).toBeNull();
  });

  it("invalid transition is a conflict OfError", () => {
    try {
      nextRunState("pending", { event: "assigned" });
      expect.unreachable();
    } catch (e) {
      expect(isOfError(e, "conflict")).toBe(true);
      expect((e as InvalidTransitionError).reason).toBe("invalid_transition");
    }
  });

  it("result fields are clean", () => {
    for (const s of RUN_STATES) {
      for (const ev of RUN_EVENTS) {
        const got = tryNextRunState(s, { event: ev, outcome: "done" });
        if (!got) continue;
        if (got.blockedReason) expect(got.state === "blocked" && got.changed).toBe(true);
        if (got.failureCode) expect(got.state === "failed" || got.state === "error" || got.retry === true).toBe(true);
      }
    }
  });

  it("walks a run", () => {
    const steps: [RunStateInput, string][] = [
      [{ event: "assigned" }, "pending"],
      [{ event: "session_started" }, "claimed"],
      [{ event: "turn_started" }, "running"],
      [{ event: "turn_ended", outcome: "done" }, "succeeded"],
      [{ event: "message" }, "queued"],
      [{ event: "assigned" }, "pending"],
      [{ event: "session_started" }, "claimed"],
      [{ event: "turn_started" }, "running"],
      [{ event: "turn_ended", outcome: "done", hasOpenRequest: true }, "blocked"],
      [{ event: "cancel" }, "cancelled"],
      [{ event: "message" }, "queued"],
    ];
    let cur = "queued";
    for (const [input, want] of steps) {
      const got = nextRunState(cur, input);
      expect(got.state).toBe(want);
      expect(got.changed).toBe(true);
      cur = got.state;
    }
  });

  it("the exported transition table agrees with the implementation", () => {
    for (const ev of RUN_EVENTS) {
      const row = RUN_TRANSITIONS[ev];
      for (const s of RUN_STATES) {
        const target = row[s];
        const inputs: RunStateInput[] = [
          { event: ev, outcome: "done" },
          { event: ev, outcome: "error", failureCode: "sandbox_lost", sandboxRunning: true },
          { event: ev, outcome: "needs_input", failureCode: "agent_failed", priorRetries: 5 },
        ];
        for (const input of inputs) {
          const got = tryNextRunState(s, input);
          if (!target) {
            expect(got, `${ev} in ${s}`).toBeNull();
            continue;
          }
          expect(got, `${ev} in ${s}`).not.toBeNull();
          if (target.kind === "move") expect(got!.state).toBe(target.to);
          else if (target.kind === "stay") expect(got).toEqual({ state: s, changed: false });
          else expect(target.to).toContain(got!.state);
        }
      }
    }
  });
});
