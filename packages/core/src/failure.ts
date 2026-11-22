// Frozen contract (C0). Port of internal/core/failure.go.
// Checked against schemas/testdata/failure-map.json.

export const FAILURE_CODES = [
  "invalid_configuration",
  "environment_setup_failed",
  "harness_auth_failed",
  "external_authentication_required",
  "integration_disabled",
  "agent_failed",
  "out_of_memory",
  "resource_unavailable",
  "sandbox_create_failed",
  "network_policy_timeout",
  "sandbox_lost",
  "runner_lost",
  "agent_process_failed",
  "infrastructure_timeout",
  "internal_error",
  "spend_limit_exceeded",
] as const;

/** The closed list of reasons why a run failed. */
export type FailureCode = (typeof FAILURE_CODES)[number];

/** Whose fault a failure is. */
export type FailureClass = "user" | "platform";

export interface FailureInfo {
  readonly code: FailureCode;
  readonly class: FailureClass;
  /** The platform starts a new session for a retry code. */
  readonly retry: boolean;
}

/** The number of sessions in sequence that can end with a retry code. */
export const MAX_RETRY_SESSIONS = 3;

/** AllFailureCodes, in table order. */
export const FAILURE_TABLE: readonly FailureInfo[] = [
  { code: "invalid_configuration", class: "user", retry: false },
  { code: "environment_setup_failed", class: "user", retry: false },
  { code: "harness_auth_failed", class: "user", retry: false },
  { code: "external_authentication_required", class: "user", retry: false },
  { code: "integration_disabled", class: "user", retry: false },
  { code: "agent_failed", class: "user", retry: false },
  { code: "out_of_memory", class: "user", retry: false },
  { code: "resource_unavailable", class: "platform", retry: true },
  { code: "sandbox_create_failed", class: "platform", retry: true },
  { code: "network_policy_timeout", class: "platform", retry: true },
  { code: "sandbox_lost", class: "platform", retry: true },
  { code: "runner_lost", class: "platform", retry: true },
  { code: "agent_process_failed", class: "platform", retry: false },
  { code: "infrastructure_timeout", class: "platform", retry: false },
  { code: "internal_error", class: "platform", retry: false },
  { code: "spend_limit_exceeded", class: "platform", retry: false },
];

export function isFailureCode(s: string): s is FailureCode {
  return (FAILURE_CODES as readonly string[]).includes(s);
}

export function lookupFailure(code: string): FailureInfo | undefined {
  return FAILURE_TABLE.find((f) => f.code === code);
}

/**
 * The failure code for a code that a runner reported. A runner can report
 * only environment_setup_failed, harness_auth_failed and agent_failed. Every
 * other string gives agent_failed.
 */
export function runnerFailureCode(code: string): FailureCode {
  switch (code) {
    case "environment_setup_failed":
    case "harness_auth_failed":
    case "agent_failed":
      return code;
  }
  return "agent_failed";
}

/** An error that carries the failure code of a run (core.FailureCodeError). */
export class FailureCodeError extends Error {
  override readonly name = "FailureCodeError";
  constructor(
    readonly failureCode: FailureCode,
    message?: string,
    options?: { cause?: unknown },
  ) {
    super(message ?? failureCode, options);
  }
}
