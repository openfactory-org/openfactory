// The SandboxProvider contract: a port of core.TaskBackend
// (internal/core/task.go) for Workers RPC.
//
// Rules:
// - Every method is async and returns quickly; long work happens in the provider.
// - Payloads are plain JSON-shaped objects (no class instances), so the same
//   interface works in-process (daytona, boatdev), over a service binding
//   (cloudflare-sandbox, plugin Workers) and for workers behind WorkerHubDO.
// - `Watch` is gone: WorkDO timers poll `status`, microVM workers push task.state.
// - Arch comes from the provider capabilities, never from the control plane host.

import type { FailureCode } from "@openfactory/core";

// ---------- Task (port of core.Task without ToolImages/RunnerImage) ----------

export type TaskState = "accepted" | "starting" | "running" | "exited";

export const TASK_STATES: readonly TaskState[] = ["accepted", "starting", "running", "exited"];

export type SandboxReason =
  | "completed"
  | "process_failed"
  | "image_pull"
  | "unschedulable"
  | "network_policy_timeout"
  | "invalid_spec"
  | "container_oom"
  | "evicted"
  | "node_lost"
  | "deadline_exceeded"
  | "cancelled"
  | "unknown";

export const SANDBOX_REASONS: readonly SandboxReason[] = [
  "completed",
  "process_failed",
  "image_pull",
  "unschedulable",
  "network_policy_timeout",
  "invalid_spec",
  "container_oom",
  "evicted",
  "node_lost",
  "deadline_exceeded",
  "cancelled",
  "unknown",
];

/** Variables of the sandbox image and bootstrap contract. */
export const TASK_ENV = {
  controlUrl: "OF_CONTROL_URL",
  runId: "OF_RUN_ID",
  session: "OF_SESSION",
  bootstrapToken: "OF_BOOTSTRAP_TOKEN",
  runnerVersion: "OF_RUNNER_VERSION",
  runnerUrl: "OF_RUNNER_URL",
  runnerSha256: "OF_RUNNER_SHA256",
  egressLock: "OF_EGRESS_LOCK",
  netguardReport: "OF_NETGUARD_REPORT",
} as const;

/** Value of OF_EGRESS_LOCK. */
export type EgressLock = "none" | "netns" | "iptables";

export type Arch = "amd64" | "arm64";

export interface TaskPlatform {
  os: "linux";
  arch: Arch;
  /** Numeric user ID, or "image" for the image user. Empty: provider default. */
  user?: string;
}

/** Normalized resources, filled from the machine spec (vCPUs, memory). */
export interface TaskResources {
  vcpus?: number;
  memoryMiB?: number;
  diskMiB?: number;
  /** Set by preflight when the provider maps the request to a named type. */
  instanceType?: string;
}

export interface TaskNetworkPolicy {
  /** An unrestricted task carries no policy. */
  mode: "restricted";
  /** Exact hostnames or IP literals; required hosts (control plane, R2) are merged in. */
  allowedHosts: string[];
  deniedHosts: string[];
  /** Platform hosts. A leading "." means the host and its subdomains, "*." subdomains only. */
  alwaysAllow?: string[];
}

export interface SandboxTask {
  /** `${runId}.${session}`; the idempotency key of start. */
  taskId: string;
  runId: string;
  session: number;
  teamId: string;
  factoryId?: string;
  /** Provider-specific ref: OCI ref, snapshot name or image key. */
  image: string;
  platform: TaskPlatform;
  resources: TaskResources;
  /** Bootstrap variables only (TASK_ENV). Never secret values other than the bootstrap token. */
  env: Record<string, string>;
  networkPolicy?: TaskNetworkPolicy;
  maxSessionSeconds: number;
  failureRetentionSeconds: number;
  workspaceSizeLimit?: string;
  labels?: Record<string, string>;
}

export interface TaskStatus {
  taskId: string;
  state: TaskState;
  reason?: SandboxReason;
  exitCode?: number;
  message?: string;
  sandboxId?: string;
  /** RFC 3339. */
  startedAt?: string;
  endedAt?: string;
  /** Provider-native state, for the timeline and for debugging. */
  providerState?: string;
}

// ---------- Capabilities ----------

export type Egress = "native" | "in-sandbox" | "none";

export type Isolation = "container" | "microvm" | "vm" | "process";

export interface InstanceType {
  name: string;
  vcpus: number;
  memoryMiB: number;
  diskMiB?: number;
}

/** A start-rate window, for example boat.dev {12,60}, {60,3600}, {200,86400}. */
export interface RateWindow {
  limit: number;
  windowSeconds: number;
}

/** Limits of a provider-native allow list. */
export interface NativeEgressLimits {
  maxEntries: number | null;
  wildcard: "base-and-subdomains" | "glob" | "none";
  ipLiterals: boolean;
  nonHttp: "blocked" | "allowed";
  /** The image must trust a provider CA. */
  tlsInterception: boolean;
}

export interface ProviderCapabilities {
  /** How the provider enforces a restricted network policy. "none": it cannot. */
  egress: Egress;
  nativeEgress?: NativeEgressLimits;
  image: "oci" | "snapshot";
  isolation: Isolation;
  /** Architectures the provider runs. All three hosted providers: ["amd64"]. */
  arch: Arch[];
  /** null: no fixed limit. The provider TTL is still set as a leak bound. */
  maxLifetimeSeconds: number | null;
  /** Start-rate windows; empty means not documented (adapt from 429 responses). */
  startRate: RateWindow[];
  maxConcurrent?: number;
  instanceTypes: InstanceType[];
  /** True when resources can be any value within limits, not only instanceTypes. */
  customResources?: boolean;
  /** Provider daemon ports on loopback that the in-sandbox lock blocks for the agent uid. */
  loopbackDaemonPorts: number[];
  /** Fixed slot count (microVM workers). */
  slots?: number;
  regions?: string[];
}

// ---------- Errors ----------

export type ProviderErrorKind =
  "at_capacity" | "rate_limited" | "unavailable" | "not_found" | "invalid" | "conflict" | "internal";

/**
 * A provider failure. Over RPC the class and its fields are lost; only the
 * message survives. The message has the form
 * `provider_error:<kind>[;retry_after=<seconds>][;code=<code>]: <detail>`, so
 * `parseProviderError` can rebuild the kind, the retry delay and the provider
 * code. `code` is URI-encoded.
 *
 * DispatchDO mapping (startFailed in dispatch/claim.go):
 * - at_capacity, rate_limited: requeue without using a retry
 *   (resource_unavailable when the queue timeout passes)
 * - invalid: invalid_configuration, no retry
 * - unavailable, internal: sandbox_create_failed (retry code)
 * - not_found (status): runner_lost via failureForSandboxReason("unknown")
 */
export class ProviderError extends Error {
  override readonly name = "ProviderError";
  constructor(
    readonly kind: ProviderErrorKind,
    readonly detail: string,
    readonly retryAfterSeconds?: number,
    readonly providerCode?: string,
  ) {
    super(providerErrorMessage(kind, detail, retryAfterSeconds, providerCode));
  }
}

const PROVIDER_ERROR_KINDS: readonly string[] = [
  "at_capacity",
  "rate_limited",
  "unavailable",
  "not_found",
  "invalid",
  "conflict",
  "internal",
];

function providerErrorMessage(
  kind: ProviderErrorKind,
  detail: string,
  retryAfterSeconds?: number,
  providerCode?: string,
): string {
  let meta = "";
  if (retryAfterSeconds !== undefined && Number.isFinite(retryAfterSeconds) && retryAfterSeconds >= 0)
    meta += `;retry_after=${retryAfterSeconds}`;
  if (providerCode) meta += `;code=${encodeURIComponent(providerCode)}`;
  return `provider_error:${kind}${meta}: ${detail}`;
}

export function parseProviderError(e: unknown): ProviderError | undefined {
  if (e instanceof ProviderError) return e;
  if (!(e instanceof Error)) return undefined;
  const m = /^provider_error:([a-z_]+)((?:;[a-z_]+=[^;:\s]*)*): ([\s\S]*)$/.exec(e.message);
  if (!m || !PROVIDER_ERROR_KINDS.includes(m[1]!)) return undefined;
  let retryAfter: number | undefined;
  let code: string | undefined;
  for (const part of m[2]!.split(";").slice(1)) {
    const i = part.indexOf("=");
    const k = part.slice(0, i);
    const v = part.slice(i + 1);
    if (k === "retry_after") {
      const n = Number(v);
      if (v !== "" && Number.isFinite(n) && n >= 0) retryAfter = n;
    } else if (k === "code" && v !== "") {
      try {
        code = decodeURIComponent(v);
      } catch {
        code = v;
      }
    }
  }
  return new ProviderError(m[1] as ProviderErrorKind, m[3]!, retryAfter, code);
}

// ---------- Provider ----------

export type PreflightResult =
  { ok: true; resources?: TaskResources } | { ok: false; code: FailureCode; message: string };

export interface SandboxProvider {
  /** "cloudflare-sandbox" | "daytona" | "boatdev" | "microvm" | "process" | plugin name. */
  readonly id: string;
  capabilities(): Promise<ProviderCapabilities>;
  /**
   * Checks one task before start: credentials, image or snapshot readiness,
   * machine size to instance type (too large: invalid_configuration), arch.
   * Never creates a billable sandbox.
   */
  preflight(task: SandboxTask): Promise<PreflightResult>;
  /** Idempotent on taskId: a second call returns the existing task. */
  start(task: SandboxTask): Promise<TaskStatus>;
  /** Throws ProviderError not_found for an unknown task. */
  status(taskId: string): Promise<TaskStatus>;
  /** Asks the runner to stop. The caller schedules destroy after graceSeconds. */
  cancel(taskId: string, graceSeconds: number): Promise<void>;
  /** Removes the sandbox and its storage. Safe to call twice; not_found is success. */
  destroy(taskId: string): Promise<void>;
  /** Every sandbox of this install, for the orphan sweep. */
  list(): Promise<TaskStatus[]>;
}

/** The RPC shape of a provider Worker over a service binding (`id` becomes a method). */
export type SandboxProviderRpc = Omit<SandboxProvider, "id"> & { id(): Promise<string> };
