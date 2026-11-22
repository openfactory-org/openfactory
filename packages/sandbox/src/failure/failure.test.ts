import { describe, expect, it } from "vitest";
import failureMap from "../../../../schemas/testdata/failure-map.json";
import { ProviderError } from "../types";
import { failureForProviderError, failureForSandboxReason, reasonForExitCode } from "./index";

describe("failureForSandboxReason (failure-map.json)", () => {
  it.each(failureMap.sandbox_reasons)("$reason", ({ reason, failure }) => {
    expect(failureForSandboxReason(reason) ?? null).toBe(failure);
  });
  it("maps an unknown reason to runner_lost", () => {
    expect(failureForSandboxReason("something_new")).toBe("runner_lost");
  });
});

describe("reasonForExitCode", () => {
  it.each([
    [0, false, "completed"],
    [75, false, "network_policy_timeout"],
    [64, false, "invalid_spec"],
    [65, false, "invalid_spec"],
    [1, false, "process_failed"],
    [137, true, "container_oom"],
    [137, false, "process_failed"],
  ] as const)("%d (oom %s) → %s", (code, oom, want) => {
    expect(reasonForExitCode(code, oom)).toBe(want);
  });
});

describe("failureForProviderError", () => {
  it.each([
    [new ProviderError("at_capacity", "full", 30), { consumesRetry: false, requeueAfterSeconds: 30 }],
    [new ProviderError("rate_limited", "slow"), { consumesRetry: false }],
    [new ProviderError("invalid", "bad"), { code: "invalid_configuration", consumesRetry: false }],
    [new ProviderError("unavailable", "down"), { code: "sandbox_create_failed", consumesRetry: true }],
    [new ProviderError("internal", "oops"), { code: "sandbox_create_failed", consumesRetry: true }],
    [new ProviderError("not_found", "gone"), { code: "runner_lost", consumesRetry: true }],
    [new Error("provider_error:invalid: over RPC"), { code: "invalid_configuration", consumesRetry: false }],
    [new Error("network"), { code: "sandbox_create_failed", consumesRetry: true }],
  ])("%s", (err, want) => {
    expect(failureForProviderError(err)).toEqual(want);
  });
});
