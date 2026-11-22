import { describe, expect, it } from "vitest";
import fixture from "../../../../schemas/testdata/runstate-transitions.json";
import { finished, InvalidTransitionError, known, next, type RunInput } from "./runstate";

interface Case {
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
    changed?: boolean;
    invalid?: boolean;
    failure_code?: string;
    blocked_reason?: string;
    retry?: boolean;
  };
}

describe("runstate (go-final goldens)", () => {
  const cases = (fixture as { cases: Case[] }).cases;

  it("has the generated cases", () => {
    expect(cases.length).toBeGreaterThan(400);
  });

  it.each(cases.map((c, i) => [i, c] as const))("case %i", (_i, c) => {
    const input: RunInput = {
      event: c.input.event as RunInput["event"],
      outcome: c.input.outcome,
      failureCode: c.input.failure_code,
      hasOpenRequest: c.input.has_open_request,
      sandboxRunning: c.input.sandbox_running,
      priorRetries: c.input.prior_retries,
    };
    if (c.want.invalid) {
      expect(() => next(c.state, input)).toThrow(InvalidTransitionError);
      return;
    }
    const got = next(c.state, input);
    expect(got.state).toBe(c.want.state);
    expect(got.changed).toBe(c.want.changed ?? false);
    expect(got.failureCode ?? "").toBe(c.want.failure_code ?? "");
    expect(got.blockedReason ?? "").toBe(c.want.blocked_reason ?? "");
    expect(got.retry ?? false).toBe(c.want.retry ?? false);
  });

  it("known and finished match", () => {
    const f = fixture as unknown as { known: Record<string, boolean>; finished: Record<string, boolean> };
    for (const [s, v] of Object.entries(f.known)) expect(known(s)).toBe(v);
    for (const [s, v] of Object.entries(f.finished)) expect(finished(s)).toBe(v);
  });
});
