import { describe, expect, it } from "vitest";
import failureMap from "../../../../schemas/testdata/failure-map.json";
import { ProviderError, parseProviderError } from "../types";
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
    // Over RPC only the message survives: the retry delay must come back from it.
    [
      new Error(new ProviderError("at_capacity", "full", 30).message),
      { consumesRetry: false, requeueAfterSeconds: 30 },
    ],
  ])("%s", (err, want) => {
    expect(failureForProviderError(err)).toEqual(want);
  });
});

describe("parseProviderError across RPC", () => {
  it.each<[string, ProviderError]>([
    ["kind only", new ProviderError("invalid", "bad spec")],
    ["retry delay", new ProviderError("rate_limited", "slow down", 12)],
    ["retry delay 0", new ProviderError("at_capacity", "full", 0)],
    ["code", new ProviderError("not_found", "gone", undefined, "snapshot_not_found")],
    ["both", new ProviderError("at_capacity", "no machine: try later", 30, "no_ready_machine")],
    ["code with separators", new ProviderError("unavailable", "x", 5, "a;b: c=d")],
    ["detail with colons", new ProviderError("unavailable", "boatdev start: 503 a: b", 7, "busy")],
  ])("%s", (_n, pe) => {
    // Workers RPC rebuilds a plain Error with the same message.
    const got = parseProviderError(new Error(pe.message))!;
    expect(got).toBeInstanceOf(ProviderError);
    expect({ kind: got.kind, detail: got.detail, retry: got.retryAfterSeconds, code: got.providerCode }).toEqual({
      kind: pe.kind,
      detail: pe.detail,
      retry: pe.retryAfterSeconds,
      code: pe.providerCode,
    });
    expect(got.message).toBe(pe.message);
  });
  it("keeps the old form without metadata", () => {
    expect(new ProviderError("invalid", "bad").message).toBe("provider_error:invalid: bad");
  });
  it.each(["provider_error:bogus: x", "provider_error:invalid", "something else"])("rejects %j", (m) => {
    expect(parseProviderError(new Error(m))).toBeUndefined();
  });
});
