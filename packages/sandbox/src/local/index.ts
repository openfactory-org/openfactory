// The local SandboxProvider (OF_SANDBOX_PROVIDER=local): development process
// workers (`openfactory-runtime worker --process`) connected to the control
// plane. The control plane allows them only with OF_DEV_PROCESS_WORKERS=true.
// They run agents on the host without isolation, so egress is "none".
// Owner: WS6.
import type { WorkerHubClient } from "../microvm/provider";
import {
  ProviderError,
  parseProviderError,
  type Arch,
  type PreflightResult,
  type ProviderCapabilities,
  type SandboxProvider,
  type SandboxTask,
  type TaskStatus,
} from "../types";

/** A development worker as the directory sees it. */
export interface DevWorker {
  id: string;
  online: boolean;
  draining: boolean;
  /** 0 means no limit. */
  slots: number;
  used: number;
  arch?: Arch[];
}

/** Where the local provider finds development workers and their hubs. */
export interface DevWorkerDirectory {
  list(): Promise<DevWorker[]>;
  hub(workerId: string): WorkerHubClient;
}

function freeSlots(w: DevWorker): number {
  return w.slots === 0 ? Number.MAX_SAFE_INTEGER : w.slots - w.used;
}

export class LocalProvider implements SandboxProvider {
  readonly id = "local";

  constructor(private readonly dir: DevWorkerDirectory) {}

  async capabilities(): Promise<ProviderCapabilities> {
    return {
      egress: "none",
      image: "oci",
      isolation: "process",
      arch: ["amd64", "arm64"],
      maxLifetimeSeconds: null,
      startRate: [],
      instanceTypes: [],
      customResources: true,
      loopbackDaemonPorts: [],
    };
  }

  async preflight(task: SandboxTask): Promise<PreflightResult> {
    const workers = (await this.dir.list()).filter((w) => w.online);
    if (
      workers.length > 0 &&
      !workers.some((w) => !w.arch || w.arch.length === 0 || w.arch.includes(task.platform.arch))
    ) {
      return { ok: false, code: "invalid_configuration", message: `no development worker runs ${task.platform.arch}` };
    }
    return { ok: true };
  }

  /** Idempotent: a worker that already holds the task answers for it. */
  async start(task: SandboxTask): Promise<TaskStatus> {
    const workers = await this.dir.list();
    const held = await this.find(task.taskId, workers);
    if (held) return held.status;
    const candidates = workers
      .filter((w) => w.online && !w.draining && freeSlots(w) > 0)
      .filter((w) => !w.arch || w.arch.length === 0 || w.arch.includes(task.platform.arch))
      .sort((a, b) => freeSlots(b) - freeSlots(a) || a.id.localeCompare(b.id));
    if (candidates.length === 0) {
      throw new ProviderError("at_capacity", "no development process worker with a free slot is connected");
    }
    let last: unknown;
    for (const w of candidates) {
      try {
        return await this.dir.hub(w.id).taskAssign(task);
      } catch (e) {
        last = e;
        if (parseProviderError(e)?.kind !== "at_capacity") throw e;
      }
    }
    throw last;
  }

  async status(taskId: string): Promise<TaskStatus> {
    const found = await this.find(taskId, await this.dir.list());
    if (!found) throw new ProviderError("not_found", `task ${taskId} is not known to any development worker`);
    return found.status;
  }

  async cancel(taskId: string, graceSeconds: number): Promise<void> {
    const found = await this.find(taskId, await this.dir.list());
    if (!found) throw new ProviderError("not_found", `task ${taskId} is not known to any development worker`);
    await this.dir.hub(found.workerId).taskCancel(taskId, graceSeconds);
  }

  async destroy(taskId: string): Promise<void> {
    const found = await this.find(taskId, await this.dir.list());
    if (!found) return;
    await this.dir.hub(found.workerId).taskDestroy(taskId);
  }

  async list(): Promise<TaskStatus[]> {
    const out: TaskStatus[] = [];
    for (const w of await this.dir.list()) out.push(...(await this.dir.hub(w.id).taskList()));
    return out;
  }

  private async find(taskId: string, workers: DevWorker[]): Promise<{ workerId: string; status: TaskStatus } | null> {
    for (const w of workers) {
      try {
        return { workerId: w.id, status: await this.dir.hub(w.id).taskStatus(taskId) };
      } catch (e) {
        if (parseProviderError(e)?.kind !== "not_found") throw e;
      }
    }
    return null;
  }
}
