// Server-Sent Events: the team stream and the run stream.
import { afterEach, expect, it } from "vitest";
import { adminApi, adminContext, createTestFactory } from "../harness/context.ts";
import { assertComponent } from "../harness/schemas.ts";
import { SseStream } from "../harness/sse.ts";
import { describeFeature, TAGS } from "../harness/target.ts";

describeFeature([TAGS.sse], "SSE", () => {
  const open: SseStream[] = [];
  afterEach(() => {
    for (const s of open.splice(0)) s.close();
  });

  it("GET /stream sends ready, then run events of a new run", async () => {
    const s = await SseStream.open("/stream?topics=run,workitem", adminContext().key);
    open.push(s);
    expect(s.contentType).toMatch(/^text\/event-stream/);
    await s.waitEvent((e) => e.event === "ready");
    const f = await createTestFactory(adminApi());
    const run = await adminApi().post<{ run_id: string }>(
      "/runs",
      { prompt: "contract: sse", factory_id: f.uid, agent_name: "foreman" },
      201,
    );
    const ev = await s.waitEvent(
      (e) => e.event === "run" && (e.json?.run_id === run.run_id || e.json?.id === run.run_id),
    );
    assertComponent("Run", ev.json);
    await adminApi().post(`/runs/${run.run_id}/cancel`, {});
  });

  it("GET /stream without credentials is refused", async () => {
    await expect(SseStream.open("/stream", "")).rejects.toThrow(/401/);
  });

  it("GET /runs/{id}/events sends run, then done after cancel", async () => {
    const f = await createTestFactory(adminApi());
    const run = await adminApi().post<{ run_id: string }>(
      "/runs",
      { prompt: "contract: run sse", factory_id: f.uid, agent_name: "foreman" },
      201,
    );
    const s = await SseStream.open(`/runs/${run.run_id}/events`, adminContext().key);
    open.push(s);
    await s.waitEvent((e) => e.event === "run");
    await adminApi().post(`/runs/${run.run_id}/cancel`, {});
    await s.waitEvent((e) => e.event === "done", 30_000);
    await s.waitEnd(10_000);
  });

  it("the run stream of an unknown run is resource_not_found", async () => {
    await expect(SseStream.open("/runs/run_doesnotexist00000000/events", adminContext().key)).rejects.toThrow(/404/);
  });
});
