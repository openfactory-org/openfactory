// Workers: REST (list, enrollment, drain, resume, revoke) and the worker
// protocol v2 over WebSocket with a fake worker.
import { afterAll, beforeAll, expect, it } from "vitest";
import { Api, uniq, waitFor } from "../harness/api.ts";
import { adminApi, createTestFactory } from "../harness/context.ts";
import { FakeRunner } from "../harness/runner.ts";
import { assertWorkerDef } from "../harness/schemas.ts";
import { describeFeature, TAGS } from "../harness/target.ts";
import { FakeWorker, newWorkerKey, type WorkerKeyPair } from "../harness/worker.ts";

interface Worker {
  id: string;
  name: string;
  status?: string;
}

interface Enrolled {
  workerId: string;
  name: string;
  key: WorkerKeyPair;
}

/** Enrollment token, then POST /workers/enroll. */
async function enroll(api: Api, opts: { labels?: Record<string, string>; slots?: number } = {}): Promise<Enrolled> {
  const name = uniq("ct-w");
  const enrollment = await api.post<{ id: string; token: string; expires_at: string }>(
    "/workers/enrollments",
    { name, labels: opts.labels ?? {}, expires_in_seconds: 600 },
    201,
  );
  expect(enrollment.token).toMatch(/^ofe_/);
  const key = newWorkerKey();
  const enrolled = await api.post<{ worker_id: string; control_url: string }>(
    "/workers/enroll",
    {
      enrollment_token: enrollment.token,
      name,
      public_key: key.publicKeyB64,
      facts: { arch: "amd64", os: "linux", vcpus: 4, memory_mib: 8192, kvm: true },
      labels: opts.labels ?? {},
      slots: opts.slots ?? 2,
      slot_size: { vcpus: 1, memory_mib: 1024 },
    },
    201,
    { key: null },
  );
  return { workerId: enrolled.worker_id, name, key };
}

/** Enrolls a worker and connects it as a fake worker with Ed25519 auth. */
async function connectWorker(
  api: Api,
  opts: { labels?: Record<string, string> } = {},
): Promise<{ worker: FakeWorker; id: string; name: string }> {
  const e = await enroll(api, opts);
  const worker = await FakeWorker.connect(
    { kind: "ed25519", workerId: e.workerId, privateKey: e.key.privateKey },
    { workerId: e.workerId, name: e.name, backend: "microvm", capacity: 2, slots: 2, labels: opts.labels },
  );
  return { worker, id: e.workerId, name: e.name };
}

describeFeature([TAGS.rest], "workers list", () => {
  it("GET /workers", async () => {
    const list = await adminApi().get<{ items: Worker[] }>("/workers");
    expect(Array.isArray(list.items)).toBe(true);
  });
});

describeFeature([TAGS.workerProtocol], "worker protocol v2", () => {
  const open: FakeWorker[] = [];
  afterAll(() => {
    for (const w of open) w.close();
  });

  it("connect, hello, welcome, heartbeat; the worker is online", async () => {
    const { worker, id, name } = await connectWorker(adminApi(), { labels: { gpu: "none" } });
    open.push(worker);
    assertWorkerDef("welcome", worker.welcome);
    expect(worker.welcome.heartbeat_ms).toBeGreaterThanOrEqual(0);
    worker.heartbeat(0, 2);
    const w = await waitFor("worker online in GET /workers", async () => {
      const list = await adminApi().get<{ items: Worker[] }>("/workers");
      return list.items.find((x) => (x.id === id || x.name === name) && x.status === "online");
    });
    expect(w.name).toBe(name);
    expect(worker.invalid).toEqual([]);
  });

  it("refuses a connection without credentials", async () => {
    const name = uniq("ct-anon");
    await expect(
      FakeWorker.connect({ kind: "bearer", token: "" }, { workerId: name, backend: "local", capacity: 1 }, 5000),
    ).rejects.toThrow();
  });
});

describeFeature([TAGS.workerProtocol, TAGS.workerDispatch], "worker dispatch", () => {
  const open: FakeWorker[] = [];
  afterAll(() => {
    for (const w of open) w.close();
  });

  it("a run with worker_host set to the worker gets task.assign; the fake runner starts the session", async () => {
    const api = adminApi();
    const { worker, id } = await connectWorker(api);
    open.push(worker);
    const f = await createTestFactory(api);
    const run = await api.post<{ run_id: string }>(
      "/runs",
      { prompt: "contract: on a worker", factory_id: f.uid, agent_name: "foreman", config: { worker_host: id } },
      201,
    );
    const task = await worker.awaitTask(run.run_id, 1, 30_000);
    assertWorkerDef("task", task);
    expect(task.env.OF_RUN_ID).toBe(run.run_id);
    expect(task.env.OF_BOOTSTRAP_TOKEN).toMatch(/^ofb_/);
    const runner = new FakeRunner(task.env);
    const start = await runner.start({ sandbox_id: `fake-${task.task_id}` });
    expect(start.run_id).toBe(run.run_id);
    await api.post(`/runs/${run.run_id}/cancel`, {});
    // The hub cancels or destroys the task.
    await worker.waitFrame(
      (fr) => fr.type === "request" && (fr.method === "task.cancel" || fr.method === "task.destroy"),
      30_000,
    );
    expect(worker.invalid).toEqual([]);
  });
});

describeFeature([TAGS.workerEnrollment], "worker enrollment and lifecycle", () => {
  let api: Api;
  let e: Enrolled;
  let worker: FakeWorker | undefined;

  beforeAll(async () => {
    api = adminApi();
    e = await enroll(api, { labels: { pool: "contract" } });
  });
  afterAll(() => worker?.close());

  it("the worker is pending until it connects", async () => {
    const w = await api.get<Worker & { slots?: number }>(`/workers/${e.workerId}`);
    expect(w.name).toBe(e.name);
    expect(["pending", "offline"]).toContain(w.status);
  });

  it("an enrollment token works once", async () => {
    const enrollment = await api.post<{ token: string }>("/workers/enrollments", {}, 201);
    const body = {
      enrollment_token: enrollment.token,
      name: uniq("ct-once"),
      public_key: newWorkerKey().publicKeyB64,
      facts: { arch: "amd64", vcpus: 2, kvm: true },
      slots: 1,
      slot_size: { vcpus: 1, memory_mib: 512 },
    };
    await api.post("/workers/enroll", body, 201, { key: null });
    const res = await api.request("POST", "/workers/enroll", { body: { ...body, name: uniq("ct-twice") }, key: null });
    expect(res.status).toBeGreaterThanOrEqual(400);
    expect(res.status).toBeLessThan(500);
  });

  it("a non-admin cannot create enrollment tokens", async () => {
    const u = await api.post<{ key: string }>(
      "/users",
      { email: `${uniq("ed")}@example.com`, role: "editor", create_api_key: true },
      201,
    );
    await new Api(u.key).problem(403, "POST", "/workers/enrollments", { body: {}, code: "not_authorized" });
  });

  it("Ed25519 challenge, hello, welcome; online", async () => {
    worker = await FakeWorker.connect(
      { kind: "ed25519", workerId: e.workerId, privateKey: e.key.privateKey },
      { workerId: e.workerId, name: e.name, backend: "microvm", capacity: 2, slots: 2 },
    );
    const challenge = worker.received.find((f) => f.type === "auth.challenge");
    expect(challenge).toBeDefined();
    await waitFor("worker online", async () => (await api.get<Worker>(`/workers/${e.workerId}`)).status === "online");
  });

  it("a wrong signature is refused", async () => {
    await expect(
      FakeWorker.connect(
        { kind: "ed25519", workerId: e.workerId, privateKey: newWorkerKey().privateKey },
        { workerId: e.workerId, backend: "microvm", capacity: 1 },
        8000,
      ),
    ).rejects.toThrow();
  });

  it("PATCH updates labels and slots", async () => {
    const w = await api.patch<{ labels?: Record<string, string>; slots?: number }>(`/workers/${e.workerId}`, {
      labels: { pool: "contract", extra: "yes" },
      slots: 1,
    });
    expect(w.labels?.extra).toBe("yes");
    expect(w.slots).toBe(1);
  });

  it("drain, then resume", async () => {
    const drained = await api.post<Worker>(`/workers/${e.workerId}/drain`, {});
    expect(drained.status).toBe("draining");
    if (worker) await worker.waitFrame((f) => f.type === "request" && f.method === "drain", 10_000);
    const resumed = await api.post<Worker>(`/workers/${e.workerId}/resume`, {});
    expect(resumed.status).not.toBe("draining");
  });

  it("revoke closes the connection and refuses git credentials", async () => {
    const revoked = await api.post<Worker>(`/workers/${e.workerId}/revoke`, {});
    expect(revoked.status).toBe("revoked");
    if (worker) await worker.waitClosed(10_000);
    await expect(
      FakeWorker.connect(
        { kind: "ed25519", workerId: e.workerId, privateKey: e.key.privateKey },
        { workerId: e.workerId, backend: "microvm", capacity: 1 },
        8000,
      ),
    ).rejects.toThrow();
    const res = await api.request("GET", `/workers/${e.workerId}/git-credential`, {
      query: { repo: "https://github.com/example/repo.git" },
    });
    expect([401, 403]).toContain(res.status);
  });

  it("DELETE removes the worker", async () => {
    await api.del(`/workers/${e.workerId}`);
    await api.problem(404, "GET", `/workers/${e.workerId}`, { code: "resource_not_found" });
  });
});
