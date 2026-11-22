// Port of core.FailureForSandboxReason (internal/core/failure.go).
// Checked against schemas/testdata/failure-map.json.

import type { FailureCode } from "@openfactory/core";
import type { SandboxReason } from "./types";

/**
 * The failure code of a run whose sandbox ended without a session end.
 * "cancelled" has no failure code: the result is undefined. An unknown
 * reason gives runner_lost.
 */
export function failureForSandboxReason(reason: SandboxReason | string): FailureCode | undefined {
  switch (reason) {
    case "completed":
    case "process_failed":
      return "agent_process_failed";
    case "image_pull":
    case "unschedulable":
      return "sandbox_create_failed";
    case "network_policy_timeout":
      return "network_policy_timeout";
    case "invalid_spec":
      return "invalid_configuration";
    case "container_oom":
      return "out_of_memory";
    case "evicted":
    case "node_lost":
      return "sandbox_lost";
    case "deadline_exceeded":
      return "infrastructure_timeout";
    case "cancelled":
      return undefined;
  }
  return "runner_lost";
}
