// @openfactory/daytona: the Daytona sandbox provider (a library the control
// plane runs in-process). See docs/sandbox-providers.md.
//
// Facts that shape it:
// - Tiers 1 and 2 are refused: their org policy allows only Daytona's
//   "essential services", which exclude *.workers.dev, and they refuse a
//   per-sandbox allow list.
// - `autoStopInterval: 0` (the runner is not API activity) plus `ttlMinutes`
//   as a hard leak bound.
// - The sandbox name `of-<task>` is the idempotency key (create has no
//   idempotency header).
// - Snapshots go inactive after 2 weeks unused: preflight reactivates them.
// - Native egress is the domain allow list (at most 100 entries, no IP
//   literals, TLS re-encrypted with Daytona's CA). A policy that does not fit
//   uses the in-sandbox lock when it is available, else egress "none".
// - Default per-sandbox maximum 4 vCPU, 8 GiB, 10 GiB disk. linux/amd64 only.

import {
  ProviderError,
  TASK_ENV,
  effectiveAllowedHosts,
  egressLockFor,
  isIpLiteral,
  isRestricted,
  normalizeAlwaysAllow,
  parseRetryAfter,
  preflightTask,
  reasonForExitCode,
  resolveInstanceType,
  sandboxEnv,
  sandboxLabels,
  taskIdFromLabel,
  type Egress,
  type PreflightResult,
  type ProviderCapabilities,
  type SandboxProvider,
  type SandboxTask,
  type TaskNetworkPolicy,
  type TaskStatus,
} from "@openfactory/sandbox";
import {
  BOOTSTRAP_ENTRYPOINT,
  ProviderHttpClient,
  RUNNER_TERM_COMMAND,
  TTL_GRACE_SECONDS,
  exitedStatus,
  paginateCursor,
  sandboxName,
  type HttpErrorInfo,
} from "@openfactory/sandbox/provider-kit";

export const PROVIDER_ID = "daytona";
export const DEFAULT_API_URL = "https://app.daytona.io/api";
export const DEFAULT_TOOLBOX_URL = "https://proxy.app.daytona.io/toolbox";
export const MAX_DOMAIN_ALLOW_LIST = 100;
/** Creates per minute by tier. */
export const CREATE_RATE_BY_TIER: Record<number, number> = { 1: 300, 2: 400, 3: 500, 4: 600 };
/** Port of the in-sandbox toolbox daemon (VERIFY with `ss -ltnp` in the live check). */
export const DEFAULT_DAEMON_PORTS = [2280];
/**
 * Label that cancel() sets on the sandbox. The control plane builds a new
 * provider object for each call, so the cancel request must live on the
 * provider side for a later status() or list() to report "cancelled".
 */
export const CANCEL_LABEL = "of-cancel";

export interface DaytonaConfig {
  apiKey: string;
  installId: string;
  /** Organization tier 1–4. Tiers 1 and 2 are refused. */
  tier: number;
  /** Shared region: "eu" or "us". */
  target?: string;
  apiUrl?: string;
  toolboxUrl?: string;
  /** Needed only with a JWT. */
  organizationId?: string;
  fetch?: typeof fetch;
  /** auto (default): native when the policy fits, else in-sandbox, else none. */
  egress?: "auto" | "native" | "in-sandbox" | "none";
  /** The live check proved the in-sandbox lock on this sandbox class. Default true. */
  inSandboxLock?: boolean;
  /** OF_ALLOW_UNENFORCED_NETWORK_POLICY of the control plane. */
  allowUnenforcedNetworkPolicy?: boolean | string;
  /** Org per-sandbox maximum. Default 4 vCPU / 8 GiB / 10 GiB. */
  maxResources?: { vcpus: number; memoryMiB: number; diskMiB: number };
  loopbackDaemonPorts?: number[];
  onRateLimit?: (info: { provider: string; remaining?: string; retryAfterSeconds?: number }) => void;
}

/** Fields of the Daytona Sandbox schema that the adapter reads. */
export interface DaytonaSandbox {
  id: string;
  name?: string;
  state: string;
  desiredState?: string;
  errorReason?: string | null;
  recoverable?: boolean;
  networkPolicyError?: string | null;
  autoDestroyAt?: string | null;
  spotEvictedAt?: string | null;
  queueTimedOutAt?: string | null;
  createdAt?: string;
  updatedAt?: string;
  labels?: Record<string, string>;
  toolboxProxyUrl?: string;
}

export interface EntrypointSession {
  sessionId?: string;
  commands?: { id?: string; command?: string; exitCode?: number | null }[];
}

export interface DaytonaEgressPlan {
  egress: Egress;
  domainAllowList?: string[];
  reason?: string;
}

/** Native list entries for a policy, or the reason it does not fit. */
export function domainAllowList(policy: TaskNetworkPolicy): { list?: string[]; reason?: string } {
  const out = new Set<string>();
  for (const h of effectiveAllowedHosts(policy)) {
    if (isIpLiteral(h)) return { reason: `the allow list has the IP literal ${h}, which a domain list cannot express` };
    out.add(h);
  }
  for (const a of normalizeAlwaysAllow(policy.alwaysAllow)) {
    // Daytona "*.x" covers the base and subdomains. For "*.x" that widens to the base: accepted.
    const base = a.startsWith("*.") ? a.slice(2) : a.startsWith(".") ? a.slice(1) : a;
    if (isIpLiteral(base)) return { reason: `the platform host ${base} is an IP literal` };
    out.add(a.startsWith("*.") || a.startsWith(".") ? `*.${base}` : base);
  }
  const list = [...out].sort();
  if (list.length > MAX_DOMAIN_ALLOW_LIST) {
    return {
      reason: `the allow list has ${list.length} entries, more than the ${MAX_DOMAIN_ALLOW_LIST} of a Daytona domain list`,
    };
  }
  return { list };
}

const STARTING = new Set([
  "unknown",
  "pending_build",
  "building_snapshot",
  "pulling_snapshot",
  "creating",
  "starting",
  "restoring",
]);
const STOPPED = new Set(["stopping", "stopped", "archiving", "archived", "destroying", "destroyed"]);
const TRANSIENT_RUNNING = new Set(["resizing", "snapshotting", "forking", "pausing", "resuming"]);

/** Maps a Daytona sandbox (and the entrypoint exit code) to a TaskStatus. */
export function mapDaytonaSandbox(
  taskId: string,
  sb: DaytonaSandbox,
  ctx: { exitCode?: number | null; cancelRequested?: boolean; nativeEgress?: boolean; now?: number } = {},
): TaskStatus {
  const st: TaskStatus = { taskId, state: "starting", sandboxId: sb.id, providerState: sb.state };
  if (sb.createdAt) st.startedAt = sb.createdAt;
  const exited = (reason: TaskStatus["reason"], message?: string | null, exitCode?: number): TaskStatus =>
    exitedStatus(st, reason, { message, exitCode, endedAt: sb.updatedAt });
  const now = ctx.now ?? Date.now();
  if (sb.queueTimedOutAt) return exited("unschedulable", "the sandbox queue timed out");
  if (sb.state === "build_failed") return exited("image_pull", sb.errorReason);
  if (ctx.nativeEgress && sb.networkPolicyError) return exited("network_policy_timeout", sb.networkPolicyError);
  if (STARTING.has(sb.state)) return st;
  if (sb.state === "started") {
    const code = ctx.exitCode;
    if (code === undefined || code === null) return { ...st, state: "running" };
    if (code === 137 && /oom|out of memory/i.test(sb.errorReason ?? ""))
      return exited("container_oom", sb.errorReason, code);
    if (ctx.cancelRequested && code !== 0) return exited("cancelled", undefined, code);
    return exited(reasonForExitCode(code), undefined, code);
  }
  if (STOPPED.has(sb.state)) {
    if (ctx.cancelRequested) return exited("cancelled");
    if (sb.autoDestroyAt && Date.parse(sb.autoDestroyAt) <= now)
      return exited("deadline_exceeded", "the sandbox reached its ttl");
    return exited("evicted", sb.spotEvictedAt ? "the sandbox was evicted" : sb.errorReason);
  }
  if (sb.state === "error") return exited(sb.recoverable ? "node_lost" : "unknown", sb.errorReason);
  if (TRANSIENT_RUNNING.has(sb.state)) return { ...st, state: "running" };
  if (sb.state === "paused") return exited("evicted", "the sandbox was paused");
  return { ...st, state: "running" };
}

const GiB = 1024;

export class DaytonaProvider implements SandboxProvider {
  readonly id = PROVIDER_ID;
  private readonly http: ProviderHttpClient;
  private readonly toolbox: string;

  constructor(private readonly cfg: DaytonaConfig) {
    if (!cfg.apiKey) throw new ProviderError("invalid", "daytona: the API key is empty");
    if (!cfg.installId) throw new ProviderError("invalid", "daytona: the install ID is empty");
    this.http = new ProviderHttpClient({
      provider: PROVIDER_ID,
      baseUrl: cfg.apiUrl ?? DEFAULT_API_URL,
      apiKey: cfg.apiKey,
      ...(cfg.organizationId ? { headers: { "x-daytona-organization-id": cfg.organizationId } } : {}),
      ...(cfg.fetch ? { fetch: cfg.fetch } : {}),
      retryAfterHeaders: ["retry-after-sandbox-create", "retry-after"],
      toError: daytonaError,
    });
    this.toolbox = (cfg.toolboxUrl ?? DEFAULT_TOOLBOX_URL).replace(/\/$/, "");
  }

  private get tierOk(): boolean {
    return this.cfg.tier >= 3;
  }

  async capabilities(): Promise<ProviderCapabilities> {
    const max = this.cfg.maxResources ?? { vcpus: 4, memoryMiB: 8 * GiB, diskMiB: 10 * GiB };
    const mode = this.cfg.egress ?? "auto";
    const egress: Egress = !this.tierOk || mode === "none" ? "none" : mode === "in-sandbox" ? "in-sandbox" : "native";
    const caps: ProviderCapabilities = {
      egress,
      image: "snapshot",
      isolation: "container",
      arch: ["amd64"],
      maxLifetimeSeconds: null,
      startRate: [{ limit: CREATE_RATE_BY_TIER[this.cfg.tier] ?? 300, windowSeconds: 60 }],
      instanceTypes: [
        { name: "default", vcpus: 1, memoryMiB: 1 * GiB, diskMiB: 3 * GiB },
        { name: "max", vcpus: max.vcpus, memoryMiB: max.memoryMiB, diskMiB: max.diskMiB },
      ],
      customResources: true,
      loopbackDaemonPorts: this.cfg.loopbackDaemonPorts ?? DEFAULT_DAEMON_PORTS,
      regions: [this.cfg.target ?? "eu"],
    };
    if (egress === "native") {
      caps.nativeEgress = {
        maxEntries: MAX_DOMAIN_ALLOW_LIST,
        wildcard: "base-and-subdomains",
        ipLiterals: false,
        nonHttp: "blocked",
        tlsInterception: true,
      };
    }
    return caps;
  }

  /** Chooses the egress mode for one task. Throws ProviderError("invalid"). */
  planEgress(policy: TaskNetworkPolicy | undefined): DaytonaEgressPlan {
    if (!isRestricted(policy)) return { egress: "none" };
    const mode = this.cfg.egress ?? "auto";
    const lock = this.cfg.inSandboxLock ?? true;
    if (mode === "none" || !this.tierOk)
      return {
        egress: "none",
        reason: this.tierOk ? "egress is off by configuration" : "tier 1 and 2 cannot set a per-sandbox allow list",
      };
    if (mode === "in-sandbox")
      return lock ? { egress: "in-sandbox" } : { egress: "none", reason: "the in-sandbox lock is not available" };
    const fit = domainAllowList(policy);
    if (fit.list) return { egress: "native", domainAllowList: fit.list };
    if (mode === "native") throw new ProviderError("invalid", `daytona: ${fit.reason}`);
    return lock ? { egress: "in-sandbox", reason: fit.reason! } : { egress: "none", reason: fit.reason! };
  }

  // ---------- HTTP ----------

  private async getSandbox(taskId: string): Promise<DaytonaSandbox | undefined> {
    const res = await this.http.call("GET", `/sandbox/${encodeURIComponent(sandboxName(taskId))}`);
    if (res.status === 404) return undefined;
    const sb = await this.http.json<DaytonaSandbox>(res, "status");
    // A name of another install never matches this task.
    if (sb.labels?.["of-install"] && sb.labels["of-install"] !== this.cfg.installId) return undefined;
    return sb;
  }

  private toolboxBase(sb: DaytonaSandbox): string {
    return `${(sb.toolboxProxyUrl ?? this.toolbox).replace(/\/$/, "")}/${encodeURIComponent(sb.id)}`;
  }

  private async entrypointExitCode(sb: DaytonaSandbox): Promise<number | null | undefined> {
    const res = await this.http.call("GET", `${this.toolboxBase(sb)}/process/session/entrypoint`);
    if (res.status === 404) return undefined;
    const s = await this.http.json<EntrypointSession>(res, "entrypoint");
    const last = s.commands?.at(-1);
    return last?.exitCode ?? undefined;
  }

  // ---------- SandboxProvider ----------

  async preflight(task: SandboxTask): Promise<PreflightResult> {
    if (!this.tierOk) {
      return {
        ok: false,
        code: "invalid_configuration",
        message: `daytona: organization tier ${this.cfg.tier} cannot reach the control plane (tier 1 and 2 allow only Daytona's essential services). Use tier 3 or higher`,
      };
    }
    const caps = await this.capabilities();
    let egress: Egress;
    try {
      egress = this.planEgress(task.networkPolicy).egress;
    } catch (e) {
      return { ok: false, code: "invalid_configuration", message: (e as ProviderError).detail };
    }
    const allow = this.cfg.allowUnenforcedNetworkPolicy ?? false;
    const shape = preflightTask(caps, task, { allowUnenforced: allow, egress });
    if (!shape.ok) return shape;
    try {
      await this.ensureSnapshotActive(task.image);
    } catch (e) {
      const pe = e instanceof ProviderError ? e : new ProviderError("unavailable", String(e));
      return {
        ok: false,
        code: pe.kind === "invalid" || pe.kind === "not_found" ? "invalid_configuration" : "sandbox_create_failed",
        message: pe.detail,
      };
    }
    return shape;
  }

  /** Reactivates an inactive snapshot (snapshots go inactive after 2 weeks unused). */
  async ensureSnapshotActive(name: string): Promise<string> {
    const res = await this.http.call("GET", `/snapshots/${encodeURIComponent(name)}`);
    if (res.status === 404) throw new ProviderError("invalid", `daytona: snapshot ${name} does not exist`);
    const s = await this.http.json<{ id: string; state: string; errorReason?: string }>(res, "snapshot");
    switch (s.state) {
      case "active":
        return s.state;
      case "inactive": {
        await this.http.json(
          await this.http.call("POST", `/snapshots/${encodeURIComponent(s.id)}/activate`),
          "activate snapshot",
        );
        return "activating";
      }
      case "error":
      case "build_failed":
        throw new ProviderError(
          "invalid",
          `daytona: snapshot ${name} is in state ${s.state}: ${s.errorReason ?? ""}`.trim(),
        );
      default:
        throw new ProviderError("unavailable", `daytona: snapshot ${name} is ${s.state}, not active yet`);
    }
  }

  /** Creates the snapshot from an OCI image when it does not exist (used by the image pipeline). */
  async ensureSnapshot(opts: {
    name: string;
    imageName: string;
    vcpus?: number;
    memoryGiB?: number;
    diskGiB?: number;
    sandboxClass?: "container" | "linux-vm";
  }): Promise<void> {
    const res = await this.http.call("GET", `/snapshots/${encodeURIComponent(opts.name)}`);
    if (res.ok) return;
    if (res.status !== 404) throw await this.http.error(res, "snapshot");
    const body: Record<string, unknown> = {
      name: opts.name,
      imageName: opts.imageName,
      entrypoint: [...BOOTSTRAP_ENTRYPOINT],
      sandboxClass: opts.sandboxClass ?? "container",
    };
    if (opts.vcpus) body.cpu = opts.vcpus;
    if (opts.memoryGiB) body.memory = opts.memoryGiB;
    if (opts.diskGiB) body.disk = opts.diskGiB;
    await this.http.json(await this.http.call("POST", "/snapshots", body), "create snapshot");
  }

  /** The create body for one task. */
  async createBody(task: SandboxTask): Promise<Record<string, unknown>> {
    const caps = await this.capabilities();
    const type = resolveInstanceType(caps, task.resources);
    const plan = this.planEgress(task.networkPolicy);
    const body: Record<string, unknown> = {
      name: sandboxName(task.taskId),
      snapshot: task.image,
      user: "root",
      target: this.cfg.target ?? "eu",
      cpu: Math.max(1, Math.ceil(type.vcpus)),
      memory: Math.max(1, Math.ceil(type.memoryMiB / GiB)),
      disk: Math.max(1, Math.ceil((type.diskMiB ?? 3 * GiB) / GiB)),
      env: sandboxEnv(task, egressLockFor(plan.egress, task.networkPolicy), caps.loopbackDaemonPorts),
      labels: sandboxLabels(this.cfg.installId, task),
      autoStopInterval: 0,
      autoArchiveInterval: 0,
      autoDeleteInterval: Math.ceil(task.failureRetentionSeconds / 60),
      ttlMinutes: Math.ceil((task.maxSessionSeconds + TTL_GRACE_SECONDS) / 60),
      queueTimeout: 10,
      public: false,
    };
    // VERIFY in the live check: the API takes the list as a comma-separated string.
    if (plan.egress === "native" && plan.domainAllowList) body.domainAllowList = plan.domainAllowList.join(",");
    return body;
  }

  async start(task: SandboxTask): Promise<TaskStatus> {
    if (!task.env[TASK_ENV.bootstrapToken])
      throw new ProviderError("invalid", "daytona: the task has no bootstrap token");
    const existing = await this.getSandbox(task.taskId);
    if (existing) return this.statusOf(task.taskId, existing);
    const body = await this.createBody(task);
    const res = await this.http.call("POST", "/sandbox", body);
    this.reportRate(res);
    if (res.ok) {
      const sb = (await res.json()) as DaytonaSandbox;
      return mapDaytonaSandbox(task.taskId, sb, {
        cancelRequested: cancelRequested(sb),
        nativeEgress: this.nativeFor(task),
      });
    }
    const err = await this.http.error(res, "start");
    // The name is the idempotency key: a concurrent create with the same name wins.
    if (err.kind === "conflict" || /already exists/i.test(err.detail)) {
      const sb = await this.getSandbox(task.taskId);
      if (sb) return this.statusOf(task.taskId, sb);
    }
    throw err;
  }

  private nativeFor(task: SandboxTask): boolean {
    try {
      return this.planEgress(task.networkPolicy).egress === "native";
    } catch {
      return false;
    }
  }

  private reportRate(res: Response): void {
    if (!this.cfg.onRateLimit) return;
    const remaining = res.headers.get("x-ratelimit-remaining-sandbox-create") ?? undefined;
    const retry = parseRetryAfter(res.headers.get("retry-after-sandbox-create"));
    if (remaining === undefined && retry === undefined) return;
    const info: { provider: string; remaining?: string; retryAfterSeconds?: number } = { provider: PROVIDER_ID };
    if (remaining !== undefined) info.remaining = remaining;
    if (retry !== undefined) info.retryAfterSeconds = retry;
    this.cfg.onRateLimit(info);
  }

  private async statusOf(taskId: string, sb: DaytonaSandbox): Promise<TaskStatus> {
    const exitCode = sb.state === "started" ? await this.entrypointExitCode(sb) : undefined;
    // networkPolicyError matters only when a native list was requested; the sandbox has one then.
    return mapDaytonaSandbox(taskId, sb, {
      exitCode,
      cancelRequested: cancelRequested(sb),
      nativeEgress: !!sb.networkPolicyError,
    });
  }

  async status(taskId: string): Promise<TaskStatus> {
    const sb = await this.getSandbox(taskId);
    if (!sb) throw new ProviderError("not_found", `daytona: no sandbox for task ${taskId}`);
    return this.statusOf(taskId, sb);
  }

  async cancel(taskId: string, _graceSeconds: number): Promise<void> {
    const sb = await this.getSandbox(taskId);
    if (!sb) return;
    await this.markCancelled(sb);
    if (sb.state !== "started") return;
    const res = await this.http.call("POST", `${this.toolboxBase(sb)}/process/execute`, {
      command: RUNNER_TERM_COMMAND,
      timeout: 10,
    });
    if (res.status === 404) return;
    await this.http.json(res, "cancel");
  }

  /**
   * Sets CANCEL_LABEL. The call replaces all labels, so it sends the current
   * ones too: of-install and of-task drive list() and the orphan sweep.
   * VERIFY in the live check: PUT /sandbox/{idOrName}/labels with {labels}.
   */
  private async markCancelled(sb: DaytonaSandbox): Promise<void> {
    if (cancelRequested(sb)) return;
    const labels = { ...(sb.labels ?? {}), [CANCEL_LABEL]: "1" };
    const res = await this.http.call("PUT", `/sandbox/${encodeURIComponent(sb.id)}/labels`, { labels });
    if (res.status === 404) return;
    if (!res.ok) throw await this.http.error(res, "cancel label");
    sb.labels = labels;
  }

  async destroy(taskId: string): Promise<void> {
    const res = await this.http.call("DELETE", `/sandbox/${encodeURIComponent(sandboxName(taskId))}`);
    if (res.status === 404 || res.ok) return;
    throw await this.http.error(res, "destroy");
  }

  async list(): Promise<TaskStatus[]> {
    const out: TaskStatus[] = [];
    const pages = paginateCursor(async (cursor) => {
      const q = new URLSearchParams({
        labels: JSON.stringify({ "of-install": this.cfg.installId }),
        includeErroredDeleted: "false",
        limit: "200",
      });
      if (cursor) q.set("cursor", cursor);
      const b = await this.http.json<DaytonaSandbox[] | { items?: DaytonaSandbox[]; nextCursor?: string | null }>(
        await this.http.call("GET", `/sandbox?${q}`),
        "list",
      );
      return Array.isArray(b) ? { items: b } : { items: b.items ?? [], cursor: b.nextCursor };
    });
    for await (const sb of pages) {
      if (sb.labels?.["of-install"] !== this.cfg.installId) continue;
      const label = sb.labels?.["of-task"];
      if (!label) continue;
      // The list skips the toolbox call: the orphan sweep needs presence and state only.
      out.push(mapDaytonaSandbox(taskIdFromLabel(label), sb, { cancelRequested: cancelRequested(sb) }));
    }
    return out;
  }
}

/** Maps a failed Daytona response to a ProviderError. */
function daytonaError(e: HttpErrorInfo, what: string): ProviderError {
  const msg = `daytona ${what}: ${e.status} ${e.message}`;
  if (e.status === 429) return new ProviderError("rate_limited", msg, e.retryAfter);
  if ((e.status === 403 || e.status === 400) && /quota exceeded/i.test(e.message))
    return new ProviderError("at_capacity", msg, e.retryAfter);
  if (e.status === 404) return new ProviderError("not_found", msg);
  if (e.status === 409) return new ProviderError("conflict", msg);
  if (e.status === 400 || e.status === 401 || e.status === 403 || e.status === 422)
    return new ProviderError("invalid", msg);
  return new ProviderError("unavailable", msg, e.retryAfter);
}

function cancelRequested(sb: DaytonaSandbox): boolean {
  return sb.labels?.[CANCEL_LABEL] === "1";
}

export function createDaytonaProvider(cfg: DaytonaConfig): DaytonaProvider {
  return new DaytonaProvider(cfg);
}
