// Runs, work items and the inbox over REST (no runner involved).
import { beforeAll, expect, it } from "vitest";
import { waitFor } from "../harness/api.ts";
import { adminApi, createTestFactory, type CreatedFactory } from "../harness/context.ts";
import { describeFeature, TAGS } from "../harness/target.ts";

interface Run {
  run_id: string;
  id: string;
  state: string;
  phase?: string;
  factory_id: string;
  work_item_id?: string;
  agent_name?: string;
  metadata?: Record<string, string>;
}

describeFeature([TAGS.rest], "runs", () => {
  let f: CreatedFactory;
  beforeAll(async () => {
    f = await createTestFactory(adminApi());
  });

  it("creates a run by factory and agent name", async () => {
    const run = await adminApi().post<Run>(
      "/runs",
      { prompt: "contract: create", factory_id: f.uid, agent_name: "foreman", metadata: { suite: "contract" } },
      201,
    );
    expect(run.run_id).toMatch(/^run_/);
    expect(run.factory_id).toBe(f.uid);
    expect(run.agent_name).toBe("foreman");
    expect(["QUEUED", "PENDING", "CLAIMED", "RUNNING"]).toContain(run.state);
    expect(run.work_item_id).toBeTruthy();

    const got = await adminApi().get<Run>(`/runs/${run.run_id}`);
    expect(got.run_id).toBe(run.run_id);
    expect(got.metadata?.suite).toBe("contract");

    const list = await adminApi().get<{ items: Run[] }>("/runs", { query: { factory_id: f.uid } });
    expect(list.items.map((r) => r.run_id)).toContain(run.run_id);
    await adminApi().post(`/runs/${run.run_id}/cancel`, {});
  });

  it("creates a run by agent_identity_uid", async () => {
    const agents = await adminApi().get<{ items: { uid?: string; id?: string; name: string }[] }>(
      `/factories/${f.uid}/agents`,
    );
    const foreman = agents.items.find((a) => a.name === "foreman");
    const uid = foreman?.uid ?? foreman?.id;
    expect(uid).toBeTruthy();
    const run = await adminApi().post<Run>("/runs", { prompt: "contract: by uid", agent_identity_uid: uid }, 201);
    expect(run.agent_name).toBe("foreman");
    await adminApi().post(`/runs/${run.run_id}/cancel`, {});
  });

  it("an unknown agent is an error", async () => {
    const res = await adminApi().request("POST", "/runs", {
      body: { prompt: "x", factory_id: f.uid, agent_name: "no-such-agent" },
    });
    expect([400, 404]).toContain(res.status);
  });

  it("a missing prompt is invalid_request", async () => {
    await adminApi().problem(400, "POST", "/runs", {
      body: { factory_id: f.uid, agent_name: "foreman" },
      code: "invalid_request",
    });
  });

  it("cancel moves the run to CANCELLED", async () => {
    const run = await adminApi().post<Run>(
      "/runs",
      { prompt: "contract: cancel", factory_id: f.uid, agent_name: "foreman" },
      201,
    );
    await adminApi().post(`/runs/${run.run_id}/cancel`, {});
    const done = await waitFor("run cancelled", async () => {
      const r = await adminApi().get<Run>(`/runs/${run.run_id}`);
      return r.state === "CANCELLED" ? r : undefined;
    });
    expect(done.state).toBe("CANCELLED");
  });

  it("lists messages, timeline, inputs, sessions", async () => {
    const run = await adminApi().post<Run>(
      "/runs",
      { prompt: "contract: views", factory_id: f.uid, agent_name: "foreman" },
      201,
    );
    const msgs = await adminApi().get<{ messages: { role: string; content: string }[] }>(
      `/runs/${run.run_id}/messages`,
    );
    expect(Array.isArray(msgs.messages)).toBe(true);
    for (const p of ["timeline", "inputs", "sessions"]) await adminApi().get(`/runs/${run.run_id}/${p}`);
    await adminApi().post(`/runs/${run.run_id}/cancel`, {});
  });
});

describeFeature([TAGS.rest], "work items", () => {
  let f: CreatedFactory;
  beforeAll(async () => {
    f = await createTestFactory(adminApi());
  });

  it("a run creates a work item that the factory lists", async () => {
    const run = await adminApi().post<Run>(
      "/runs",
      { prompt: "contract: work item", factory_id: f.uid, agent_name: "foreman" },
      201,
    );
    const wi = await adminApi().get<{ id: string; factory_id: string; stage: string; root_run_id?: string }>(
      `/work-items/${run.work_item_id}`,
    );
    expect(wi.factory_id).toBe(f.uid);
    expect(wi.root_run_id).toBe(run.run_id);
    const list = await adminApi().get<{ items: { id: string }[] }>(`/factories/${f.uid}/work-items`);
    expect(list.items.map((w) => w.id)).toContain(wi.id);
    await adminApi().post(`/runs/${run.run_id}/cancel`, {});
  });

  it("stage, then complete", async () => {
    const run = await adminApi().post<Run>(
      "/runs",
      { prompt: "contract: stage", factory_id: f.uid, agent_name: "foreman" },
      201,
    );
    const id = run.work_item_id!;
    const staged = await adminApi().post<{ stage: string }>(`/work-items/${id}/stage`, {
      stage: "planning",
      note: "contract",
    });
    expect(staged.stage).toBe("planning");
    await adminApi().post(`/work-items/${id}/complete`, {});
    const wi = await adminApi().get<{ stage: string }>(`/work-items/${id}`);
    expect(wi.stage).toBe("complete");
  });

  it("an unknown stage is invalid_request", async () => {
    const run = await adminApi().post<Run>(
      "/runs",
      { prompt: "contract: bad stage", factory_id: f.uid, agent_name: "foreman" },
      201,
    );
    await adminApi().problem(400, "POST", `/work-items/${run.work_item_id}/stage`, {
      body: { stage: "no-such-stage" },
      code: "invalid_request",
    });
    await adminApi().post(`/runs/${run.run_id}/cancel`, {});
  });

  it("cancel cancels the work item and its runs", async () => {
    const run = await adminApi().post<Run>(
      "/runs",
      { prompt: "contract: wi cancel", factory_id: f.uid, agent_name: "foreman" },
      201,
    );
    await adminApi().post(`/work-items/${run.work_item_id}/cancel`, {});
    const wi = await adminApi().get<{ stage: string }>(`/work-items/${run.work_item_id}`);
    expect(wi.stage).toBe("cancelled");
    await waitFor("run cancelled with its work item", async () => {
      const r = await adminApi().get<Run>(`/runs/${run.run_id}`);
      return r.state === "CANCELLED";
    });
  });
});

describeFeature([TAGS.rest], "inbox", () => {
  it("lists the team inbox and a factory inbox", async () => {
    const f = await createTestFactory(adminApi());
    const mine = await adminApi().get<{ items: unknown[] }>("/inbox");
    expect(Array.isArray(mine.items)).toBe(true);
    await adminApi().get("/inbox", { query: { scope: "team", include_resolved: true } });
    await adminApi().get(`/factories/${f.uid}/inbox`);
  });

  it("an unknown item is resource_not_found", async () => {
    await adminApi().problem(404, "GET", "/inbox/inb_doesnotexist000000", { code: "resource_not_found" });
  });
});
