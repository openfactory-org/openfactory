// Failure mapping for sandbox ends and provider errors.
//
// - failureForSandboxReason: port of core.FailureForSandboxReason, checked
//   against schemas/testdata/failure-map.json (implementation in ../failure.ts).
// - reasonForExitCode: the bootstrap exit codes (02 §2.3).
// - failureForProviderError: the DispatchDO mapping of a failed start
//   (startFailed in internal/dispatch/claim.go at go-final).

import type { FailureCode } from "@openfactory/core";
import { failureForSandboxReason } from "../failure";
import { parseProviderError, type SandboxReason } from "../types";

export { failureForSandboxReason };

/** Bootstrap exit codes. */
export const EXIT_BAD_PLATFORM = 64;
export const EXIT_CHECKSUM_MISMATCH = 65;
/** The in-sandbox lock was not proven (same code as egress.WaitExitCode). */
export const EXIT_LOCK_NOT_PROVEN = 75;
export const EXIT_SIGKILL = 137;
export const EXIT_SIGTERM = 143;

/**
 * The sandbox reason of a runner process that exited with code. `oom` is true
 * when the provider reports an OOM kill.
 */
export function reasonForExitCode(code: number, oom = false): SandboxReason {
  if (oom) return "container_oom";
  switch (code) {
    case 0:
      return "completed";
    case EXIT_LOCK_NOT_PROVEN:
      return "network_policy_timeout";
    case EXIT_BAD_PLATFORM:
    case EXIT_CHECKSUM_MISMATCH:
      return "invalid_spec";
  }
  return "process_failed";
}

export interface StartFailure {
  /** Failure code of the lost event. Undefined: requeue, the start did not happen. */
  code?: FailureCode;
  /** True when the failure uses one of the run's retries. */
  consumesRetry: boolean;
  /** Requeue after this many seconds (at_capacity, rate_limited). */
  requeueAfterSeconds?: number;
}

/**
 * DispatchDO mapping of a start error:
 * - at_capacity, rate_limited: requeue without using a retry (the caller
 *   uses resource_unavailable once the queue timeout passes)
 * - invalid: invalid_configuration, no retry
 * - conflict, unavailable, internal and any other error: sandbox_create_failed (retry)
 */
export function failureForProviderError(err: unknown): StartFailure {
  const pe = parseProviderError(err);
  switch (pe?.kind) {
    case "at_capacity":
    case "rate_limited": {
      const out: StartFailure = { consumesRetry: false };
      if (pe.retryAfterSeconds !== undefined) out.requeueAfterSeconds = pe.retryAfterSeconds;
      return out;
    }
    case "invalid":
      return { code: "invalid_configuration", consumesRetry: false };
    case "not_found":
      return { code: failureForSandboxReason("unknown") ?? "runner_lost", consumesRetry: true };
  }
  return { code: "sandbox_create_failed", consumesRetry: true };
}
