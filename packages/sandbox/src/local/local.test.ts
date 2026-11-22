import { describe, expect, it } from "vitest";
import type { WorkerHubClient } from "../microvm/provider";
import { ProviderError, parseProviderError, type SandboxTask, type TaskStatus } from "../types";
import { LocalProvider, type DevWorker, type DevWorkerDirectory } from "./index";

class Hub implements WorkerHubClient {
  tasks = new Map<string, TaskStatus>();
  constructor(
    readonly id: string,
    public full = false,
  ) {}
  async taskAssign(t: SandboxTask): Promise<TaskStatus> {
    if (this.full) throw new ProviderError("at_capacity", "full");
    const s: TaskStatus = { taskId: t.taskId, state: "accepted" };
    this.tasks.set(t.taskId, s);
    return s;
  }
  async taskStatus(id: string): Promise<TaskStatus> {
    const s = this.tasks.get(id);
    if (!s) throw new ProviderError("not_found", id);
    return s;
  }
  async taskCancel(): Promise<void> {}
  async taskDestroy(id: string): Promise<void> {
    this.tasks.delete(id);
  }
  async taskList(): Promise<TaskStatus[]> {
    return [...this.tasks.values()];
  }
}

function setup(workers: DevWorker[]) {
  const hubs = new Map(workers.map((w) => [w.id, new Hub(w.id)]));
  const dir: DevWorkerDirectory = { list: async () => workers, hub: (id) => hubs.get(id)! };
  return { p: new LocalProvider(dir), hubs };
}

const t = (n: number): SandboxTask => ({
  taskId: `run_x.${n}`,
  runId: "run_x",
  session: n,
  teamId: "default",
  image: "",
  platform: { os: "linux", arch: "amd64" },
  resources: {},
  env: {},
  maxSessionSeconds: 60,
  failureRetentionSeconds: 0,
});

describe("LocalProvider", () => {
  it("declares egress none and process isolation", async () => {
    const { p } = setup([]);
    expect(await p.capabilities()).toMatchObject({ egress: "none", isolation: "process" });
  });

  it("refuses to start without a connected worker (requeue)", async () => {
    const { p } = setup([{ id: "dev", online: false, draining: false, slots: 1, used: 0 }]);
    const err = await p.start(t(1)).catch((e) => e);
    expect(parseProviderError(err)?.kind).toBe("at_capacity");
  });

  it("picks the worker with the most free slots and is idempotent", async () => {
    const { p, hubs } = setup([
      { id: "a", online: true, draining: false, slots: 2, used: 1 },
      { id: "b", online: true, draining: false, slots: 4, used: 1 },
      { id: "c", online: true, draining: true, slots: 8, used: 0 },
    ]);
    await p.start(t(1));
    expect(hubs.get("b")!.tasks.has("run_x.1")).toBe(true);
    await p.start(t(1));
    expect(hubs.get("b")!.tasks.size).toBe(1);
    expect((await p.status("run_x.1")).state).toBe("accepted");
    await p.destroy("run_x.1");
    await p.destroy("run_x.1");
    const err = await p.status("run_x.1").catch((e) => e);
    expect(parseProviderError(err)?.kind).toBe("not_found");
  });

  it("falls through to the next worker when one is at capacity", async () => {
    const { p, hubs } = setup([
      { id: "a", online: true, draining: false, slots: 0, used: 0 },
      { id: "b", online: true, draining: false, slots: 1, used: 0 },
    ]);
    hubs.get("a")!.full = true;
    await p.start(t(2));
    expect(hubs.get("b")!.tasks.has("run_x.2")).toBe(true);
    expect(await p.list()).toHaveLength(1);
  });
});
