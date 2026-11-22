// @openfactory/boatdev: the boat.dev sandbox provider (a library the control
// plane runs in-process). See docs/sandbox-providers.md.
//
// Facts that shape it:
// - Start limits 12 a minute, 60 an hour, 200 a day; 100 sandboxes at once ($20 plan).
// - Commands run as `user`, so the runner starts with `sudo -n`.
// - The command API has no env field: the runner starts from `setupScript`,
//   sent with an Idempotency-Key, and that script writes the runner env to a
//   root-only file that bootstrap.sh reads.
// - `noEnv: true` keeps the account secrets and the in-sandbox API key out.
// - `ttlSeconds` = max session + grace, as a leak bound. Label `of-task=<id>`.
// - EU only. x86_64 only. No network API: egress is the in-sandbox lock.

import { sha256Hex } from "@openfactory/core";
import {
  ProviderError,
  TASK_ENV,
  egressLockFor,
  isRestricted,
  parseRetryAfter,
  preflightTask,
  reasonForExitCode,
  resolveInstanceType,
  sandboxEnv,
  sandboxLabels,
  sanitizeTaskId,
  taskIdFromLabel,
  type InstanceType,
  type PreflightResult,
  type ProviderCapabilities,
  type RateWindow,
  type SandboxProvider,
  type SandboxTask,
  type TaskStatus,
} from "@openfactory/sandbox";
import {
  BOOTSTRAP_ENTRYPOINT,
  ProviderHttpClient,
  RUNNER_TERM_COMMAND,
  RUNTIME_DIR,
  TTL_GRACE_SECONDS,
  exitedStatus,
  paginateCursor,
  parseExitCode,
  sandboxName,
  type HttpErrorInfo,
} from "@openfactory/sandbox/provider-kit";

export const PROVIDER_ID = "boatdev";
export const DEFAULT_BASE_URL = "https://boat.dev/api/v1";

export const MAX_TTL_SECONDS = 2_592_000;

/** $20 plan. */
export const DEFAULT_START_RATE: RateWindow[] = [
  { limit: 12, windowSeconds: 60 },
  { limit: 60, windowSeconds: 3600 },
  { limit: 200, windowSeconds: 86_400 },
];
export const DEFAULT_MAX_CONCURRENT = 100;

const GiB = 1024;
export const BOAT_INSTANCE_TYPES: InstanceType[] = [
  { name: "small", vcpus: 2, memoryMiB: 4 * GiB, diskMiB: 51 * GiB },
  { name: "default", vcpus: 4, memoryMiB: 8 * GiB, diskMiB: 51 * GiB },
  { name: "large", vcpus: 8, memoryMiB: 16 * GiB, diskMiB: 111 * GiB },
  { name: "xlarge", vcpus: 16, memoryMiB: 32 * GiB, diskMiB: 251 * GiB },
];

/** Loopback ports of boat daemons: ssh, VNC, web, the boat agent, streaming. */
export const BOAT_DAEMON_PORTS: number[] = [22, 6080, 8090, 8911, ...range(47984, 48010)];

function range(a: number, b: number): number[] {
  return Array.from({ length: b - a + 1 }, (_, i) => a + i);
}

/** Root-only file with the runner env. bootstrap.sh reads it (OF_ENV_FILE). */
export const RUNNER_ENV_FILE = `${RUNTIME_DIR}/runner.env`;

export interface RateLimitInfo {
  provider: string;
  /** Raw RateLimit-Policy and RateLimit headers. */
  policy?: string;
  remaining?: string;
  retryAfterSeconds?: number;
}

export interface BoatdevConfig {
  /** `boat_…` API key with the ci preset. */
  apiKey: string;
  /** Install ID (label of-install). */
  installId: string;
  baseUrl?: string;
  fetch?: typeof fetch;
  /** Plan limits. Default: the $20 plan. */
  startRate?: RateWindow[];
  maxConcurrent?: number;
  /** Allowed machine classes. Default: small, default, large (xlarge needs the $100 plan). */
  instanceTypes?: InstanceType[];
  /** Feeds the remaining-quota headers back to the DispatchDO bucket. */
  onRateLimit?: (info: RateLimitInfo) => void;
}

/** Fields of the boat Sandbox schema that the adapter reads. */
export interface BoatSandbox {
  id: string;
  name?: string;
  state: string;
  setupStatus?: "pending" | "running" | "done" | "failed" | null;
  setupError?: string | null;
  health?: "ok" | "degraded";
  healthReason?: string | null;
  lastReboot?: { at?: string | null } | null;
  archiveAfter?: string | null;
  stoppedAt?: string | null;
  createdAt?: string;
  readyAt?: string | null;
  error?: string | { message?: string } | null;
  labels?: Record<string, string>;
}

/** The setupScript: writes the root-only env file, then starts the bootstrap with sudo -n. */
export function setupScript(envKeys: readonly string[]): string {
  const keys = [...envKeys].filter((k) => /^[A-Z_][A-Z0-9_]*$/.test(k)).sort();
  return [
    "set -eu",
    `sudo -n /bin/sh -c 'umask 077; mkdir -p ${RUNTIME_DIR}; : > ${RUNNER_ENV_FILE}'`,
    `for v in ${keys.join(" ")}; do`,
    String.raw`  eval "val=\${$v-}"`,
    String.raw`  printf "export %s='%s'\n" "$v" "$(printf '%s' "$val" | sed "s/'/'\\\\''/g")"`,
    `done | sudo -n tee ${RUNNER_ENV_FILE} >/dev/null`,
    `exec sudo -n ${BOOTSTRAP_ENTRYPOINT.join(" ")}`,
  ].join("\n");
}

interface Ctx {
  cancelRequested: boolean;
}

const STARTING = new Set(["init", "provisioning", "provisioned", "cloning"]);
const READY = new Set(["ready", "idle", "running"]);
const ARCHIVED = new Set(["archiving", "archived"]);

function errText(e: BoatSandbox["error"]): string | undefined {
  if (!e) return undefined;
  return typeof e === "string" ? e : e.message;
}

/** Maps a boat sandbox to a TaskStatus. The rules are evaluated in order. */
export function mapBoatSandbox(taskId: string, sb: BoatSandbox, ctx: Ctx = { cancelRequested: false }): TaskStatus {
  const st: TaskStatus = { taskId, state: "starting", sandboxId: sb.id, providerState: sb.state };
  const startedAt = sb.readyAt ?? sb.createdAt;
  if (startedAt) st.startedAt = startedAt;
  const exited = (reason: TaskStatus["reason"], message?: string, exitCode?: number): TaskStatus =>
    exitedStatus(st, reason, { message, exitCode, endedAt: sb.stoppedAt ?? undefined });
  if (sb.health === "degraded" && sb.healthReason) st.message = sb.healthReason;

  if (sb.state === "cancelled")
    return exited(ctx.cancelRequested ? "cancelled" : "unschedulable", "boat cancelled the sandbox");
  if (STARTING.has(sb.state)) return st;
  const reboot = sb.lastReboot?.at;
  if (reboot && startedAt && Date.parse(reboot) > Date.parse(startedAt)) {
    return exited("node_lost", "the sandbox rebooted, and its processes ended");
  }
  if (sb.state === "error") return exited("node_lost", errText(sb.error));
  if (ARCHIVED.has(sb.state)) {
    if (ctx.cancelRequested) return exited("cancelled");
    if (sb.archiveAfter && sb.stoppedAt && Date.parse(sb.archiveAfter) <= Date.parse(sb.stoppedAt)) {
      return exited("deadline_exceeded", "the sandbox reached its ttl");
    }
    return exited("evicted", "boat stopped the sandbox");
  }
  if (READY.has(sb.state)) {
    switch (sb.setupStatus) {
      case "done":
        return exited("completed", undefined, 0);
      case "failed": {
        const code = parseExitCode(sb.setupError);
        if (ctx.cancelRequested) return exited("cancelled", sb.setupError ?? undefined, code);
        return exited(
          code === undefined ? "process_failed" : reasonForExitCode(code),
          sb.setupError ?? undefined,
          code,
        );
      }
      default:
        return { ...st, state: "running" };
    }
  }
  return { ...st, state: "running" };
}

export class BoatdevProvider implements SandboxProvider {
  readonly id = PROVIDER_ID;
  private readonly http: ProviderHttpClient;
  /**
   * taskId → sandbox ID, a cache for the life of this object only. The control
   * plane builds a new provider object for each call, so every lookup must also
   * work from the label lookup alone.
   */
  private readonly ids = new Map<string, string>();

  constructor(private readonly cfg: BoatdevConfig) {
    if (!cfg.apiKey) throw new ProviderError("invalid", "boatdev: the API key is empty");
    if (!cfg.installId) throw new ProviderError("invalid", "boatdev: the install ID is empty");
    this.http = new ProviderHttpClient({
      provider: PROVIDER_ID,
      baseUrl: cfg.baseUrl ?? DEFAULT_BASE_URL,
      apiKey: cfg.apiKey,
      ...(cfg.fetch ? { fetch: cfg.fetch } : {}),
      toError: boatError,
    });
  }

  async capabilities(): Promise<ProviderCapabilities> {
    return {
      egress: "in-sandbox",
      image: "snapshot",
      isolation: "vm",
      arch: ["amd64"],
      maxLifetimeSeconds: MAX_TTL_SECONDS - TTL_GRACE_SECONDS,
      startRate: this.cfg.startRate ?? DEFAULT_START_RATE,
      maxConcurrent: this.cfg.maxConcurrent ?? DEFAULT_MAX_CONCURRENT,
      instanceTypes: this.cfg.instanceTypes ?? BOAT_INSTANCE_TYPES.slice(0, 3),
      customResources: false,
      loopbackDaemonPorts: BOAT_DAEMON_PORTS,
      regions: ["eu"],
    };
  }

  // ---------- HTTP ----------

  private reportRate(res: Response): void {
    const policy = res.headers.get("ratelimit-policy") ?? undefined;
    const remaining = res.headers.get("ratelimit") ?? undefined;
    const retry = parseRetryAfter(res.headers.get("retry-after"));
    if (!this.cfg.onRateLimit || (policy === undefined && remaining === undefined && retry === undefined)) return;
    const info: RateLimitInfo = { provider: PROVIDER_ID };
    if (policy !== undefined) info.policy = policy;
    if (remaining !== undefined) info.remaining = remaining;
    if (retry !== undefined) info.retryAfterSeconds = retry;
    this.cfg.onRateLimit(info);
  }

  private static unwrap(b: unknown): BoatSandbox {
    const o = b as { sandbox?: BoatSandbox; data?: BoatSandbox };
    return o.sandbox ?? o.data ?? (b as BoatSandbox);
  }

  // ---------- Lookup ----------

  private async findByLabel(taskId: string): Promise<BoatSandbox | undefined> {
    const q = new URLSearchParams();
    q.append("label", `of-task=${sanitizeTaskId(taskId)}`);
    q.append("label", `of-install=${this.cfg.installId}`);
    q.set("limit", "200");
    const res = await this.http.call("GET", `/sandboxes?${q}`);
    const b = await this.http.json<{ sandboxes?: BoatSandbox[]; items?: BoatSandbox[]; data?: BoatSandbox[] }>(
      res,
      "list",
    );
    // Repeated label= parameters may not combine with AND: check both labels here.
    const hit = (b.sandboxes ?? b.items ?? b.data ?? []).find(
      (s) =>
        s.labels?.["of-task"] === sanitizeTaskId(taskId) &&
        s.labels?.["of-install"] === this.cfg.installId &&
        s.state !== "cancelled",
    );
    if (hit) this.ids.set(taskId, hit.id);
    return hit;
  }

  private async get(taskId: string): Promise<BoatSandbox | undefined> {
    const id = this.ids.get(taskId);
    if (id) {
      const res = await this.http.call("GET", `/sandboxes/${encodeURIComponent(id)}`);
      if (res.status === 404) {
        this.ids.delete(taskId);
        return undefined;
      }
      return BoatdevProvider.unwrap(await this.http.json(res, "status"));
    }
    return this.findByLabel(taskId);
  }

  // ---------- SandboxProvider ----------

  async preflight(task: SandboxTask): Promise<PreflightResult> {
    const caps = await this.capabilities();
    const shape = preflightTask(caps, task);
    if (!shape.ok) return shape;
    try {
      await this.http.json(await this.http.call("GET", "/me"), "me");
      const limits = await this.http.json<{
        canStart?: boolean;
        machineTypes?: string[];
        limits?: { canStart?: boolean; machineTypes?: string[] };
      }>(await this.http.call("GET", "/limits"), "limits");
      const l = limits.limits ?? limits;
      const type = shape.resources?.instanceType;
      if (type && Array.isArray(l.machineTypes) && !l.machineTypes.includes(type)) {
        return {
          ok: false,
          code: "invalid_configuration",
          message: `boatdev: the plan does not allow machine type ${type}`,
        };
      }
      if (l.canStart === false)
        return { ok: false, code: "resource_unavailable", message: "boatdev: the account cannot start a sandbox now" };
      const snap = await this.http.call("GET", `/saved-snapshots/${encodeURIComponent(task.image)}`);
      if (snap.status === 404)
        return {
          ok: false,
          code: "invalid_configuration",
          message: `boatdev: saved snapshot ${task.image} does not exist`,
        };
      const s = await this.http.json<{ status?: string; snapshot?: { status?: string } }>(snap, "snapshot");
      const status = s.snapshot?.status ?? s.status;
      if (status && status !== "ready") {
        return {
          ok: false,
          code: "sandbox_create_failed",
          message: `boatdev: saved snapshot ${task.image} is ${status}, not ready`,
        };
      }
    } catch (e) {
      const pe = e instanceof ProviderError ? e : new ProviderError("unavailable", String(e));
      return {
        ok: false,
        code: pe.kind === "invalid" ? "invalid_configuration" : "sandbox_create_failed",
        message: pe.detail,
      };
    }
    return shape;
  }

  /** The deterministic create body for one task. */
  async createBody(task: SandboxTask): Promise<Record<string, unknown>> {
    const caps = await this.capabilities();
    const type = resolveInstanceType(caps, task.resources);
    const egress = isRestricted(task.networkPolicy) ? "in-sandbox" : "none";
    const env = sandboxEnv(task, egressLockFor(egress, task.networkPolicy), caps.loopbackDaemonPorts);
    env.OF_ENV_FILE = RUNNER_ENV_FILE;
    return {
      name: sandboxName(task.taskId),
      from: task.image,
      type: type.name,
      ttlSeconds: Math.min(MAX_TTL_SECONDS, task.maxSessionSeconds + TTL_GRACE_SECONDS),
      noEnv: true,
      snapshots: false,
      monitoring: true,
      labels: sandboxLabels(this.cfg.installId, task),
      env,
      setupScript: setupScript(Object.keys(env)),
    };
  }

  async idempotencyKey(taskId: string): Promise<string> {
    return sha256Hex(`${this.cfg.installId}:${taskId}`);
  }

  async start(task: SandboxTask): Promise<TaskStatus> {
    if (!task.env[TASK_ENV.bootstrapToken])
      throw new ProviderError("invalid", "boatdev: the task has no bootstrap token");
    const body = await this.createBody(task);
    const res = await this.http.call("POST", "/sandboxes", body, {
      "idempotency-key": await this.idempotencyKey(task.taskId),
    });
    this.reportRate(res);
    if (res.ok) {
      const sb = BoatdevProvider.unwrap(await res.json());
      this.ids.set(task.taskId, sb.id);
      return mapBoatSandbox(task.taskId, sb);
    }
    const err = await this.http.readError(res);
    if (err.status === 409 && (err.code === "idempotency_key_reused" || err.code === "idempotency_in_progress")) {
      const sb = await this.findByLabel(task.taskId);
      if (sb) return mapBoatSandbox(task.taskId, sb);
      if (err.code === "idempotency_in_progress") return { taskId: task.taskId, state: "accepted" };
    }
    throw boatError(err, "start");
  }

  async status(taskId: string): Promise<TaskStatus> {
    const sb = await this.get(taskId);
    if (!sb) throw new ProviderError("not_found", `boatdev: no sandbox for task ${taskId}`);
    return mapBoatSandbox(taskId, sb);
  }

  /**
   * Sends SIGTERM to the runner. The boat API documents no call to change the labels
   * of a sandbox, so nothing on the provider side records the cancel: status()
   * reports the sandbox state as boat sees it. WorkDO owns the cancel: it ends
   * the session when it sends the cancel and does not read the reason after.
   */
  async cancel(taskId: string, graceSeconds: number): Promise<void> {
    const sb = await this.get(taskId);
    if (!sb) return;
    const res = await this.http.call("POST", `/sandboxes/${encodeURIComponent(sb.id)}/commands`, {
      command: `sudo -n ${RUNNER_TERM_COMMAND}`,
      timeoutSeconds: Math.max(1, Math.min(15, Math.round(graceSeconds))),
    });
    if (res.status === 404) return;
    await this.http.json(res, "cancel");
  }

  async destroy(taskId: string): Promise<void> {
    const sb = await this.get(taskId);
    if (!sb) return;
    const res = await this.http.call("DELETE", `/sandboxes/${encodeURIComponent(sb.id)}`, undefined, {
      "x-ascii-confirm-delete": sb.id,
    });
    this.ids.delete(taskId);
    if (res.status === 404) return;
    if (!res.ok) throw await this.http.error(res, "destroy");
  }

  async list(): Promise<TaskStatus[]> {
    const out: TaskStatus[] = [];
    const pages = paginateCursor(async (cursor) => {
      const q = new URLSearchParams({ label: `of-install=${this.cfg.installId}`, limit: "200" });
      if (cursor) q.set("cursor", cursor);
      const b = await this.http.json<{
        sandboxes?: BoatSandbox[];
        items?: BoatSandbox[];
        data?: BoatSandbox[];
        pageInfo?: { nextCursor?: string | null };
      }>(await this.http.call("GET", `/sandboxes?${q}`), "list");
      return { items: b.sandboxes ?? b.items ?? b.data ?? [], cursor: b.pageInfo?.nextCursor };
    });
    for await (const sb of pages) {
      if (sb.labels?.["of-install"] !== this.cfg.installId || sb.state === "cancelled") continue;
      const label = sb.labels?.["of-task"];
      if (!label) continue;
      out.push(mapBoatSandbox(taskIdFromLabel(label), sb));
    }
    return out;
  }
}

/** Maps a failed boat response to a ProviderError. */
function boatError(e: HttpErrorInfo, what: string): ProviderError {
  const msg = `boatdev ${what}: ${e.status} ${e.code || ""} ${e.message}`.replace(/\s+/g, " ").trim();
  if (e.status === 429 && e.code === "limit_reached")
    return new ProviderError("at_capacity", msg, e.retryAfter, e.code);
  if (e.status === 429) return new ProviderError("rate_limited", msg, e.retryAfter, e.code || "rate_limited");
  if (e.status === 503 && e.code === "no_ready_machine")
    return new ProviderError("at_capacity", msg, e.retryAfter ?? 30, e.code);
  if (e.status === 402) return new ProviderError("unavailable", msg, undefined, e.code || "billing_required");
  if (e.status === 401) return new ProviderError("invalid", msg, undefined, e.code || "unauthorized");
  if (e.status === 403) return new ProviderError("invalid", msg, undefined, e.code);
  if (e.status === 404) {
    return e.code === "snapshot_not_found"
      ? new ProviderError("invalid", msg, undefined, e.code)
      : new ProviderError("not_found", msg, undefined, e.code);
  }
  if (e.status === 409) return new ProviderError("conflict", msg, undefined, e.code);
  if (e.status === 400 || e.status === 422) return new ProviderError("invalid", msg, undefined, e.code);
  return new ProviderError("unavailable", msg, e.retryAfter, e.code);
}

export function createBoatdevProvider(cfg: BoatdevConfig): BoatdevProvider {
  return new BoatdevProvider(cfg);
}
