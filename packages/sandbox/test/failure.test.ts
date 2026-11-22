import { describe, expect, it } from "vitest";
import failureMap from "../../../schemas/testdata/failure-map.json";
import { failureForSandboxReason } from "../src/index";

describe("failureForSandboxReason", () => {
  it.each(failureMap.sandbox_reasons)("$reason", ({ reason, failure }) => {
    expect(failureForSandboxReason(reason) ?? null).toBe(failure);
  });
});
