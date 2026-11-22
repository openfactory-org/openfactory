// Conformance suite for SandboxProvider implementations. A provider test runs
// it against a fetch mock (or a fake container) and, opt-in, against a live
// account:
//
//   describeProviderConformance("daytona (mock)", async () => ({ provider, task: sampleTask }));
//
// The suite checks the contract of packages/sandbox/src/types.ts: capability
// shape, preflight refusals, idempotent start, not_found on unknown tasks,
// list for the orphan sweep, and safe double destroy.

import { afterAll, describe, expect, it } from "vitest";
import {
  SANDBOX_REASONS,
  TASK_STATES,
  parseProviderError,
  type Arch,
  type SandboxProvider,
  type SandboxTask,
  type TaskStatus,
} from "../types";
import { largestInstanceType } from "../gate/index";

export interface ConformanceHarness {
  provider: SandboxProvider;
  /** A task the provider can start. Each call must return a new taskId. */
  task: (overrides?: Partial<SandboxTask>) => SandboxTask;
  /** Live accounts: wait longer and clean up. */
  live?: boolean;
  /** Called after each test that started tasks. */
  cleanup?: () => Promise<void>;
}

let seq = 0;

/** A task with the bootstrap variables set. Clearly fake values. */
export function sampleTask(overrides: Partial<SandboxTask> = {}): SandboxTask {
  seq += 1;
  const runId = overrides.runId ?? `run_0000000000000000conf${Date.now().toString(36)}${seq}`;
  const session = overrides.session ?? 1;
  return {
    taskId: `${runId}.${session}`,
    runId,
    session,
    teamId: "default",
    image: "of-sandbox-test",
    platform: { os: "linux", arch: "amd64" },
    resources: {},
    env: {
      OF_CONTROL_URL: "https://control.example.test",
      OF_RUN_ID: runId,
      OF_SESSION: String(session),
      OF_BOOTSTRAP_TOKEN: "ofb_default.test.not-a-real-token",
      OF_RUNNER_VERSION: "0.0.0-test",
      OF_RUNNER_URL: "https://control.example.test/runner/v1/bin/linux-amd64",
      OF_RUNNER_SHA256: "0".repeat(64),
    },
    maxSessionSeconds: 3600,
    failureRetentionSeconds: 600,
    ...overrides,
  };
}

function expectStatus(s: TaskStatus, taskId: string): void {
  expect(s.taskId).toBe(taskId);
  expect(TASK_STATES).toContain(s.state);
  if (s.reason !== undefined) expect(SANDBOX_REASONS).toContain(s.reason);
}

async function expectNotFound(p: Promise<unknown>): Promise<void> {
  let err: unknown;
  try {
    await p;
  } catch (e) {
    err = e;
  }
  expect(err, "expected a not_found ProviderError").toBeDefined();
  expect(parseProviderError(err)?.kind).toBe("not_found");
}

export function describeProviderConformance(name: string, setup: () => Promise<ConformanceHarness>): void {
  describe(`SandboxProvider conformance: ${name}`, () => {
    let h: ConformanceHarness | undefined;
    const harness = async (): Promise<ConformanceHarness> => (h ??= await setup());
    const timeout = () => (h?.live ? 300_000 : 10_000);

    afterAll(async () => {
      await h?.cleanup?.();
    });

    it("declares an id and valid capabilities", async () => {
      const { provider } = await harness();
      expect(typeof provider.id).toBe("string");
      expect(provider.id.length).toBeGreaterThan(0);
      const caps = await provider.capabilities();
      expect(["native", "in-sandbox", "none"]).toContain(caps.egress);
      expect(["oci", "snapshot"]).toContain(caps.image);
      expect(caps.arch.length).toBeGreaterThan(0);
      expect(caps.instanceTypes.length).toBeGreaterThan(0);
      for (const t of caps.instanceTypes) {
        expect(t.vcpus).toBeGreaterThan(0);
        expect(t.memoryMiB).toBeGreaterThan(0);
      }
      expect(Array.isArray(caps.loopbackDaemonPorts)).toBe(true);
      for (const w of caps.startRate) expect(w.limit > 0 && w.windowSeconds > 0).toBe(true);
      if (caps.nativeEgress) expect(caps.egress).toBe("native");
    });

    it(
      "preflight accepts a default task and names an instance type",
      async () => {
        const { provider, task } = await harness();
        const r = await provider.preflight(task());
        expect(r, JSON.stringify(r)).toMatchObject({ ok: true });
        if (r.ok && r.resources) expect(r.resources.instanceType).toBeTruthy();
      },
      timeout(),
    );

    it(
      "preflight refuses a machine larger than the largest instance type",
      async () => {
        const { provider, task } = await harness();
        const caps = await provider.capabilities();
        const max = largestInstanceType(caps.instanceTypes)!;
        const r = await provider.preflight(
          task({ resources: { vcpus: max.vcpus * 64, memoryMiB: max.memoryMiB * 64 } }),
        );
        expect(r).toMatchObject({ ok: false, code: "invalid_configuration" });
      },
      timeout(),
    );

    it(
      "preflight refuses an arch the provider does not run",
      async () => {
        const { provider, task } = await harness();
        const caps = await provider.capabilities();
        const other = (["amd64", "arm64"] as Arch[]).find((a) => !caps.arch.includes(a));
        if (!other) return;
        const r = await provider.preflight(task({ platform: { os: "linux", arch: other } }));
        expect(r).toMatchObject({ ok: false, code: "invalid_configuration" });
      },
      timeout(),
    );

    it(
      "status of an unknown task is not_found",
      async () => {
        const { provider, task } = await harness();
        await expectNotFound(provider.status(task().taskId));
      },
      timeout(),
    );

    it(
      "destroy of an unknown task succeeds",
      async () => {
        const { provider, task } = await harness();
        await provider.destroy(task().taskId);
      },
      timeout(),
    );

    it(
      "start is idempotent, list sees the task, destroy is safe twice",
      async () => {
        const { provider, task } = await harness();
        const t = task();
        const a = await provider.start(t);
        expectStatus(a, t.taskId);
        const b = await provider.start(t);
        expectStatus(b, t.taskId);
        if (a.sandboxId !== undefined) expect(b.sandboxId).toBe(a.sandboxId);

        const s = await provider.status(t.taskId);
        expectStatus(s, t.taskId);
        if (a.sandboxId !== undefined && s.sandboxId !== undefined) expect(s.sandboxId).toBe(a.sandboxId);

        const listed = await provider.list();
        expect(listed.filter((x) => x.taskId === t.taskId)).toHaveLength(1);

        await provider.cancel(t.taskId, 10);
        await provider.destroy(t.taskId);
        await provider.destroy(t.taskId);
        try {
          const after = await provider.status(t.taskId);
          expect(after.state).toBe("exited");
        } catch (e) {
          expect(parseProviderError(e)?.kind).toBe("not_found");
        }
      },
      timeout(),
    );
  });
}
