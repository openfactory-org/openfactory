// The target under test. tools/contract/run.mjs starts the target and passes
// these variables. Without OF_CONTRACT_BASE_URL every suite skips, so the
// plain workspace test run (`npm test`) stays green without a server.
import { describe } from "vitest";

/**
 * Feature tags. A suite names the features it needs; a target skips the
 * suites whose tags it lists in OF_CONTRACT_SKIP_TAGS.
 */
export const TAGS = {
  /** Core REST API. */
  rest: "rest",
  /** Fake runner sessions over /runner/v1. Needs a session source. */
  runner: "runner",
  /** Session start refuses an unsupported runner version. */
  runnerVersion: "runner-version",
  /** Server-Sent Events: /stream and /runs/{id}/events. */
  sse: "sse",
  /** Worker protocol v2 over WebSocket (connect, hello, welcome, heartbeat). */
  workerProtocol: "worker-protocol",
  /** A run dispatched to a connected worker (task.assign). */
  workerDispatch: "worker-dispatch",
  /** Enrollment tokens, Ed25519 worker auth, drain, resume, revoke, git credential. */
  workerEnrollment: "worker-enrollment",
  /** POST /secrets/rewrap. */
  secretsRewrap: "secrets-rewrap",
  /** Run-loop scenarios with the fake runner (foreman, child, lifecycle, concurrency). */
  scenario: "scenario",
  /** The real Go runner with the exec harness. Opt-in: OF_CONTRACT_REAL_RUNNER=1. */
  realRunner: "real-runner",
} as const;

export type Tag = (typeof TAGS)[keyof typeof TAGS];

const env = process.env;

export const target = {
  baseUrl: (env.OF_CONTRACT_BASE_URL ?? "").replace(/\/+$/, ""),
  /** The runner version that the fake runner sends. */
  runnerVersion: env.OF_CONTRACT_RUNNER_VERSION ?? "1.0.0-contract",
  /** A version that session start must refuse. */
  badRunnerVersion: env.OF_CONTRACT_BAD_RUNNER_VERSION ?? "0.0.1-mismatch",
  skipTags: new Set(
    (env.OF_CONTRACT_SKIP_TAGS ?? "")
      .split(",")
      .map((s) => s.trim())
      .filter(Boolean),
  ),
};

export const enabled = target.baseUrl !== "";

/** Whether the target supports every tag. */
export function supports(...tags: Tag[]): boolean {
  if (!enabled) return false;
  if (tags.includes(TAGS.realRunner) && env.OF_CONTRACT_REAL_RUNNER !== "1") return false;
  return tags.every((t) => !target.skipTags.has(t));
}

/** describe() that skips when the target lacks a tag. The tags show in the suite name. */
export function describeFeature(tags: Tag[], name: string, fn: () => void): void {
  const title = `${name} [${tags.join(",")}]`;
  if (supports(...tags)) describe(title, fn);
  else describe.skip(title, fn);
}

export function apiUrl(path: string): string {
  return `${target.baseUrl}/api/v1${path}`;
}
