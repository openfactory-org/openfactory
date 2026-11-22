// Worker protocol v2 wire shapes (schemas/worker/v2/protocol.json) and the
// conversions between them and the SandboxProvider types. The worker speaks
// snake_case JSON; the control plane uses the camelCase SandboxTask.
import type { FailureCode } from "@openfactory/core";
import { failureForSandboxReason } from "../failure/index";
import {
  ProviderError,
  SANDBOX_REASONS,
  TASK_STATES,
  type SandboxReason,
  type SandboxTask,
  type TaskState,
  type TaskStatus,
} from "../types";

/** WebSocket subprotocol of GET /api/v1/workers/connect. */
export const WORKER_SUBPROTOCOL = "openfactory.worker.v2";

/** Prefix of the signed challenge bytes (internal/sandbox/direct/auth.go). */
export const WORKER_AUTH_CONTEXT = "openfactory-worker-auth-v1";

/** The bytes that a worker signs to answer an auth.challenge. */
export function workerAuthMessage(workerId: string, connectionId: string, nonce: string): Uint8Array {
  return new TextEncoder().encode(`${WORKER_AUTH_CONTEXT}\n${workerId}\n${connectionId}\n${nonce}\n`);
}

/** Worker ID pattern of the protocol schema ($defs.workerId). */
export const WORKER_ID_RE = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;

export interface WireTaskStatus {
  task_id: string;
  state: TaskState;
  reason?: SandboxReason;
  exit_code?: number | null;
  message?: string;
  sandbox_id?: string;
  started_at?: string;
  ended_at?: string;
}

export interface WireHeldTask {
  task_id: string;
  status: WireTaskStatus;
}

export interface WireTask {
  task_id: string;
  run_id: string;
  session: number;
  team_id: string;
  factory_id?: string;
  image: string;
  platform: { os: "linux"; arch: "amd64" | "arm64"; user?: string };
  resources: { cpu?: string; memory?: string };
  env: Record<string, string>;
  network_policy?: { mode: string; allowed_hosts: string[] | null; denied_hosts: string[] | null };
  max_session_seconds: number;
  failure_retention_seconds: number;
  workspace_size_limit?: string;
  labels?: Record<string, string>;
}

export interface WireSlotSize {
  vcpus: number;
  memory_mib: number;
  scratch_gib?: number;
}

export interface WireFacts {
  arch: "amd64" | "arm64";
  os?: string;
  kernel?: string;
  vcpus: number;
  memory_mib?: number;
  disk_total_bytes?: number;
  disk_free_bytes?: number;
  firecracker_version?: string;
  guest_kernel?: string;
  runtime_version?: string;
  kvm?: boolean;
  warnings?: string[];
}

export interface WireHello {
  worker_id: string;
  name?: string;
  backend: string;
  labels?: Record<string, string>;
  capacity: number;
  version?: string;
  one_shot?: boolean;
  tasks: WireHeldTask[] | null;
  protocol_minor?: number;
  features?: string[];
  facts?: WireFacts;
  slots?: number;
  slot?: WireSlotSize;
  draining?: boolean;
}

/** Error codes of a response frame. */
export type WireErrorCode = "not_found" | "at_capacity" | "invalid_spec" | "internal" | "unknown_method";

export interface WireError {
  code: WireErrorCode | string;
  message: string;
}

/** One frame of the envelope {type, id, method, params, result, error}. */
export interface WireFrame {
  type: string;
  id?: string;
  method?: string;
  params?: unknown;
  result?: unknown;
  error?: WireError;
}

/** SandboxTask → wire task. */
export function toWireTask(t: SandboxTask): WireTask {
  const resources: WireTask["resources"] = {};
  if (t.resources.vcpus) resources.cpu = String(t.resources.vcpus);
  if (t.resources.memoryMiB) resources.memory = `${t.resources.memoryMiB}Mi`;
  const out: WireTask = {
    task_id: t.taskId,
    run_id: t.runId,
    session: t.session,
    team_id: t.teamId,
    image: t.image,
    platform: { os: "linux", arch: t.platform.arch, ...(t.platform.user ? { user: t.platform.user } : {}) },
    resources,
    env: { ...t.env },
    max_session_seconds: t.maxSessionSeconds,
    failure_retention_seconds: t.failureRetentionSeconds,
  };
  if (t.factoryId) out.factory_id = t.factoryId;
  if (t.networkPolicy) {
    const allowed = new Set([...t.networkPolicy.allowedHosts, ...(t.networkPolicy.alwaysAllow ?? [])]);
    out.network_policy = {
      mode: t.networkPolicy.mode,
      allowed_hosts: [...allowed].sort(),
      denied_hosts: [...t.networkPolicy.deniedHosts],
    };
  }
  if (t.workspaceSizeLimit) out.workspace_size_limit = t.workspaceSizeLimit;
  if (t.labels && Object.keys(t.labels).length > 0) out.labels = { ...t.labels };
  return out;
}

/** Wire status → TaskStatus. Unknown states and reasons are rejected (null). */
export function fromWireStatus(s: unknown): TaskStatus | null {
  if (typeof s !== "object" || s === null) return null;
  const w = s as Partial<WireTaskStatus>;
  if (typeof w.task_id !== "string" || w.task_id === "") return null;
  if (!TASK_STATES.includes(w.state as TaskState)) return null;
  const out: TaskStatus = { taskId: w.task_id, state: w.state as TaskState };
  if (w.reason !== undefined) {
    if (!SANDBOX_REASONS.includes(w.reason)) return null;
    out.reason = w.reason;
  }
  if (out.state === "exited" && !out.reason) out.reason = "unknown";
  if (typeof w.exit_code === "number") out.exitCode = w.exit_code;
  if (typeof w.message === "string" && w.message) out.message = w.message;
  if (typeof w.sandbox_id === "string" && w.sandbox_id) out.sandboxId = w.sandbox_id;
  if (typeof w.started_at === "string") out.startedAt = w.started_at;
  if (typeof w.ended_at === "string") out.endedAt = w.ended_at;
  return out;
}

/** TaskStatus → wire status. */
export function toWireStatus(s: TaskStatus): WireTaskStatus {
  const out: WireTaskStatus = { task_id: s.taskId, state: s.state };
  if (s.reason) out.reason = s.reason;
  if (s.exitCode !== undefined) out.exit_code = s.exitCode;
  if (s.message) out.message = s.message;
  if (s.sandboxId) out.sandbox_id = s.sandboxId;
  if (s.startedAt) out.started_at = s.startedAt;
  if (s.endedAt) out.ended_at = s.endedAt;
  return out;
}

/** Order of task states (stateRank in hubv2.go): accepted < starting < running < exited. */
export function stateRank(s: TaskState): number {
  return TASK_STATES.indexOf(s);
}

/**
 * Merges a reported status into a known one (mergeStatus in hubv2.go): a
 * status never moves back, and exited is final.
 */
export function mergeStatus(known: TaskStatus | undefined, next: TaskStatus): TaskStatus {
  if (!known) return next;
  if (known.state === "exited") return known;
  if (stateRank(next.state) < stateRank(known.state)) return known;
  return next;
}

/** The ProviderError of a response error (ErrNotFound, ErrInvalid, ErrAtCapacity in hubv2.go). */
export function providerErrorOf(err: WireError): ProviderError {
  switch (err.code) {
    case "not_found":
      return new ProviderError("not_found", err.message || "task not found");
    case "at_capacity":
      return new ProviderError("at_capacity", err.message || "the worker is at capacity");
    case "invalid_spec":
      return new ProviderError("invalid", err.message || "the worker refused the task spec");
    case "unknown_method":
      return new ProviderError("unavailable", `the worker does not know this request: ${err.message}`);
  }
  return new ProviderError("internal", err.message || "worker error");
}

/**
 * Failure code of a microVM worker task end:
 * image pull → sandbox_create_failed, guest OOM → out_of_memory, host reboot or
 * VM lost → sandbox_lost / runner_lost (retry), deadline → infrastructure_timeout.
 * Cancelled gives none.
 */
export function failureForWorkerEnd(s: TaskStatus): FailureCode | undefined {
  if (s.state !== "exited") return undefined;
  return failureForSandboxReason(s.reason ?? "unknown");
}

/**
 * Failure code of a refused task.assign: at_capacity (slots, draining or a
 * full disk) → resource_unavailable; invalid_spec → invalid_configuration.
 */
export function failureForAssignError(code: string): FailureCode {
  switch (code) {
    case "at_capacity":
      return "resource_unavailable";
    case "invalid_spec":
      return "invalid_configuration";
    case "not_found":
      return "sandbox_lost";
  }
  return "sandbox_create_failed";
}
