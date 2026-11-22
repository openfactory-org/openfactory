// The microvm SandboxProvider: one enrolled worker host (Firecracker microVM
// per session) behind its WorkerHubDO. The control plane gives it a
// WorkerHubClient (the typed RPC stub of the hub), so this file has no
// dependency on the control-plane bindings.
import type { Arch, PreflightResult, ProviderCapabilities, SandboxProvider, SandboxTask, TaskStatus } from "../types";

/** What the provider needs of a WorkerHubDO (a subset of WorkerHubRpc). */
export interface WorkerHubClient {
  taskAssign(task: SandboxTask, extras?: Record<string, unknown>): Promise<TaskStatus>;
  taskStatus(taskId: string): Promise<TaskStatus>;
  taskCancel(taskId: string, graceSeconds: number): Promise<void>;
  taskDestroy(taskId: string): Promise<void>;
  taskList(): Promise<TaskStatus[]>;
}

/** The size of one slot of a worker. */
export interface SlotSize {
  vcpus: number;
  memoryMiB: number;
  scratchGiB?: number;
}

/** Facts of the worker record that shape capabilities and preflight. */
export interface WorkerShape {
  /** Architectures of the host (one, from its facts). Empty: not known yet. */
  arch: Arch[];
  slots: number;
  slotSize?: SlotSize;
}

export class MicroVMProvider implements SandboxProvider {
  readonly id = "microvm";

  constructor(
    private readonly hub: WorkerHubClient,
    private readonly shape: () => Promise<WorkerShape> | WorkerShape,
  ) {}

  async capabilities(): Promise<ProviderCapabilities> {
    const s = await this.shape();
    const caps: ProviderCapabilities = {
      egress: "native",
      image: "oci",
      isolation: "microvm",
      arch: s.arch.length > 0 ? [...s.arch] : ["amd64", "arm64"],
      maxLifetimeSeconds: null,
      startRate: [],
      instanceTypes: s.slotSize
        ? [
            {
              name: "slot",
              vcpus: s.slotSize.vcpus,
              memoryMiB: s.slotSize.memoryMiB,
              ...(s.slotSize.scratchGiB ? { diskMiB: s.slotSize.scratchGiB * 1024 } : {}),
            },
          ]
        : [],
      customResources: false,
      loopbackDaemonPorts: [],
      slots: s.slots,
    };
    return caps;
  }

  /** Refuses a machine spec larger than the slot size and an arch the host does not have. */
  async preflight(task: SandboxTask): Promise<PreflightResult> {
    const s = await this.shape();
    if (s.arch.length > 0 && !s.arch.includes(task.platform.arch)) {
      return {
        ok: false,
        code: "invalid_configuration",
        message: `the worker runs ${s.arch.join(", ")}, the run asks for ${task.platform.arch}`,
      };
    }
    const slot = s.slotSize;
    if (slot) {
      if (task.resources.vcpus !== undefined && task.resources.vcpus > slot.vcpus) {
        return {
          ok: false,
          code: "invalid_configuration",
          message: `the machine asks for ${task.resources.vcpus} vCPUs; a slot of this worker has ${slot.vcpus}`,
        };
      }
      if (task.resources.memoryMiB !== undefined && task.resources.memoryMiB > slot.memoryMiB) {
        return {
          ok: false,
          code: "invalid_configuration",
          message: `the machine asks for ${task.resources.memoryMiB} MiB of memory; a slot of this worker has ${slot.memoryMiB} MiB`,
        };
      }
    }
    return { ok: true, resources: { ...task.resources, instanceType: "slot" } };
  }

  start(task: SandboxTask): Promise<TaskStatus> {
    return this.hub.taskAssign(task);
  }

  status(taskId: string): Promise<TaskStatus> {
    return this.hub.taskStatus(taskId);
  }

  cancel(taskId: string, graceSeconds: number): Promise<void> {
    return this.hub.taskCancel(taskId, graceSeconds);
  }

  destroy(taskId: string): Promise<void> {
    return this.hub.taskDestroy(taskId);
  }

  list(): Promise<TaskStatus[]> {
    return this.hub.taskList();
  }
}
