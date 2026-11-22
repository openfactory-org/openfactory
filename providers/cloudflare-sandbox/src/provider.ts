// The SandboxProvider logic of the cloudflare-sandbox Worker. The
// WorkerEntrypoint in index.ts delegates to it; tests drive it with fakes.

import {
  TASK_ENV,
  isIpLiteral,
  isRestricted,
  parseProviderError,
  preflightTask,
  resolveInstanceType,
  type InstanceType,
  type PreflightResult,
  type ProviderCapabilities,
  type SandboxProvider,
  type SandboxTask,
  type TaskStatus,
} from "@openfactory/sandbox";
import { paginateCursor } from "@openfactory/sandbox/provider-kit";
import type { RegistryPage } from "./registry";

export const PROVIDER_ID = "cloudflare-sandbox";

/** How many sandbox DO status calls list() runs at once. */
export const LIST_PARALLEL = 16;

const GiB = 1024;
/** Platform instance types. The account maximum is 4 vCPU / 12 GiB / 20 GB. */
export const CF_INSTANCE_TYPES: InstanceType[] = [
  { name: "lite", vcpus: 0.0625, memoryMiB: 256, diskMiB: 2000 },
  { name: "basic", vcpus: 0.25, memoryMiB: 1 * GiB, diskMiB: 4000 },
  { name: "standard-1", vcpus: 0.5, memoryMiB: 4 * GiB, diskMiB: 8000 },
  { name: "standard-2", vcpus: 1, memoryMiB: 6 * GiB, diskMiB: 12000 },
  { name: "standard-3", vcpus: 2, memoryMiB: 8 * GiB, diskMiB: 16000 },
  { name: "standard-4", vcpus: 4, memoryMiB: 12 * GiB, diskMiB: 20000 },
];

/** The RPC surface of one OfTaskSandbox stub. */
export interface TaskSandboxStub {
  start(task: SandboxTask, instance: InstanceType): Promise<TaskStatus>;
  status(): Promise<TaskStatus>;
  cancel(graceSeconds: number): Promise<void>;
  destroy(): Promise<void>;
  ack(): Promise<void>;
  images(): Promise<string[]>;
}

export interface RegistryStub {
  list(cursor?: string): Promise<RegistryPage>;
  remove(taskId: string): Promise<void>;
}

export interface ProviderDeps {
  sandbox(taskId: string): TaskSandboxStub;
  registry(): RegistryStub;
  /** Service binding to the control-plane Worker. */
  controlPlane?: Fetcher;
  /** OF_RUNNER_VERSION of this deploy (must equal the control plane's). */
  runnerVersion?: string;
  imageKey?: string;
  regions?: string[];
}

/** Rebuilds a ProviderError that lost its class over RPC. */
function rethrow(e: unknown): never {
  throw parseProviderError(e) ?? e;
}

export class CloudflareSandboxProvider implements SandboxProvider {
  readonly id = PROVIDER_ID;
  constructor(private readonly d: ProviderDeps) {}

  async capabilities(): Promise<ProviderCapabilities> {
    return {
      egress: "native",
      nativeEgress: {
        maxEntries: null,
        wildcard: "base-and-subdomains",
        ipLiterals: false,
        nonHttp: "blocked",
        tlsInterception: true,
      },
      image: "oci",
      isolation: "container",
      arch: ["amd64"],
      maxLifetimeSeconds: null,
      startRate: [],
      instanceTypes: CF_INSTANCE_TYPES,
      customResources: true,
      loopbackDaemonPorts: [],
      regions: this.d.regions ?? ["auto"],
    };
  }

  async preflight(task: SandboxTask): Promise<PreflightResult> {
    const caps = await this.capabilities();
    const shape = preflightTask(caps, task);
    if (!shape.ok) return shape;
    if (isRestricted(task.networkPolicy)) {
      const ip = task.networkPolicy.allowedHosts.find(isIpLiteral);
      if (ip) {
        return {
          ok: false,
          code: "invalid_configuration",
          message: `cloudflare-sandbox: the allow list has the IP literal ${ip}; use host names`,
        };
      }
    }
    const want = task.env[TASK_ENV.runnerVersion];
    if (want && this.d.runnerVersion && want !== this.d.runnerVersion) {
      return {
        ok: false,
        code: "invalid_configuration",
        message: `cloudflare-sandbox: the provider Worker has runner version ${this.d.runnerVersion}, the control plane wants ${want}. Deploy both from the same release`,
      };
    }
    if (this.d.controlPlane) {
      try {
        const res = await this.d.controlPlane.fetch("https://control-plane.internal/api/v1/version", { method: "GET" });
        if (res.status >= 500)
          return {
            ok: false,
            code: "sandbox_create_failed",
            message: `cloudflare-sandbox: the control-plane binding answered ${res.status}`,
          };
      } catch (e) {
        return {
          ok: false,
          code: "sandbox_create_failed",
          message: `cloudflare-sandbox: the control-plane binding failed: ${(e as Error).message}`,
        };
      }
    }
    try {
      const keys = await this.d.sandbox("preflight").images();
      const key = this.d.imageKey ?? "runner";
      if (!keys.includes(task.image) && !keys.includes(key)) {
        return {
          ok: false,
          code: "invalid_configuration",
          message: `cloudflare-sandbox: no image ${task.image} or ${key} in the container binding`,
        };
      }
    } catch (e) {
      return {
        ok: false,
        code: "sandbox_create_failed",
        message: `cloudflare-sandbox: image check: ${(e as Error).message}`,
      };
    }
    return shape;
  }

  async start(task: SandboxTask): Promise<TaskStatus> {
    const caps = await this.capabilities();
    const instance = resolveInstanceType(caps, task.resources);
    return this.d.sandbox(task.taskId).start(task, instance).catch(rethrow);
  }

  async status(taskId: string): Promise<TaskStatus> {
    return this.d.sandbox(taskId).status().catch(rethrow);
  }

  async cancel(taskId: string, graceSeconds: number): Promise<void> {
    await this.d.sandbox(taskId).cancel(graceSeconds).catch(rethrow);
  }

  async destroy(taskId: string): Promise<void> {
    await this.d.sandbox(taskId).destroy().catch(rethrow);
  }

  /** The control plane saw session.started: the sandbox DO forgets the bootstrap token. */
  async ack(taskId: string): Promise<void> {
    await this.d.sandbox(taskId).ack().catch(rethrow);
  }

  /**
   * The status of every registered task, in registry order. Status calls run in
   * batches of LIST_PARALLEL. A failure other than not_found throws the error
   * of the first failed task in registry order.
   */
  async list(): Promise<TaskStatus[]> {
    const out: TaskStatus[] = [];
    const reg = this.d.registry();
    const statusOf = async (id: string): Promise<TaskStatus | undefined> => {
      try {
        return await this.d.sandbox(id).status();
      } catch (e) {
        if (parseProviderError(e)?.kind !== "not_found") throw e;
        await reg.remove(id); // stale index row
        return undefined;
      }
    };
    let batch: string[] = [];
    const flush = async (): Promise<void> => {
      const settled = await Promise.allSettled(batch.map(statusOf));
      batch = [];
      for (const r of settled) {
        if (r.status === "rejected") throw r.reason;
        if (r.value) out.push(r.value);
      }
    };
    const ids = paginateCursor(async (cursor) => {
      const page = await reg.list(cursor);
      return { items: page.taskIds, cursor: page.cursor };
    });
    for await (const id of ids) {
      batch.push(id);
      if (batch.length >= LIST_PARALLEL) await flush();
    }
    await flush();
    return out;
  }
}
