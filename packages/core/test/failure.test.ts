import { describe, expect, it } from "vitest";
import failureMap from "../../../schemas/testdata/failure-map.json";
import { FAILURE_TABLE, MAX_RETRY_SESSIONS, isFailureCode, lookupFailure, runnerFailureCode } from "../src/index";

describe("failure codes match the Go table", () => {
  it("has the same codes, classes and retry flags in order", () => {
    expect(FAILURE_TABLE.map((f) => ({ ...f }))).toEqual(failureMap.failure_codes);
    expect(MAX_RETRY_SESSIONS).toBe(failureMap.max_retry_sessions);
  });
  it.each(failureMap.runner_codes)("runnerFailureCode($reported)", ({ reported, failure }) => {
    expect(runnerFailureCode(reported)).toBe(failure);
  });
  it("looks up codes", () => {
    expect(lookupFailure("sandbox_lost")?.retry).toBe(true);
    expect(lookupFailure("nope")).toBeUndefined();
    expect(isFailureCode("agent_failed")).toBe(true);
  });
});
