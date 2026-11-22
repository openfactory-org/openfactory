// @openfactory/daytona: the Daytona sandbox provider (a library the control
// plane runs in-process). Spec: .owner/refactor/02-sandbox-providers.md §4.
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
  sanitizeTaskId,
  taskIdFromLabel,
  type Egress,
  type PreflightResult,
  type ProviderCapabilities,
  type SandboxProvider,
  type SandboxTask,
  type TaskNetworkPolicy,
  type TaskStatus,
} from "@openfactory/sandbox";

export const PROVIDER_ID = "daytona";
export const DEFAULT_API_URL = "https://app.daytona.io/api";
export const DEFAULT_TOOLBOX_URL = "https://proxy.app.daytona.io/toolbox";
export const MAX_DOMAIN_ALLOW_LIST = 100;
export const TTL_GRACE_SECONDS = 900;
/** Creates per minute by tier. */
export const CREATE_RATE_BY_TIER: Record<number, number> = { 1: 300, 2: 400, 3: 500, 4: 600 };
/** Port of the in-sandbox toolbox daemon (VERIFY with `ss -ltnp` in the live check). */
export const DEFAULT_DAEMON_PORTS = [2280];
export const ENTRYPOINT = ["/usr/bin/tini", "-g", "--", "/opt/openfactory/bootstrap.sh"];

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

/** Maps a Daytona sandbox (and the entrypoint exit code) to a TaskStatus (02 §4.4). */
export function mapDaytonaSandbox(
  taskId: string,
  sb: DaytonaSandbox,
  ctx: { exitCode?: number | null; cancelRequested?: boolean; nativeEgress?: boolean; now?: number } = {},
): TaskStatus {
  const st: TaskStatus = { taskId, state: "starting", sandboxId: sb.id, providerState: sb.state };
  if (sb.createdAt) st.startedAt = sb.createdAt;
  const exited = (reason: TaskStatus["reason"], message?: string | null, exitCode?: number): TaskStatus => {
    const out: TaskStatus = { ...st, state: "exited", reason };
    if (message) out.message = message;
    if (exitCode !== undefined) out.exitCode = exitCode;
    if (sb.updatedAt) out.endedAt = sb.updatedAt;
    return out;
  };
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
  private readonly api: string;
  private readonly toolbox: string;
  private readonly f: typeof fetch;
  private readonly cancelled = new Set<string>();

  constructor(private readonly cfg: DaytonaConfig) {
    if (!cfg.apiKey) throw new ProviderError("invalid", "daytona: the API key is empty");
    if (!cfg.installId) throw new ProviderError("invalid", "daytona: the install ID is empty");
    this.api = (cfg.apiUrl ?? DEFAULT_API_URL).replace(/\/$/, "");
    this.toolbox = (cfg.toolboxUrl ?? DEFAULT_TOOLBOX_URL).replace(/\/$/, "");
    this.f = cfg.fetch ?? ((i, init) => fetch(i, init));
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

  /** Chooses the egress mode for one task (02 §2.2 decision table). Throws ProviderError("invalid"). */
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

  private headers(): Record<string, string> {
    const h: Record<string, string> = { authorization: `Bearer ${this.cfg.apiKey}`, accept: "application/json" };
    if (this.cfg.organizationId) h["x-daytona-organization-id"] = this.cfg.organizationId;
    return h;
  }

  private async call(method: string, url: string, body?: unknown): Promise<Response> {
    const init: RequestInit = { method, headers: this.headers() };
    if (body !== undefined) {
      init.body = JSON.stringify(body);
      (init.headers as Record<string, string>)["content-type"] = "application/json";
    }
    try {
      return await this.f(url, init);
    } catch (e) {
      throw new ProviderError("unavailable", `daytona ${method} ${new URL(url).pathname}: ${(e as Error).message}`);
    }
  }

  private async toError(res: Response, what: string): Promise<ProviderError> {
    let message = res.statusText;
    try {
      const b = (await res.json()) as { message?: string | string[]; error?: string };
      message = (Array.isArray(b.message) ? b.message.join("; ") : b.message) ?? b.error ?? message;
    } catch {
      /* not JSON */
    }
    const msg = `daytona ${what}: ${res.status} ${message}`;
    const retry = parseRetryAfter(res.headers.get("retry-after-sandbox-create") ?? res.headers.get("retry-after"));
    if (res.status === 429) return new ProviderError("rate_limited", msg, retry);
    if ((res.status === 403 || res.status === 400) && /quota exceeded/i.test(message))
      return new ProviderError("at_capacity", msg, retry);
    if (res.status === 404) return new ProviderError("not_found", msg);
    if (res.status === 409) return new ProviderError("conflict", msg);
    if (res.status === 400 || res.status === 401 || res.status === 403 || res.status === 422)
      return new ProviderError("invalid", msg);
    return new ProviderError("unavailable", msg, retry);
  }

  private async json<T>(res: Response, what: string): Promise<T> {
    if (!res.ok) throw await this.toError(res, what);
    return (await res.json()) as T;
  }

  static sandboxName(taskId: string): string {
    return `of-${sanitizeTaskId(taskId)}`.slice(0, 63);
  }

  private async getSandbox(taskId: string): Promise<DaytonaSandbox | undefined> {
    const res = await this.call(
      "GET",
      `${this.api}/sandbox/${encodeURIComponent(DaytonaProvider.sandboxName(taskId))}`,
    );
    if (res.status === 404) return undefined;
    const sb = await this.json<DaytonaSandbox>(res, "status");
    // A name of another install never matches this task.
    if (sb.labels?.["of-install"] && sb.labels["of-install"] !== this.cfg.installId) return undefined;
    return sb;
  }

  private toolboxBase(sb: DaytonaSandbox): string {
    return `${(sb.toolboxProxyUrl ?? this.toolbox).replace(/\/$/, "")}/${encodeURIComponent(sb.id)}`;
  }

  private async entrypointExitCode(sb: DaytonaSandbox): Promise<number | null | undefined> {
    const res = await this.call("GET", `${this.toolboxBase(sb)}/process/session/entrypoint`);
    if (res.status === 404) return undefined;
    const s = await this.json<EntrypointSession>(res, "entrypoint");
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
    const res = await this.call("GET", `${this.api}/snapshots/${encodeURIComponent(name)}`);
    if (res.status === 404) throw new ProviderError("invalid", `daytona: snapshot ${name} does not exist`);
    const s = await this.json<{ id: string; state: string; errorReason?: string }>(res, "snapshot");
    switch (s.state) {
      case "active":
        return s.state;
      case "inactive": {
        await this.json(
          await this.call("POST", `${this.api}/snapshots/${encodeURIComponent(s.id)}/activate`),
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
    const res = await this.call("GET", `${this.api}/snapshots/${encodeURIComponent(opts.name)}`);
    if (res.ok) return;
    if (res.status !== 404) throw await this.toError(res, "snapshot");
    const body: Record<string, unknown> = {
      name: opts.name,
      imageName: opts.imageName,
      entrypoint: ENTRYPOINT,
      sandboxClass: opts.sandboxClass ?? "container",
    };
    if (opts.vcpus) body.cpu = opts.vcpus;
    if (opts.memoryGiB) body.memory = opts.memoryGiB;
    if (opts.diskGiB) body.disk = opts.diskGiB;
    await this.json(await this.call("POST", `${this.api}/snapshots`, body), "create snapshot");
  }

  /** The create body for one task (02 §4.3). */
  async createBody(task: SandboxTask): Promise<Record<string, unknown>> {
    const caps = await this.capabilities();
    const type = resolveInstanceType(caps, task.resources);
    const plan = this.planEgress(task.networkPolicy);
    const body: Record<string, unknown> = {
      name: DaytonaProvider.sandboxName(task.taskId),
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
    const res = await this.call("POST", `${this.api}/sandbox`, body);
    this.reportRate(res);
    if (res.ok) return mapDaytonaSandbox(task.taskId, (await res.json()) as DaytonaSandbox, this.ctx(task));
    const err = await this.toError(res, "start");
    // The name is the idempotency key: a concurrent create with the same name wins.
    if (err.kind === "conflict" || /already exists/i.test(err.detail)) {
      const sb = await this.getSandbox(task.taskId);
      if (sb) return this.statusOf(task.taskId, sb);
    }
    throw err;
  }

  private ctx(task?: SandboxTask): { cancelRequested: boolean; nativeEgress: boolean } {
    let native = false;
    if (task) {
      try {
        native = this.planEgress(task.networkPolicy).egress === "native";
      } catch {
        native = false;
      }
    }
    return { cancelRequested: task ? this.cancelled.has(task.taskId) : false, nativeEgress: native };
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
      cancelRequested: this.cancelled.has(taskId),
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
    this.cancelled.add(taskId);
    if (sb.state !== "started") return;
    const res = await this.call("POST", `${this.toolboxBase(sb)}/process/execute`, {
      command: "kill -TERM $(cat /run/openfactory/runner.pid)",
      timeout: 10,
    });
    if (res.status === 404) return;
    await this.json(res, "cancel");
  }

  async destroy(taskId: string): Promise<void> {
    const res = await this.call(
      "DELETE",
      `${this.api}/sandbox/${encodeURIComponent(DaytonaProvider.sandboxName(taskId))}`,
    );
    if (res.status === 404 || res.ok) return;
    throw await this.toError(res, "destroy");
  }

  async list(): Promise<TaskStatus[]> {
    const out: TaskStatus[] = [];
    let cursor: string | undefined;
    for (let page = 0; page < 1000; page++) {
      const q = new URLSearchParams({
        labels: JSON.stringify({ "of-install": this.cfg.installId }),
        includeErroredDeleted: "false",
        limit: "200",
      });
      if (cursor) q.set("cursor", cursor);
      const b = await this.json<DaytonaSandbox[] | { items?: DaytonaSandbox[]; nextCursor?: string | null }>(
        await this.call("GET", `${this.api}/sandbox?${q}`),
        "list",
      );
      const items = Array.isArray(b) ? b : (b.items ?? []);
      for (const sb of items) {
        if (sb.labels?.["of-install"] !== this.cfg.installId) continue;
        const label = sb.labels?.["of-task"];
        if (!label) continue;
        const taskId = taskIdFromLabel(label);
        // The list skips the toolbox call: the orphan sweep needs presence and state only.
        out.push(mapDaytonaSandbox(taskId, sb, { cancelRequested: this.cancelled.has(taskId) }));
      }
      cursor = Array.isArray(b) ? undefined : (b.nextCursor ?? undefined);
      if (!cursor) break;
    }
    return out;
  }
}

export function createDaytonaProvider(cfg: DaytonaConfig): DaytonaProvider {
  return new DaytonaProvider(cfg);
}
