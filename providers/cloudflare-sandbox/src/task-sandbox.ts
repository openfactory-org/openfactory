// The state machine of one task sandbox (OfTaskSandbox, 02 §5.2–§5.4).
// Platform objects are injected, so tests run it with a fake container.

import { sha256Hex } from "@openfactory/core";
import { INTERCEPT_CA, REPORT_URL, recordBlocked, type BlockedHost, type EgressProps } from "./egress";
import {
  ProviderError,
  TASK_ENV,
  isIpLiteral,
  isRestricted,
  reasonForExitCode,
  sandboxEnv,
  type InstanceType,
  type SandboxReason,
  type SandboxTask,
  type TaskStatus,
} from "@openfactory/sandbox";
import { BOOTSTRAP_ENTRYPOINT, exitedStatus, parseExitCode } from "@openfactory/sandbox/provider-kit";

export const KEEPALIVE_MS = 60_000;
/** The largest inactivity timeout the platform allows. */
export const INACTIVITY_TIMEOUT_MS = 6 * 3600 * 1000;
export const BOOT_GRACE_MS = 120_000;
/** Slack after maxSessionSeconds before the sandbox gets SIGTERM. */
export const DEADLINE_SLACK_MS = 300_000;

/** The subset of the platform Container the state machine uses. */
export interface ContainerLike {
  readonly running: boolean;
  readonly images: Record<string, string>;
  start(options: {
    image: string;
    entrypoint: string[];
    env: Record<string, string>;
    enableInternet: boolean;
    instance?: string | { vcpu: number; memoryMib: number; diskMb: number };
    labels?: Record<string, string>;
  }): void;
  monitor(): Promise<void>;
  destroy(reason?: unknown): Promise<void>;
  signal(signo: number): void;
  setInactivityTimeout(ms: number): Promise<void>;
  interceptAllOutboundHttp(binding: Fetcher): Promise<void>;
  interceptOutboundHttps(addr: string, binding: Fetcher): Promise<void>;
}

/** Synchronous KV of a SQLite-backed Durable Object (ctx.storage.kv). */
export interface KvLike {
  get<T>(key: string): T | undefined;
  put<T>(key: string, value: T): void;
  delete(key: string): boolean;
}

export interface TaskSandboxDeps {
  container: ContainerLike;
  kv: KvLike;
  setAlarm(at: number): void;
  deleteAlarm(): void;
  now(): number;
  /** A Fetcher for the Egress entrypoint with these props (ctx.exports.Egress({props})). */
  egress(props: EgressProps): Fetcher;
  registry: { add(taskId: string): Promise<void>; remove(taskId: string): Promise<void> };
  waitUntil(p: Promise<unknown>): void;
  /** Image key in container.images. Default "runner". */
  imageKey?: string;
}

type Outcome =
  | { kind: "exit"; exitCode: number }
  | { kind: "error"; message: string; exitCode?: number }
  | { kind: "start_failed"; reason: SandboxReason; message: string }
  | { kind: "lost" }
  | { kind: "destroyed"; reason: "cancelled" | "deadline_exceeded" };

interface Record_ {
  task: SandboxTask;
  instance: InstanceType;
  startedAt: number;
  seenRunning?: boolean;
  cancelRequested?: boolean;
  cancelAt?: number;
  deadlineSignalled?: boolean;
  outcome?: Outcome;
  endedAt?: number;
}

const K_TASK = "task";
const K_BLOCKED = "blocked";

/** Short label value for the platform (labels are at most 64 bytes). */
async function shortHash(s: string): Promise<string> {
  return (await sha256Hex(s)).slice(0, 16);
}

function exitCodeOf(e: unknown): number | undefined {
  const c = (e as { exitCode?: unknown })?.exitCode;
  if (typeof c === "number") return c;
  return parseExitCode(String((e as Error)?.message ?? e));
}

function hostOf(url: string | undefined): string | undefined {
  if (!url) return undefined;
  try {
    return new URL(url).hostname;
  } catch {
    return undefined;
  }
}

export class TaskSandbox {
  constructor(private readonly d: TaskSandboxDeps) {}

  private get rec(): Record_ | undefined {
    return this.d.kv.get<Record_>(K_TASK);
  }

  private save(r: Record_): void {
    this.d.kv.put(K_TASK, r);
  }

  /** Call from the DO constructor: a DO restart clears the inactivity timeout. */
  async onStartup(): Promise<void> {
    const r = this.rec;
    if (!r || r.outcome) return;
    if (this.d.container.running) {
      await this.d.container.setInactivityTimeout(INACTIVITY_TIMEOUT_MS);
      this.watch();
    }
    this.d.setAlarm(this.d.now() + KEEPALIVE_MS);
  }

  /** Lists the image keys of the container binding (preflight; never starts a container). */
  images(): string[] {
    return Object.keys(this.d.container.images ?? {});
  }

  async start(task: SandboxTask, instance: InstanceType): Promise<TaskStatus> {
    if (this.rec) return this.status();
    const policy = task.networkPolicy;
    if (isRestricted(policy)) {
      const ip = policy.allowedHosts.find(isIpLiteral);
      if (ip)
        throw new ProviderError(
          "invalid",
          `cloudflare-sandbox: the allow list has the IP literal ${ip}; the HTTPS intercept cannot express it`,
        );
    }
    const controlHost = hostOf(task.env[TASK_ENV.controlUrl]);
    if (!controlHost) throw new ProviderError("invalid", "cloudflare-sandbox: OF_CONTROL_URL is missing or not a URL");
    const now = this.d.now();
    this.save({ task, instance, startedAt: now });

    // Intercepts first: no request may leave before them.
    const props: EgressProps = { taskId: task.taskId, controlHost };
    if (policy) props.policy = policy;
    const runnerHost = hostOf(task.env[TASK_ENV.runnerUrl]);
    if (runnerHost) props.bootstrapHosts = [runnerHost];
    const out = this.d.egress(props);
    await this.d.container.interceptAllOutboundHttp(out);
    await this.d.container.interceptOutboundHttps("*", out);

    const key = this.d.imageKey ?? "runner";
    const images = this.d.container.images ?? {};
    const image = images[task.image] ?? images[key];
    if (!image) {
      return this.fail(
        "image_pull",
        `cloudflare-sandbox: the image ${task.image || key} is not in the container binding`,
      );
    }
    const env = sandboxEnv(task, "none", []);
    env.OF_TRUST_CA = INTERCEPT_CA;
    env[TASK_ENV.netguardReport] = REPORT_URL;
    try {
      this.d.container.start({
        image,
        entrypoint: [...BOOTSTRAP_ENTRYPOINT],
        env,
        enableInternet: false,
        instance:
          instance.name === "custom"
            ? { vcpu: instance.vcpus, memoryMib: instance.memoryMiB, diskMb: instance.diskMiB ?? 2000 }
            : instance.name,
        labels: { t: await shortHash(task.taskId) },
      });
    } catch (e) {
      if (e instanceof TypeError || e instanceof RangeError) return this.fail("invalid_spec", e.message);
      throw new ProviderError("unavailable", `cloudflare-sandbox: start: ${(e as Error).message}`);
    }
    await this.d.container.setInactivityTimeout(INACTIVITY_TIMEOUT_MS);
    this.d.setAlarm(now + KEEPALIVE_MS);
    this.watch();
    await this.d.registry.add(task.taskId);
    return this.status();
  }

  private async fail(reason: SandboxReason, message: string): Promise<TaskStatus> {
    const r = this.rec!;
    r.outcome = { kind: "start_failed", reason, message };
    r.endedAt = this.d.now();
    this.save(r);
    await this.d.registry.add(r.task.taskId);
    this.d.setAlarm(r.endedAt + r.task.failureRetentionSeconds * 1000);
    return this.status();
  }

  /** Stores the outcome of the container process when monitor() settles. */
  private watch(): void {
    this.d.waitUntil(
      this.d.container.monitor().then(
        () => this.settle({ kind: "exit", exitCode: 0 }),
        (e: unknown) => {
          const exitCode = exitCodeOf(e);
          const o: Outcome = { kind: "error", message: String((e as Error)?.message ?? e) };
          if (exitCode !== undefined) o.exitCode = exitCode;
          this.settle(o);
        },
      ),
    );
  }

  private settle(o: Outcome): void {
    const r = this.rec;
    if (!r || r.outcome) return;
    r.outcome = o;
    r.endedAt = this.d.now();
    this.save(r);
  }

  status(): TaskStatus {
    const r = this.rec;
    if (!r) throw new ProviderError("not_found", "cloudflare-sandbox: no such task");
    const st: TaskStatus = {
      taskId: r.task.taskId,
      state: "starting",
      sandboxId: r.task.taskId,
      startedAt: new Date(r.startedAt).toISOString(),
    };
    const endedAt = r.endedAt ? new Date(r.endedAt).toISOString() : undefined;
    const exited = (reason: SandboxReason, message?: string, exitCode?: number): TaskStatus =>
      exitedStatus(st, reason, { message, exitCode, endedAt });
    const o = r.outcome;
    if (o) {
      st.providerState = o.kind;
      switch (o.kind) {
        case "start_failed":
          return exited(o.reason, o.message);
        case "destroyed":
          return exited(o.reason);
        case "lost":
          return exited("evicted", "the container stopped without an exit status (platform event)");
        case "exit":
          if (r.cancelRequested && o.exitCode !== 0) return exited("cancelled", undefined, o.exitCode);
          return exited(reasonForExitCode(o.exitCode), undefined, o.exitCode);
        case "error": {
          if (r.cancelRequested) return exited("cancelled", o.message, o.exitCode);
          if (o.exitCode !== undefined) return exited(reasonForExitCode(o.exitCode), o.message, o.exitCode);
          if (!r.seenRunning && /image/i.test(o.message)) return exited("image_pull", o.message);
          if (!r.seenRunning && /capacity|no.*available/i.test(o.message)) return exited("unschedulable", o.message);
          return exited("evicted", o.message);
        }
      }
    }
    if (this.d.container.running) {
      if (!r.seenRunning) {
        r.seenRunning = true;
        this.save(r);
      }
      return { ...st, state: "running", providerState: "running" };
    }
    if (!r.seenRunning && this.d.now() - r.startedAt < BOOT_GRACE_MS) return { ...st, providerState: "booting" };
    return exited("evicted", "the container is not running and reported no exit status");
  }

  async cancel(graceSeconds: number): Promise<void> {
    const r = this.rec;
    if (!r || r.outcome) return;
    const now = this.d.now();
    r.cancelRequested = true;
    r.cancelAt = now + Math.max(0, graceSeconds) * 1000;
    this.save(r);
    if (this.d.container.running) this.d.container.signal(15);
    this.d.setAlarm(Math.min(now + KEEPALIVE_MS, r.cancelAt));
  }

  /** Removes the container and all state. Safe to call twice. */
  async destroy(): Promise<void> {
    const r = this.rec;
    if (!r) return;
    if (this.d.container.running) await this.d.container.destroy("cancelled");
    this.d.kv.delete(K_TASK);
    this.d.kv.delete(K_BLOCKED);
    this.d.deleteAlarm();
    await this.d.registry.remove(r.task.taskId);
  }

  /** The runner started its session: forget the bootstrap token. */
  ack(): void {
    const r = this.rec;
    if (!r || !r.task.env[TASK_ENV.bootstrapToken]) return;
    delete r.task.env[TASK_ENV.bootstrapToken];
    this.save(r);
  }

  recordBlocked(host: string): void {
    this.d.kv.put(K_BLOCKED, recordBlocked(this.blocked(), host, new Date(this.d.now())));
  }

  blocked(): BlockedHost[] {
    return this.d.kv.get<BlockedHost[]>(K_BLOCKED) ?? [];
  }

  /** Keepalive, deadline, eviction detection and cleanup. Never throws; always sets the next alarm. */
  async alarm(): Promise<void> {
    const now = this.d.now();
    let next: number | undefined = now + KEEPALIVE_MS;
    try {
      const r = this.rec;
      if (!r) {
        next = undefined;
        return;
      }
      if (r.outcome) {
        if (r.task.env[TASK_ENV.bootstrapToken]) this.ack();
        const until = (r.endedAt ?? now) + r.task.failureRetentionSeconds * 1000;
        if (now >= until) {
          await this.destroy();
          next = undefined;
        } else {
          next = until;
        }
        return;
      }
      const c = this.d.container;
      if (!c.running) {
        if (r.seenRunning || now - r.startedAt >= BOOT_GRACE_MS) {
          r.outcome = { kind: "lost" };
          r.endedAt = now;
          this.save(r);
          next = now + r.task.failureRetentionSeconds * 1000;
        }
        return;
      }
      if (!r.seenRunning) {
        r.seenRunning = true;
        this.save(r);
      }
      await c.setInactivityTimeout(INACTIVITY_TIMEOUT_MS);
      if (r.cancelRequested && r.cancelAt !== undefined && now >= r.cancelAt) {
        await c.destroy("cancelled");
        r.outcome = { kind: "destroyed", reason: "cancelled" };
        r.endedAt = now;
        this.save(r);
        next = now + r.task.failureRetentionSeconds * 1000;
        return;
      }
      const deadline = r.startedAt + r.task.maxSessionSeconds * 1000 + DEADLINE_SLACK_MS;
      if (r.deadlineSignalled) {
        await c.destroy("deadline");
        r.outcome = { kind: "destroyed", reason: "deadline_exceeded" };
        r.endedAt = now;
        this.save(r);
        next = now + r.task.failureRetentionSeconds * 1000;
      } else if (now >= deadline) {
        c.signal(15);
        r.deadlineSignalled = true;
        this.save(r);
      }
    } catch (e) {
      console.error("cloudflare-sandbox: alarm", e);
    } finally {
      if (next !== undefined) this.d.setAlarm(next);
    }
  }
}
