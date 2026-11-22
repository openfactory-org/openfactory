import { describe, expect, it } from "vitest";
import { ProviderError, parseProviderError, type SandboxTask, type TaskStatus } from "../types";
import { MicroVMProvider, type WorkerHubClient } from "./provider";
import {
  failureForAssignError,
  failureForWorkerEnd,
  fromWireStatus,
  mergeStatus,
  providerErrorOf,
  toWireStatus,
  toWireTask,
  workerAuthMessage,
} from "./wire";

function task(over: Partial<SandboxTask> = {}): SandboxTask {
  return {
    taskId: "run_wabcdefghijklmnoabcdefghijkl.1",
    runId: "run_wabcdefghijklmnoabcdefghijkl",
    session: 1,
    teamId: "default",
    factoryId: "fac_1",
    image: "ghcr.io/example/sandbox:1",
    platform: { os: "linux", arch: "amd64" },
    resources: { vcpus: 2, memoryMiB: 4096 },
    env: { OF_CONTROL_URL: "https://cp.example.test", OF_RUN_ID: "r", OF_SESSION: "1", OF_BOOTSTRAP_TOKEN: "ofb_fake" },
    networkPolicy: {
      mode: "restricted",
      allowedHosts: ["api.example.test"],
      deniedHosts: [],
      alwaysAllow: ["cp.example.test"],
    },
    maxSessionSeconds: 3600,
    failureRetentionSeconds: 0,
    labels: { "of-task": "x" },
    ...over,
  };
}

class FakeHub implements WorkerHubClient {
  assigned: SandboxTask[] = [];
  async taskAssign(t: SandboxTask): Promise<TaskStatus> {
    this.assigned.push(t);
    return { taskId: t.taskId, state: "accepted" };
  }
  async taskStatus(id: string): Promise<TaskStatus> {
    throw new ProviderError("not_found", id);
  }
  async taskCancel(): Promise<void> {}
  async taskDestroy(): Promise<void> {}
  async taskList(): Promise<TaskStatus[]> {
    return [];
  }
}

describe("wire conversions", () => {
  it("converts a task to the worker schema shape", () => {
    const w = toWireTask(task());
    expect(w.task_id).toBe("run_wabcdefghijklmnoabcdefghijkl.1");
    expect(w.resources).toEqual({ cpu: "2", memory: "4096Mi" });
    expect(w.network_policy).toEqual({
      mode: "restricted",
      allowed_hosts: ["api.example.test", "cp.example.test"],
      denied_hosts: [],
    });
    expect(w.platform).toEqual({ os: "linux", arch: "amd64" });
    expect(w.env.OF_BOOTSTRAP_TOKEN).toBe("ofb_fake");
  });

  it("round-trips a status and rejects unknown states", () => {
    const s: TaskStatus = {
      taskId: "r.1",
      state: "exited",
      reason: "container_oom",
      exitCode: 137,
      endedAt: "2026-11-22T00:00:00Z",
    };
    expect(fromWireStatus(toWireStatus(s))).toEqual(s);
    expect(fromWireStatus({ task_id: "r.1", state: "gone" })).toBeNull();
    expect(fromWireStatus({ task_id: "r.1", state: "exited" })?.reason).toBe("unknown");
  });

  it("merges statuses forward only; exited is final", () => {
    const running: TaskStatus = { taskId: "r.1", state: "running" };
    expect(mergeStatus(running, { taskId: "r.1", state: "starting" })).toBe(running);
    const ex: TaskStatus = { taskId: "r.1", state: "exited", reason: "completed" };
    expect(mergeStatus(ex, { taskId: "r.1", state: "exited", reason: "unknown" })).toBe(ex);
    expect(mergeStatus(running, ex)).toBe(ex);
  });

  it("signs the same bytes as the Go worker", () => {
    expect(new TextDecoder().decode(workerAuthMessage("w1", "conn_1", "nonce"))).toBe(
      "openfactory-worker-auth-v1\nw1\nconn_1\nnonce\n",
    );
  });
});

describe("failure mapping", () => {
  const end = (reason: TaskStatus["reason"]): TaskStatus => ({
    taskId: "r.1",
    state: "exited",
    ...(reason ? { reason } : {}),
  });
  it.each([
    ["image_pull", "sandbox_create_failed"],
    ["container_oom", "out_of_memory"],
    ["node_lost", "sandbox_lost"],
    ["unknown", "runner_lost"],
    ["deadline_exceeded", "infrastructure_timeout"],
    ["process_failed", "agent_process_failed"],
    ["cancelled", undefined],
  ] as const)("%s → %s", (reason, code) => {
    expect(failureForWorkerEnd(end(reason))).toBe(code);
  });

  it("maps assign errors", () => {
    expect(failureForAssignError("at_capacity")).toBe("resource_unavailable");
    expect(failureForAssignError("invalid_spec")).toBe("invalid_configuration");
    expect(failureForAssignError("internal")).toBe("sandbox_create_failed");
  });

  it("rebuilds provider errors from response errors", () => {
    const e = providerErrorOf({ code: "at_capacity", message: "draining" });
    expect(parseProviderError(new Error(e.message))?.kind).toBe("at_capacity");
    expect(providerErrorOf({ code: "invalid_spec", message: "x" }).kind).toBe("invalid");
    expect(providerErrorOf({ code: "boom", message: "x" }).kind).toBe("internal");
  });
});

describe("MicroVMProvider", () => {
  const shape = { arch: ["amd64" as const], slots: 2, slotSize: { vcpus: 2, memoryMiB: 4096, scratchGiB: 20 } };

  it("declares native egress and the slot as its instance type", async () => {
    const p = new MicroVMProvider(new FakeHub(), () => shape);
    const caps = await p.capabilities();
    expect(caps.egress).toBe("native");
    expect(caps.isolation).toBe("microvm");
    expect(caps.slots).toBe(2);
    expect(caps.instanceTypes).toEqual([{ name: "slot", vcpus: 2, memoryMiB: 4096, diskMiB: 20480 }]);
  });

  it("refuses a machine larger than the slot and a foreign arch", async () => {
    const p = new MicroVMProvider(new FakeHub(), () => shape);
    expect(await p.preflight(task())).toMatchObject({ ok: true });
    expect(await p.preflight(task({ resources: { vcpus: 4 } }))).toMatchObject({
      ok: false,
      code: "invalid_configuration",
    });
    expect(await p.preflight(task({ resources: { memoryMiB: 8192 } }))).toMatchObject({
      ok: false,
      code: "invalid_configuration",
    });
    expect(await p.preflight(task({ platform: { os: "linux", arch: "arm64" } }))).toMatchObject({ ok: false });
  });

  it("starts through the hub", async () => {
    const hub = new FakeHub();
    const p = new MicroVMProvider(hub, () => shape);
    expect((await p.start(task())).state).toBe("accepted");
    expect(hub.assigned).toHaveLength(1);
  });
});
